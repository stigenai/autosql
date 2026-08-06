package operatorcontroller

import (
	"bytes"
	"context"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"autosql/pkg/operator"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	utilyaml "k8s.io/apimachinery/pkg/util/yaml"
	"k8s.io/client-go/util/retry"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"
)

func TestEnvtestReconcilesCRAndWritesStatus(t *testing.T) {
	if os.Getenv("AUTOSQL_OPERATOR_ENVTEST") != "1" {
		t.Skip("set AUTOSQL_OPERATOR_ENVTEST=1 to run the Kubernetes API-server integration test")
	}
	root, err := filepath.Abs(filepath.Join("..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	crd, err := os.ReadFile(filepath.Join(root, "deploy", "operator", "crd.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	crdDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(crdDir, "autosqlschema.yaml"), crd, 0600); err != nil {
		t.Fatal(err)
	}
	testEnv := &envtest.Environment{CRDDirectoryPaths: []string{crdDir}}
	cfg, err := testEnv.Start()
	if err != nil {
		t.Fatal(err)
	}
	defer testEnv.Stop()
	mgr, err := ctrl.NewManager(cfg, ctrl.Options{Scheme: NewScheme(), Metrics: metricsserver.Options{BindAddress: "0"}, HealthProbeBindAddress: "0"})
	if err != nil {
		t.Fatal(err)
	}
	var calls atomic.Int32
	controller := &Controller{Client: mgr.GetClient(), Reader: mgr.GetAPIReader(), Store: operator.NewMemoryStore(), Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
		calls.Add(1)
		return operator.ApplyResult{PlanDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TargetIdentity: "prod/orders", ExecutionID: "exec-envtest", AppliedSteps: 1}, nil
	}}
	if err := controller.SetupWithManager(mgr); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	managerDone := make(chan error, 1)
	go func() { managerDone <- mgr.Start(ctx) }()
	if !mgr.GetCache().WaitForCacheSync(ctx) {
		t.Fatal("manager cache did not synchronize")
	}
	if err := installAuthorizationAdmission(ctx, mgr.GetClient(), filepath.Join(root, "deploy", "operator", "admission.yaml")); err != nil {
		t.Fatal(err)
	}

	obj := testObject()
	obj.SetNamespace("default")
	if err := mgr.GetClient().Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"}, Data: map[string][]byte{"url": []byte("postgres://operator:secret@db/app")}}); err != nil {
		t.Fatal(err)
	}
	explicitHCL := testObject()
	explicitHCL.SetName("explicit-hcl")
	explicitHCL.SetNamespace("default")
	explicitHCLSpec := explicitHCL.Object["spec"].(map[string]any)
	explicitHCLSpec["source"] = map[string]any{"format": "hcl", "inline": `schema "app" {}`}
	explicitHCLSpec["postgresVersion"] = int64(18)
	explicitHCLSpec["concurrentIndexes"] = true
	if err := mgr.GetClient().Create(ctx, explicitHCL); err != nil {
		t.Fatalf("Kubernetes 1.35 rejected explicit HCL source format: %v", err)
	}
	invalidFormat := testObject()
	invalidFormat.SetName("invalid-source-format")
	invalidFormat.SetNamespace("default")
	invalidFormat.Object["spec"].(map[string]any)["source"] = map[string]any{"format": "json", "inline": `{}`}
	if err := mgr.GetClient().Create(ctx, invalidFormat); err == nil {
		t.Fatal("API server accepted unsupported source format")
	}
	invalidVersion := testObject()
	invalidVersion.SetName("invalid-postgres-version")
	invalidVersion.SetNamespace("default")
	invalidVersion.Object["spec"].(map[string]any)["postgresVersion"] = int64(13)
	if err := mgr.GetClient().Create(ctx, invalidVersion); err == nil {
		t.Fatal("API server accepted unsupported PostgreSQL version")
	}
	formatOnDigest := testObject()
	formatOnDigest.SetName("format-on-digest")
	formatOnDigest.SetNamespace("default")
	formatOnDigest.Object["spec"].(map[string]any)["source"] = map[string]any{"format": "sql", "registryDigest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}
	if err := mgr.GetClient().Create(ctx, formatOnDigest); err == nil {
		t.Fatal("API server accepted source format on registry digest")
	}
	invalid := testObject()
	invalid.SetName("invalid")
	invalid.SetNamespace("default")
	spec := invalid.Object["spec"].(map[string]any)
	spec["source"] = map[string]any{"registryDigest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", "inline": "CREATE TABLE bad (id bigint);"}
	if err := mgr.GetClient().Create(ctx, invalid); err == nil {
		t.Fatal("API server accepted multiple source variants")
	}
	mismatched := testObject()
	mismatched.SetName("mismatched-digest")
	mismatched.SetNamespace("default")
	mismatchedSpec := mismatched.Object["spec"].(map[string]any)
	mismatchedSpec["artifactDigest"] = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := mgr.GetClient().Create(ctx, mismatched); err == nil {
		t.Fatal("API server accepted mismatched registry and artifact digests")
	}
	validAuthorization := authorizationAdmissionObject("valid-authorization")
	if err := mgr.GetClient().Create(ctx, validAuthorization); err != nil {
		t.Fatalf("valid authorization references rejected: %v", err)
	}
	for _, field := range []string{"privateKey", "manifest", "publicKey", "namespace", "misspelledPolicy"} {
		invalidAuthorization := authorizationAdmissionObject("invalid-authorization-" + strings.ToLower(field))
		invalidAuthorization.Object["spec"].(map[string]any)["bootstrapAuthorization"].(map[string]any)[field] = "forbidden-secret-value"
		if err := mgr.GetClient().Create(ctx, invalidAuthorization); err == nil {
			t.Fatalf("API admission accepted bootstrapAuthorization field %q", field)
		}
	}
	nestedOverride := authorizationAdmissionObject("invalid-authorization-namespace-override")
	nestedOverride.Object["spec"].(map[string]any)["bootstrapAuthorization"].(map[string]any)["manifestSecretRef"].(map[string]any)["namespace"] = "other"
	if err := mgr.GetClient().Create(ctx, nestedOverride); err == nil {
		t.Fatal("API admission accepted cross-namespace manifestSecretRef")
	}
	storedAuthorization := &unstructured.Unstructured{}
	storedAuthorization.SetGroupVersionKind(GroupVersionKind)
	if err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{Namespace: "default", Name: validAuthorization.GetName()}, storedAuthorization); err != nil {
		t.Fatal(err)
	}
	storedAuthorization.Object["spec"].(map[string]any)["bootstrapAuthorization"].(map[string]any)["privateKey"] = "update-forbidden-secret-value"
	if err := mgr.GetClient().Update(ctx, storedAuthorization); err == nil {
		t.Fatal("API admission accepted privateKey on update")
	}
	if err := mgr.GetClient().Create(ctx, obj); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, mgr.GetClient(), obj, string(operator.Approval))
	current := &unstructured.Unstructured{}
	current.SetGroupVersionKind(GroupVersionKind)
	key := types.NamespacedName{Namespace: "default", Name: "orders"}
	if err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		fresh := &unstructured.Unstructured{}
		fresh.SetGroupVersionKind(GroupVersionKind)
		if err := mgr.GetAPIReader().Get(ctx, key, fresh); err != nil {
			return err
		}
		base := fresh.DeepCopy()
		fresh.SetAnnotations(map[string]string{"autosql.io/approved": "true"})
		if err := mgr.GetClient().Patch(ctx, fresh, client.MergeFrom(base)); err != nil {
			return err
		}
		current = fresh
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	waitForStatus(t, mgr.GetClient(), current, string(operator.Ready))
	if err := mgr.GetAPIReader().Get(ctx, types.NamespacedName{Namespace: "default", Name: "orders"}, current); err != nil {
		t.Fatal(err)
	}
	executionID, _, _ := unstructured.NestedString(current.Object, "status", "executionID")
	planDigest, _, _ := unstructured.NestedString(current.Object, "status", "planDigest")
	if executionID != "exec-envtest" || planDigest == "" {
		t.Fatalf("actionable status missing execution metadata: %v", current.Object["status"])
	}
	if calls.Load() != 1 {
		t.Fatalf("apply calls=%d", calls.Load())
	}
	cancel()
	select {
	case err := <-managerDone:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("manager did not stop")
	}
}

