// Package operatorcontroller contains the Kubernetes adapter for AutoSQL's
// reconciliation core. The core stays Kubernetes-independent; this package
// owns watches, status updates, and namespaced object identity.
package operatorcontroller

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"autosql/pkg/operator"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var GroupVersionKind = schema.GroupVersionKind{Group: "autosql.io", Version: "v1alpha1", Kind: "AutoSQLSchema"}

// Controller adapts AutoSQL's deterministic reconciler to an unstructured CRD.
// Apply is injected so production deployments can use the signed-artifact
// guardrail while tests can use a fake PostgreSQL executor.
type Controller struct {
	client.Client
	Reader client.Reader
	Store  operator.Store
	Leader operator.Leader
	Apply  operator.ApplyFunc
	Now    func() time.Time
}

// Run starts the production controller-runtime manager. ArtifactApply keeps
// the actual mutation behind AutoSQL's signed-artifact production boundary.
func Run(ctx context.Context, leaderElection bool) error {
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:           NewScheme(),
		LeaderElection:   leaderElection,
		LeaderElectionID: "autosql-operator.autosql.io",
	})
	if err != nil {
		return err
	}
	statePath := os.Getenv("AUTOSQL_OPERATOR_STATE_FILE")
	if statePath == "" {
		statePath = "/var/lib/autosql/operator/state.json"
	}
	store, err := operator.NewFileStore(statePath)
	if err != nil {
		return err
	}
	c := &Controller{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Store: store, Apply: ArtifactApply}
	if err := c.SetupWithManager(mgr); err != nil {
		return err
	}
	return mgr.Start(ctx)
}

func NewScheme() *runtime.Scheme {
	scheme := runtime.NewScheme()
	_ = corev1.AddToScheme(scheme)
	scheme.AddKnownTypeWithName(GroupVersionKind, &unstructured.Unstructured{})
	scheme.AddKnownTypeWithName(GroupVersionKind.GroupVersion().WithKind("AutoSQLSchemaList"), &unstructured.UnstructuredList{})
	return scheme
}

func (c *Controller) SetupWithManager(mgr manager.Manager) error {
	if c.Store == nil || c.Apply == nil {
		return fmt.Errorf("operator controller requires store and apply function")
	}
	forObject := &unstructured.Unstructured{}
	forObject.SetGroupVersionKind(GroupVersionKind)
	return builder.ControllerManagedBy(mgr).
		For(forObject).
		Complete(c)
}

