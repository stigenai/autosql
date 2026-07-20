// Package operatorcontroller contains the Kubernetes adapter for AutoSQL's
// reconciliation core. The core stays Kubernetes-independent; this package
// owns watches, status updates, and namespaced object identity.
package operatorcontroller

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/bootstrap"
	"autosql/pkg/operator"
	"autosql/pkg/workloadidentity"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/builder"
	controllercache "sigs.k8s.io/controller-runtime/pkg/cache"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/handler"
	"sigs.k8s.io/controller-runtime/pkg/manager"
	"sigs.k8s.io/controller-runtime/pkg/predicate"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

var GroupVersionKind = schema.GroupVersionKind{Group: "autosql.io", Version: "v1alpha1", Kind: "AutoSQLSchema"}

var resolveWorkloadIdentityURL = func(ctx context.Context, binding workloadidentity.Binding) (string, time.Time, error) {
	var source *workloadidentity.Source
	var err error
	switch binding.Provider {
	case workloadidentity.AWSRDS:
		source, err = workloadidentity.NewAWS(ctx, binding)
	case workloadidentity.GCPCloud:
		source, err = workloadidentity.NewGCP(ctx, binding)
	case workloadidentity.AzurePG:
		source, err = workloadidentity.NewAzure(binding)
	default:
		err = errors.New("unsupported workload identity provider")
	}
	if err != nil {
		return "", time.Time{}, err
	}
	return source.ConnectionURL(ctx)
}

// Controller adapts AutoSQL's deterministic reconciler to an unstructured CRD.
// Apply is injected so production deployments can use the signed-artifact
// guardrail while tests can use a fake PostgreSQL executor.
type Controller struct {
	client.Client
	Reader client.Reader
	Store  operator.Store
	Leader operator.Leader
	Apply  operator.ApplyFunc
	// VerifyRelease is a test seam. Production leaves it nil and therefore
	// always uses the mandatory generated-origin verifier.
	VerifyRelease func(string) (artifact.VerifiedArtifact, error)
	Now           func() time.Time
	Recorder      record.EventRecorder
}

// Run starts the production controller-runtime manager. ArtifactApply keeps
// the actual mutation behind AutoSQL's signed-artifact production boundary.
func Run(ctx context.Context, leaderElection bool) error {
	options := ctrl.Options{
		Scheme:           NewScheme(),
		LeaderElection:   leaderElection,
		LeaderElectionID: "autosql-operator.autosql.io",
	}
	if namespace := strings.TrimSpace(os.Getenv("AUTOSQL_OPERATOR_WATCH_NAMESPACE")); namespace != "" {
		options.Cache.DefaultNamespaces = map[string]controllercache.Config{namespace: {}}
		options.LeaderElectionNamespace = namespace
	}
	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), options)
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
	c := &Controller{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Store: store, Apply: ArtifactApply, Recorder: mgr.GetEventRecorderFor("autosql-operator")}
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
		For(forObject, builder.WithPredicates(predicate.Or(predicate.GenerationChangedPredicate{}, predicate.AnnotationChangedPredicate{}))).
		Watches(&corev1.Secret{}, handler.EnqueueRequestsFromMapFunc(c.requestsForSecret)).
		Watches(&corev1.ConfigMap{}, handler.EnqueueRequestsFromMapFunc(c.requestsForConfigMap)).
		Complete(c)
}

func (c *Controller) requestsForSecret(ctx context.Context, object client.Object) []reconcile.Request {
	secret, ok := object.(*corev1.Secret)
	if !ok {
		return nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(GroupVersionKind.GroupVersion().WithKind("AutoSQLSchemaList"))
	if err := c.List(ctx, list, client.InNamespace(secret.Namespace)); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for index := range list.Items {
		resource, _, err := resourceFromObject(&list.Items[index])
		if err != nil {
			continue
		}
		refs := []*operator.SecretKeyRef{resource.Spec.DatabaseURL, resource.Spec.MaintenanceDatabaseURL, resource.Spec.Source.SecretRef}
		if resource.Spec.BootstrapAuthorization != nil {
			refs = append(refs, &resource.Spec.BootstrapAuthorization.ManifestSecretRef, &resource.Spec.BootstrapAuthorization.PublicKeySecretRef)
		}
		for _, ref := range refs {
			if ref != nil && ref.Name == secret.Name {
				requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: secret.Namespace, Name: list.Items[index].GetName()}})
				break
			}
		}
	}
	return requests
}

