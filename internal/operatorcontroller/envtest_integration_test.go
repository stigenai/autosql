package operatorcontroller

import (
	"context"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"autosql/pkg/operator"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
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

	obj := testObject()
	obj.SetNamespace("default")
	if err := mgr.GetClient().Create(ctx, &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "default"}, Data: map[string][]byte{"url": []byte("postgres://operator:secret@db/app")}}); err != nil {
		t.Fatal(err)
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