func (c *Controller) Reconcile(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(GroupVersionKind)
	if err := c.Get(ctx, types.NamespacedName{Namespace: req.Namespace, Name: req.Name}, obj); err != nil {
		return reconcile.Result{}, client.IgnoreNotFound(err)
	}

	resource, approved, err := resourceFromObject(obj)
	if err != nil {
		return reconcile.Result{}, c.writeFailure(ctx, obj, err)
	}
	if err := c.resolveReferences(ctx, obj.GetNamespace(), &resource); err != nil {
		return reconcile.Result{}, c.writeFailure(ctx, obj, err)
	}
	r := operator.Reconciler{Store: c.Store, Leader: c.Leader, Apply: c.Apply, Now: c.Now}
	status, err := r.Reconcile(ctx, resource, approved)
	if updateErr := writeStatus(ctx, c.Client, obj, status); updateErr != nil {
		return reconcile.Result{}, updateErr
	}
	if err != nil {
		return reconcile.Result{}, err
	}
	if len(status.Conditions) > 0 && status.Conditions[len(status.Conditions)-1].Type == operator.Approval {
		return reconcile.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if status.RetryCount > 0 && status.AppliedDigest == "" {
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}
	return reconcile.Result{}, nil
}

// resolveReferences verifies that every Kubernetes-backed source and database
// reference exists before planning or applying. Values never enter status or
// error strings; the production executor resolves the configured URL through
// its own runtime secret boundary.
func (c *Controller) resolveReferences(ctx context.Context, namespace string, resource *operator.Resource) error {
	if resource == nil {
		return fmt.Errorf("resource is required")
	}
	if resource.Spec.DatabaseURL != nil {
		secret := &corev1.Secret{}
		if err := c.read(ctx, types.NamespacedName{Namespace: namespace, Name: resource.Spec.DatabaseURL.Name}, secret); err != nil {
			return fmt.Errorf("database Secret reference unavailable")
		}
		value := secret.Data[resource.Spec.DatabaseURL.Key]
		if len(value) == 0 {
			return fmt.Errorf("database Secret key unavailable")
		}
		resource.ResolvedDatabaseURL = string(value)
	}
	if ref := resource.Spec.Source.SecretRef; ref != nil {
		secret := &corev1.Secret{}
		if err := c.read(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, secret); err != nil {
			return fmt.Errorf("source Secret reference unavailable")
		}
		value := secret.Data[ref.Key]
		if len(value) == 0 {
			return fmt.Errorf("source Secret key unavailable")
		}
		resource.ResolvedSource = string(value)
	}
	if ref := resource.Spec.Source.ConfigMapRef; ref != nil {
		config := &corev1.ConfigMap{}
		if err := c.read(ctx, types.NamespacedName{Namespace: namespace, Name: ref.Name}, config); err != nil {
			return fmt.Errorf("source ConfigMap reference unavailable")
		}
		value := config.Data[ref.Key]
		if strings.TrimSpace(value) == "" {
			return fmt.Errorf("source ConfigMap key unavailable")
		}
		resource.ResolvedSource = value
	}
	if resource.ResolvedSource == "" {
		switch {
		case resource.Spec.Source.Inline != "":
			resource.ResolvedSource = resource.Spec.Source.Inline
		case resource.Spec.Source.URL != "":
			resource.ResolvedSource = resource.Spec.Source.URL
		case resource.Spec.Source.RegistryDigest != "":
			resource.ResolvedSource = resource.Spec.Source.RegistryDigest
		}
	}
	return nil
}

func (c *Controller) read(ctx context.Context, key types.NamespacedName, obj client.Object) error {
	if c.Reader != nil {
		return c.Reader.Get(ctx, key, obj)
	}
	return c.Get(ctx, key, obj)
}

func resourceFromObject(obj *unstructured.Unstructured) (operator.Resource, bool, error) {
	spec, ok, err := unstructured.NestedMap(obj.Object, "spec")
	if err != nil || !ok {
		return operator.Resource{}, false, fmt.Errorf("spec is required")
	}
	kind, _, _ := unstructured.NestedString(spec, "kind")
	generation, _, _ := unstructured.NestedInt64(spec, "generation")
	artifactDigest, _, _ := unstructured.NestedString(spec, "artifactDigest")
	requireApproval, _, _ := unstructured.NestedBool(spec, "requireApproval")
	sourceMap, _, _ := unstructured.NestedMap(spec, "source")
	source, err := sourceFromMap(sourceMap)
	if err != nil {
		return operator.Resource{}, false, err
	}
	database, _, _ := unstructured.NestedMap(spec, "databaseURL")
	databaseURL := &operator.SecretKeyRef{}
	databaseURL.Name, _, _ = unstructured.NestedString(database, "name")
	databaseURL.Key, _, _ = unstructured.NestedString(database, "key")
	approved := strings.EqualFold(obj.GetAnnotations()["autosql.io/approved"], "true")
	return operator.Resource{Name: obj.GetNamespace() + "/" + obj.GetName(), Spec: operator.Spec{
		Kind: operator.ResourceKind(kind), Generation: generation, Source: source,
		ArtifactDigest: artifactDigest, DatabaseURL: databaseURL, RequireApproval: requireApproval,
	}, Status: statusFromObject(obj)}, approved, nil
}

func sourceFromMap(m map[string]any) (operator.Source, error) {
	s := operator.Source{}
	if value, ok := m["inline"].(string); ok {
		s.Inline = value
	}
	if value, ok := m["url"].(string); ok {
		s.URL = value
	}
	if value, ok := m["registryDigest"].(string); ok {
		s.RegistryDigest = value
	}
	if value, ok := m["secretRef"].(map[string]any); ok {
		s.SecretRef = &operator.SecretKeyRef{}
		s.SecretRef.Name, _, _ = unstructured.NestedString(value, "name")
		s.SecretRef.Key, _, _ = unstructured.NestedString(value, "key")
	}
	if value, ok := m["configMapRef"].(map[string]any); ok {
		s.ConfigMapRef = &operator.ConfigMapKeyRef{}
		s.ConfigMapRef.Name, _, _ = unstructured.NestedString(value, "name")
		s.ConfigMapRef.Key, _, _ = unstructured.NestedString(value, "key")
	}
	return s, nil
}

func statusFromObject(obj *unstructured.Unstructured) operator.Status {
	status, _, _ := unstructured.NestedMap(obj.Object, "status")
	observed, _, _ := unstructured.NestedInt64(status, "observedGeneration")
	retry, _, _ := unstructured.NestedInt64(status, "retryCount")
	applied, _, _ := unstructured.NestedString(status, "appliedDigest")
	planDigest, _, _ := unstructured.NestedString(status, "planDigest")
	target, _, _ := unstructured.NestedString(status, "targetIdentity")
	operation, _, _ := unstructured.NestedString(status, "operationID")
	recovery, _, _ := unstructured.NestedString(status, "recoveryState")
	execution, _, _ := unstructured.NestedString(status, "executionID")
	pending, _, _ := unstructured.NestedString(status, "pendingStep")
	guidance, _, _ := unstructured.NestedString(status, "recoveryGuidance")
	steps, _, _ := unstructured.NestedInt64(status, "appliedSteps")
	return operator.Status{ObservedGeneration: observed, RetryCount: int(retry), AppliedDigest: applied, PlanDigest: planDigest, TargetIdentity: target, OperationID: operation, RecoveryState: recovery, ExecutionID: execution, PendingStep: pending, RecoveryGuidance: guidance, AppliedSteps: int(steps)}
}

func writeStatus(ctx context.Context, cl client.Client, obj *unstructured.Unstructured, status operator.Status) error {
	latest := map[string]any{}
	for _, condition := range status.Conditions {
		latest[string(condition.Type)] = map[string]any{"type": string(condition.Type), "status": condition.Status, "reason": condition.Reason, "message": condition.Message, "observedGeneration": condition.ObservedGeneration, "lastTransitionTime": condition.LastTransitionTime.UTC().Format(time.RFC3339Nano)}
	}
	types := make([]string, 0, len(latest))
	for typ := range latest {
		types = append(types, typ)
	}
	sort.Strings(types)
	conditions := make([]any, 0, len(types))
	for _, typ := range types {
		conditions = append(conditions, latest[typ])
	}
	value := map[string]any{"observedGeneration": status.ObservedGeneration, "retryCount": int64(status.RetryCount), "conditions": conditions}
	if status.AppliedDigest != "" {
		value["appliedDigest"] = status.AppliedDigest
	}
	if status.PlanDigest != "" {
		value["planDigest"] = status.PlanDigest
	}
	if status.TargetIdentity != "" {
		value["targetIdentity"] = status.TargetIdentity
	}
	if status.OperationID != "" {
		value["operationID"] = status.OperationID
	}
	if status.RecoveryState != "" {
		value["recoveryState"] = status.RecoveryState
	}
	if status.ExecutionID != "" {
		value["executionID"] = status.ExecutionID
	}
	if status.PendingStep != "" {
		value["pendingStep"] = status.PendingStep
	}
	if status.RecoveryGuidance != "" {
		value["recoveryGuidance"] = status.RecoveryGuidance
	}
	if status.AppliedSteps > 0 {
		value["appliedSteps"] = int64(status.AppliedSteps)
	}
	obj.Object["status"] = value
	return cl.Status().Update(ctx, obj)
}

func (c *Controller) writeFailure(ctx context.Context, obj *unstructured.Unstructured, err error) error {
	status := map[string]any{"conditions": []any{map[string]any{"type": string(operator.Failed), "status": "True", "reason": "Invalid", "message": err.Error(), "observedGeneration": int64(0), "lastTransitionTime": time.Now().UTC().Format(time.RFC3339Nano)}}}
	obj.Object["status"] = status
	if updateErr := c.Status().Update(ctx, obj); updateErr != nil {
		return updateErr
	}
	return err
}

// ApprovedAnnotation documents the admission-to-controller approval bridge.
func ApprovedAnnotation(value bool) string { return strconv.FormatBool(value) }