func (c *Controller) requestsForConfigMap(ctx context.Context, object client.Object) []reconcile.Request {
	configMap, ok := object.(*corev1.ConfigMap)
	if !ok {
		return nil
	}
	list := &unstructured.UnstructuredList{}
	list.SetGroupVersionKind(GroupVersionKind.GroupVersion().WithKind("AutoSQLSchemaList"))
	if err := c.List(ctx, list, client.InNamespace(configMap.Namespace)); err != nil {
		return nil
	}
	requests := make([]reconcile.Request, 0)
	for index := range list.Items {
		resource, _, err := resourceFromObject(&list.Items[index])
		if err != nil {
			continue
		}
		ref := resource.Spec.Source.ConfigMapRef
		if ref != nil && ref.Name == configMap.Name {
			requests = append(requests, reconcile.Request{NamespacedName: types.NamespacedName{Namespace: configMap.Namespace, Name: list.Items[index].GetName()}})
		}
	}
	return requests
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
	r := operator.Reconciler{Store: c.Store, Leader: c.Leader, Apply: c.Apply, Now: c.Now}
	// Suspension must not resolve Secrets, mint workload credentials, or verify
	// remote artifacts. It is a control-plane pause with a status-only result.
	if resource.Spec.Suspend {
		status, reconcileErr := r.Reconcile(ctx, resource, approved)
		if updateErr := writeStatus(ctx, c.Client, obj, status); updateErr != nil {
			return reconcile.Result{}, updateErr
		}
		return reconcile.Result{}, reconcileErr
	}
	// Authenticate the immutable release before resolving any Kubernetes
	// runtime reference. The opaque verified token is carried into Apply, so
	// neither a second file read nor an artifact swap can occur after Secrets
	// become visible to reconciliation.
	if resource.Spec.Kind == operator.Declarative && (resource.Spec.DatabaseTarget != nil || resource.Spec.AdoptionPolicy == operator.AdoptIfEquivalent) {
		verifyRelease := verifyOperatorReleaseBeforeReferences
		if c.VerifyRelease != nil {
			verifyRelease = c.VerifyRelease
		}
		verified, verifyErr := verifyRelease(resource.Spec.ArtifactDigest)
		if verifyErr != nil {
			return reconcile.Result{}, c.writeFailure(ctx, obj, verifyErr)
		}
		resource.VerifiedReleaseArtifact = verified
	}
	if err := c.resolveReferences(ctx, obj.GetNamespace(), &resource); err != nil {
		return reconcile.Result{}, c.writeFailure(ctx, obj, err)
	}
	status, err := r.Reconcile(ctx, resource, approved)
	if updateErr := writeStatus(ctx, c.Client, obj, status); updateErr != nil {
		return reconcile.Result{}, updateErr
	}
	c.recordAuthorizationEvent(obj, status)
	if err != nil {
		return reconcile.Result{}, err
	}
	if len(status.Conditions) > 0 && status.Conditions[len(status.Conditions)-1].Type == operator.Approval {
		return reconcile.Result{RequeueAfter: 2 * time.Second}, nil
	}
	if status.RetryCount > 0 && status.AppliedDigest == "" {
		return reconcile.Result{RequeueAfter: time.Second}, nil
	}
	if !status.AuthorizationExpiresAt.IsZero() {
		now := time.Now().UTC()
		if c.Now != nil {
			now = c.Now().UTC()
		}
		remaining := status.AuthorizationExpiresAt.Sub(now) - 30*time.Second
		if remaining < time.Second {
			remaining = time.Second
		}
		return reconcile.Result{RequeueAfter: remaining}, nil
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
		resource.SourceBinding = referenceBinding(secret.ResourceVersion, value)
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
		resource.SourceBinding = referenceBinding(config.ResourceVersion, []byte(value))
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
	if resource.Spec.Kind == operator.Declarative && (resource.Spec.Source.Inline != "" || resource.Spec.Source.SecretRef != nil || resource.Spec.Source.ConfigMapRef != nil) {
		if _, err := loadOperatorDeclarativeSource(ctx, resource.ResolvedSource, resource.Spec.Source.Format); err != nil {
			return err
		}
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
		resource.DatabaseBinding = referenceBinding(secret.ResourceVersion, nil)
	}
	if resource.Spec.DatabaseIdentity != nil {
		url, _, err := resolveWorkloadIdentityURL(ctx, *resource.Spec.DatabaseIdentity)
		if err != nil {
			return fmt.Errorf("database workload identity unavailable")
		}
		resource.ResolvedDatabaseURL = url
		digest, err := resource.Spec.DatabaseIdentity.Digest()
		if err != nil {
			return fmt.Errorf("database workload identity invalid")
		}
		resource.DatabaseBinding = operator.RuntimeReferenceBinding{ResourceVersion: digest, ContentDigest: digest}
	}
	if resource.Spec.MaintenanceDatabaseURL != nil {
		secret := &corev1.Secret{}
		if err := c.read(ctx, types.NamespacedName{Namespace: namespace, Name: resource.Spec.MaintenanceDatabaseURL.Name}, secret); err != nil {
			return fmt.Errorf("maintenance database Secret reference unavailable")
		}
		value := secret.Data[resource.Spec.MaintenanceDatabaseURL.Key]
		if len(value) == 0 {
			return fmt.Errorf("maintenance database Secret key unavailable")
		}
		resource.ResolvedMaintenanceDatabaseURL = string(value)
		resource.MaintenanceDatabaseBinding = referenceBinding(secret.ResourceVersion, nil)
	}
	if ref := resource.Spec.BootstrapAuthorization; ref != nil {
		manifest := &corev1.Secret{}
		if err := c.read(ctx, types.NamespacedName{Namespace: namespace, Name: ref.ManifestSecretRef.Name}, manifest); err != nil {
			return &operator.AuthorizationError{State: operator.AuthorizationMissing}
		}
		manifestValue := manifest.Data[ref.ManifestSecretRef.Key]
		if len(manifestValue) == 0 {
			return &operator.AuthorizationError{State: operator.AuthorizationMissing}
		}
		publicKey := &corev1.Secret{}
		if err := c.read(ctx, types.NamespacedName{Namespace: namespace, Name: ref.PublicKeySecretRef.Name}, publicKey); err != nil {
			return &operator.AuthorizationError{State: operator.AuthorizationMissing}
		}
		publicKeyValue := publicKey.Data[ref.PublicKeySecretRef.Key]
		if len(publicKeyValue) == 0 {
			return &operator.AuthorizationError{State: operator.AuthorizationMissing}
		}
		resource.ResolvedAuthorizationManifest = append([]byte(nil), manifestValue...)
		resource.ResolvedAuthorizationPublicKey = append([]byte(nil), publicKeyValue...)
		resource.AuthorizationManifestBinding = referenceBinding(manifest.ResourceVersion, manifestValue)
		resource.AuthorizationPublicKeyBinding = referenceBinding(publicKey.ResourceVersion, publicKeyValue)
	}
	return nil
}

func referenceBinding(resourceVersion string, content []byte) operator.RuntimeReferenceBinding {
	binding := operator.RuntimeReferenceBinding{ResourceVersion: resourceVersion}
	if content != nil {
		digest := sha256.Sum256(content)
		binding.ContentDigest = fmt.Sprintf("sha256:%x", digest[:])
	}
	return binding
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
	suspend, _, _ := unstructured.NestedBool(spec, "suspend")
	createDatabase, _, _ := unstructured.NestedBool(spec, "createDatabase")
	postgresVersion, _, _ := unstructured.NestedInt64(spec, "postgresVersion")
	concurrentIndexes, _, _ := unstructured.NestedBool(spec, "concurrentIndexes")
	adoptionPolicy, _, adoptionPolicyErr := unstructured.NestedString(spec, "adoptionPolicy")
	if adoptionPolicyErr != nil {
		return operator.Resource{}, false, fmt.Errorf("adoptionPolicy must be a string")
	}
	sourceMap, _, _ := unstructured.NestedMap(spec, "source")
	source, err := sourceFromMap(sourceMap)
	if err != nil {
		return operator.Resource{}, false, err
	}
	database, _, _ := unstructured.NestedMap(spec, "databaseURL")
	databaseURL := &operator.SecretKeyRef{}
	databaseURL.Name, _, _ = unstructured.NestedString(database, "name")
	databaseURL.Key, _, _ = unstructured.NestedString(database, "key")
	if databaseURL.Name == "" && databaseURL.Key == "" {
		databaseURL = nil
	}
	var databaseIdentity *workloadidentity.Binding
	if value, found, nestedErr := unstructured.NestedMap(spec, "databaseIdentity"); nestedErr != nil {
		return operator.Resource{}, false, fmt.Errorf("databaseIdentity is invalid")
	} else if found {
		raw, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			return operator.Resource{}, false, fmt.Errorf("databaseIdentity is invalid")
		}
		databaseIdentity = &workloadidentity.Binding{}
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if decodeErr := decoder.Decode(databaseIdentity); decodeErr != nil {
			return operator.Resource{}, false, fmt.Errorf("databaseIdentity is invalid")
		}
	}
	maintenanceDatabase, _, _ := unstructured.NestedMap(spec, "maintenanceDatabaseURL")
	maintenanceDatabaseURL := &operator.SecretKeyRef{}
	maintenanceDatabaseURL.Name, _, _ = unstructured.NestedString(maintenanceDatabase, "name")
	maintenanceDatabaseURL.Key, _, _ = unstructured.NestedString(maintenanceDatabase, "key")
	if maintenanceDatabaseURL.Name == "" && maintenanceDatabaseURL.Key == "" {
		maintenanceDatabaseURL = nil
	}
	var databaseTarget *bootstrap.DatabaseTarget
	if value, found, nestedErr := unstructured.NestedMap(spec, "databaseTarget"); nestedErr != nil {
		return operator.Resource{}, false, fmt.Errorf("databaseTarget is invalid")
	} else if found {
		raw, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			return operator.Resource{}, false, fmt.Errorf("databaseTarget is invalid")
		}
		databaseTarget = &bootstrap.DatabaseTarget{}
		if unmarshalErr := json.Unmarshal(raw, databaseTarget); unmarshalErr != nil {
			return operator.Resource{}, false, fmt.Errorf("databaseTarget is invalid")
		}
	}
	var authority *bootstrap.Contract
	if value, found, nestedErr := unstructured.NestedMap(spec, "bootstrapAuthority"); nestedErr != nil {
		return operator.Resource{}, false, fmt.Errorf("bootstrapAuthority is invalid")
	} else if found {
		raw, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			return operator.Resource{}, false, fmt.Errorf("bootstrapAuthority is invalid")
		}
		authority = &bootstrap.Contract{}
		if unmarshalErr := json.Unmarshal(raw, authority); unmarshalErr != nil {
			return operator.Resource{}, false, fmt.Errorf("bootstrapAuthority is invalid")
		}
	}
	var authorization *operator.BootstrapAuthorizationRef
	if value, found, nestedErr := unstructured.NestedMap(spec, "bootstrapAuthorization"); nestedErr != nil {
		return operator.Resource{}, false, &operator.AuthorizationError{State: operator.AuthorizationInvalid}
	} else if found {
		raw, marshalErr := json.Marshal(value)
		if marshalErr != nil {
			return operator.Resource{}, false, &operator.AuthorizationError{State: operator.AuthorizationInvalid}
		}
		authorization = &operator.BootstrapAuthorizationRef{}
		decoder := json.NewDecoder(strings.NewReader(string(raw)))
		decoder.DisallowUnknownFields()
		if decodeErr := decoder.Decode(authorization); decodeErr != nil {
			return operator.Resource{}, false, &operator.AuthorizationError{State: operator.AuthorizationInvalid}
		}
	}
	approved := strings.EqualFold(obj.GetAnnotations()["autosql.io/approved"], "true")
	return operator.Resource{Name: obj.GetNamespace() + "/" + obj.GetName(), MetadataGeneration: obj.GetGeneration(), Spec: operator.Spec{
		Kind: operator.ResourceKind(kind), Generation: generation, Suspend: suspend, Source: source,
		ArtifactDigest: artifactDigest, DatabaseURL: databaseURL, DatabaseIdentity: databaseIdentity, MaintenanceDatabaseURL: maintenanceDatabaseURL, DatabaseTarget: databaseTarget, BootstrapAuthority: authority, BootstrapAuthorization: authorization, CreateDatabase: createDatabase, AdoptionPolicy: operator.AdoptionPolicy(adoptionPolicy), PostgresVersion: int(postgresVersion), ConcurrentIndexes: concurrentIndexes, RequireApproval: requireApproval,
	}, Status: statusFromObject(obj)}, approved, nil
}