func installAuthorizationAdmission(ctx context.Context, cl client.Client, path string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	decoder := utilyaml.NewYAMLOrJSONDecoder(bytes.NewReader(raw), 4096)
	for {
		var object map[string]any
		if err := decoder.Decode(&object); err != nil {
			if err == io.EOF {
				return nil
			}
			return err
		}
		if len(object) == 0 {
			continue
		}
		if err := cl.Create(ctx, &unstructured.Unstructured{Object: object}); err != nil {
			return err
		}
	}
}

func authorizationAdmissionObject(name string) *unstructured.Unstructured {
	obj := testObject()
	obj.SetName(name)
	obj.SetNamespace("default")
	spec := obj.Object["spec"].(map[string]any)
	spec["kind"] = "DeclarativeSchema"
	spec["source"] = map[string]any{"inline": "create schema app"}
	spec["databaseTarget"] = map[string]any{
		"mode": "external", "name": "cell", "owner": "postgres", "maintenanceDatabase": "postgres",
		"endpoint": map[string]any{"host": "db.internal", "port": int64(5432), "tlsMode": "verify-full"}, "connectionLimit": int64(-1), "allowConnections": true,
	}
	spec["bootstrapAuthorization"] = map[string]any{
		"manifestSecretRef":  map[string]any{"name": "authorization", "key": "manifest"},
		"publicKeySecretRef": map[string]any{"name": "authorization", "key": "public-key"},
		"issuer":             "security", "signer": "dba", "purpose": "bootstrap-authorization",
	}
	return obj
}

func waitForStatus(t *testing.T, cl client.Client, obj *unstructured.Unstructured, want string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	for {
		current := &unstructured.Unstructured{}
		current.SetGroupVersionKind(GroupVersionKind)
		err := cl.Get(ctx, types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}, current)
		if err == nil {
			conditions, _, _ := unstructured.NestedSlice(current.Object, "status", "conditions")
			for _, raw := range conditions {
				if condition, ok := raw.(map[string]any); ok && condition["type"] == want && condition["status"] == "True" {
					return
				}
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s; object=%v", want, current.Object)
		case <-time.After(100 * time.Millisecond):
		}
	}
}