func sourceFromMap(m map[string]any) (operator.Source, error) {
	s := operator.Source{}
	allowed := map[string]bool{"format": true, "inline": true, "url": true, "registryDigest": true, "secretRef": true, "configMapRef": true}
	for key := range m {
		if !allowed[key] {
			return operator.Source{}, fmt.Errorf("source contains unknown field %q", key)
		}
	}
	for field, destination := range map[string]*string{"format": &s.Format, "inline": &s.Inline, "url": &s.URL, "registryDigest": &s.RegistryDigest} {
		if raw, present := m[field]; present {
			value, ok := raw.(string)
			if !ok {
				return operator.Source{}, fmt.Errorf("source %s must be a string", field)
			}
			*destination = value
		}
	}
	if raw, present := m["secretRef"]; present {
		value, ok := raw.(map[string]any)
		if !ok {
			return operator.Source{}, fmt.Errorf("source secretRef must be an object")
		}
		name, key, err := validateSourceReferenceFields(value, "source secretRef")
		if err != nil {
			return operator.Source{}, err
		}
		s.SecretRef = &operator.SecretKeyRef{Name: name, Key: key}
	}
	if raw, present := m["configMapRef"]; present {
		value, ok := raw.(map[string]any)
		if !ok {
			return operator.Source{}, fmt.Errorf("source configMapRef must be an object")
		}
		name, key, err := validateSourceReferenceFields(value, "source configMapRef")
		if err != nil {
			return operator.Source{}, err
		}
		s.ConfigMapRef = &operator.ConfigMapKeyRef{Name: name, Key: key}
	}
	variants := 0
	if s.Inline != "" {
		variants++
	}
	if s.URL != "" {
		variants++
	}
	if s.RegistryDigest != "" {
		variants++
	}
	if s.SecretRef != nil {
		variants++
	}
	if s.ConfigMapRef != nil {
		variants++
	}
	if variants != 1 {
		return operator.Source{}, fmt.Errorf("exactly one source variant must be set")
	}
	if s.Format != "" && s.Format != "sql" && s.Format != "hcl" {
		return operator.Source{}, fmt.Errorf("source format must be sql or hcl")
	}
	if s.Format != "" && s.Inline == "" && s.SecretRef == nil && s.ConfigMapRef == nil {
		return operator.Source{}, fmt.Errorf("source format applies only to inline, Secret, or ConfigMap content")
	}
	if s.SecretRef != nil && (s.SecretRef.Name == "" || s.SecretRef.Key == "") {
		return operator.Source{}, fmt.Errorf("source secretRef requires name and key")
	}
	if s.ConfigMapRef != nil && (s.ConfigMapRef.Name == "" || s.ConfigMapRef.Key == "") {
		return operator.Source{}, fmt.Errorf("source configMapRef requires name and key")
	}
	return s, nil
}

func validateSourceReferenceFields(value map[string]any, label string) (string, string, error) {
	for key := range value {
		if key != "name" && key != "key" {
			return "", "", fmt.Errorf("%s contains unknown field %q", label, key)
		}
	}
	nameRaw, namePresent := value["name"]
	keyRaw, keyPresent := value["key"]
	name, nameOK := nameRaw.(string)
	key, keyOK := keyRaw.(string)
	if !namePresent || !nameOK {
		return "", "", fmt.Errorf("%s name must be a string", label)
	}
	if !keyPresent || !keyOK {
		return "", "", fmt.Errorf("%s key must be a string", label)
	}
	return name, key, nil
}

func statusFromObject(obj *unstructured.Unstructured) operator.Status {
	status, _, _ := unstructured.NestedMap(obj.Object, "status")
	conditionValues, _, _ := unstructured.NestedSlice(status, "conditions")
	conditions := make([]operator.Condition, 0, len(conditionValues))
	for _, value := range conditionValues {
		conditionMap, ok := value.(map[string]any)
		if !ok {
			continue
		}
		typ, _, _ := unstructured.NestedString(conditionMap, "type")
		conditionStatus, _, _ := unstructured.NestedString(conditionMap, "status")
		reason, _, _ := unstructured.NestedString(conditionMap, "reason")
		message, _, _ := unstructured.NestedString(conditionMap, "message")
		generation, _, _ := unstructured.NestedInt64(conditionMap, "observedGeneration")
		transition, _, _ := unstructured.NestedString(conditionMap, "lastTransitionTime")
		at, _ := time.Parse(time.RFC3339Nano, transition)
		conditions = append(conditions, operator.Condition{Type: operator.ConditionType(typ), Status: conditionStatus, Reason: reason, Message: message, ObservedGeneration: generation, LastTransitionTime: at})
	}
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
	appliedFingerprint, _, _ := unstructured.NestedString(status, "appliedFingerprint")
	expiresRaw, _, _ := unstructured.NestedString(status, "authorizationExpiresAt")
	expiresAt, _ := time.Parse(time.RFC3339Nano, expiresRaw)
	return operator.Status{Conditions: conditions, ObservedGeneration: observed, RetryCount: int(retry), AppliedDigest: applied, PlanDigest: planDigest, TargetIdentity: target, OperationID: operation, RecoveryState: recovery, ExecutionID: execution, PendingStep: pending, RecoveryGuidance: guidance, AppliedSteps: int(steps), AppliedFingerprint: appliedFingerprint, AuthorizationExpiresAt: expiresAt}
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
	if status.AppliedFingerprint != "" {
		value["appliedFingerprint"] = status.AppliedFingerprint
	}
	if !status.AuthorizationExpiresAt.IsZero() {
		value["authorizationExpiresAt"] = status.AuthorizationExpiresAt.UTC().Format(time.RFC3339Nano)
	}
	obj.Object["status"] = value
	return cl.Status().Update(ctx, obj)
}

func (c *Controller) writeFailure(ctx context.Context, obj *unstructured.Unstructured, err error) error {
	typ, reason, message := operator.Failed, "Invalid", err.Error()
	generation, _, _ := unstructured.NestedInt64(obj.Object, "spec", "generation")
	var authorizationErr *operator.AuthorizationError
	if errors.As(err, &authorizationErr) {
		typ, reason, message = operator.Authorization, string(authorizationErr.State), authorizationErr.Error()
	}
	now := time.Now().UTC()
	if c.Now != nil {
		now = c.Now().UTC()
	}
	status := operator.UpdateCondition(statusFromObject(obj), typ, reason, message, generation, now)
	if updateErr := writeStatus(ctx, c.Client, obj, status); updateErr != nil {
		return updateErr
	}
	if typ == operator.Authorization && c.Recorder != nil {
		c.Recorder.Event(obj, corev1.EventTypeWarning, "BootstrapAuthorization"+reason, message)
	}
	return err
}

func (c *Controller) recordAuthorizationEvent(obj *unstructured.Unstructured, status operator.Status) {
	if c.Recorder == nil {
		return
	}
	for i := len(status.Conditions) - 1; i >= 0; i-- {
		condition := status.Conditions[i]
		if condition.Type != operator.Authorization || condition.Status != "True" {
			continue
		}
		eventType := corev1.EventTypeWarning
		if condition.Reason == string(operator.AuthorizationAccepted) {
			eventType = corev1.EventTypeNormal
		}
		c.Recorder.Event(obj, eventType, "BootstrapAuthorization"+condition.Reason, condition.Message)
		return
	}
}

// ApprovedAnnotation documents the admission-to-controller approval bridge.
func ApprovedAnnotation(value bool) string { return strconv.FormatBool(value) }
