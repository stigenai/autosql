package operatorcontroller

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"autosql/pkg/executor"
	"autosql/pkg/operator"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/envtest"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

// Called by the real publisher fixture. With AUTOSQL_OPERATOR_ENVTEST=1 this
// uses the actual Kubernetes API server as well as the actual PostgreSQL target.
func assertControllerRecoversLostStores(t *testing.T, resource operator.Resource) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	obj := &unstructured.Unstructured{Object: map[string]any{"apiVersion": "autosql.io/v1alpha1", "kind": "AutoSQLSchema", "metadata": map[string]any{"name": "completion-recovery", "namespace": "default"}, "spec": map[string]any{
		"kind": "DeclarativeSchema", "generation": int64(1), "artifactDigest": resource.Spec.ArtifactDigest,
		"source": map[string]any{"inline": resource.Spec.Source.Inline, "format": "hcl"}, "databaseURL": map[string]any{"name": "recovery-db", "key": "url"}, "postgresVersion": int64(resource.Spec.PostgresVersion), "concurrentIndexes": resource.Spec.ConcurrentIndexes,
	}}}
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "recovery-db", Namespace: "default"}, Data: map[string][]byte{"url": []byte(resource.ResolvedDatabaseURL)}}
	cl := completionIntegrationClient(t)
	if err := cl.Create(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if os.Getenv("AUTOSQL_OPERATOR_ENVTEST") != "1" {
		obj.SetUID("fake-api-recovery-resource")
		obj.SetGeneration(1)
	}
	if err := cl.Create(ctx, obj); err != nil {
		t.Fatal(err)
	}
	key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	publications, observations, probes := 0, 0, 0
	wrapped := interceptor.NewClient(cl, interceptor.Funcs{SubResourceUpdate: func(ctx context.Context, underlying client.Client, subresource string, object client.Object, options ...client.SubResourceUpdateOption) error {
		if subresource == "status" && recoveredReady(statusFromObject(object.(*unstructured.Unstructured))) {
			publications++
			if publications < 3 {
				if os.Getenv("AUTOSQL_OPERATOR_ENVTEST") != "1" {
					return apierrors.NewConflict(schema.GroupResource{Group: "autosql.io", Resource: "autosqlschemas"}, object.GetName(), fmt.Errorf("injected competing writer"))
				}
				// A real competing metadata write makes this status candidate stale.
				latest := &unstructured.Unstructured{}
				latest.SetGroupVersionKind(GroupVersionKind)
				if err := underlying.Get(ctx, key, latest); err != nil {
					return err
				}
				latest.SetAnnotations(map[string]string{"autosql.test/conflict": fmt.Sprint(publications)})
				if err := underlying.Update(ctx, latest); err != nil {
					return err
				}
				err := underlying.Status().Update(ctx, object, options...)
				if !apierrors.IsConflict(err) {
					t.Fatalf("API did not reject stale publication: %v", err)
				}
				return err
			}
		}
		return underlying.Status().Update(ctx, object, options...)
	}})
	controller := &Controller{Client: wrapped, Reader: cl, Store: operator.NewMemoryStore(), Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
		t.Fatal("lost stores replayed Apply")
		return operator.ApplyResult{}, nil
	},
		Observe: func(ctx context.Context, r operator.Resource) operator.CompletionResult {
			observations++
			return ObserveArtifactCompletion(ctx, r)
		},
		Probe: func(ctx context.Context, r operator.Resource) executor.ExecutionStartCheck {
			probes++
			return ProbeArtifactExecution(ctx, r)
		},
		VerifyRelease: verifyOperatorReleaseArtifact, VerifyCompletionRelease: verifyOperatorCompletionRelease, RevalidateCompletion: RevalidateArtifactCompletion,
	}
	request := reconcile.Request{NamespacedName: key}
	started := time.Now()
	if _, err := controller.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, key, obj); err != nil {
		t.Fatal(err)
	}
	if !recoveredReady(statusFromObject(obj)) || observations != 1 || probes != 1 || publications != 3 || time.Since(started) > 2*time.Minute {
		t.Fatalf("lost stores/conflict recovery: observations=%d probes=%d publications=%d status=%+v", observations, probes, publications, statusFromObject(obj))
	}
	// Replacing the process loses the local store. Remove Kubernetes status too
	// and prove the new controller re-discovers completion rather than applying.
	obj.Object["status"] = map[string]any{}
	if err := cl.Status().Update(ctx, obj); err != nil {
		t.Fatal(err)
	}
	replacement := &Controller{
		Client: wrapped, Reader: cl, Store: operator.NewMemoryStore(),
		Apply: controller.Apply, Observe: controller.Observe, Probe: controller.Probe,
		RevalidateCompletion: controller.RevalidateCompletion,
		VerifyRelease:        controller.VerifyRelease, VerifyCompletionRelease: controller.VerifyCompletionRelease,
	}
	if _, err := replacement.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, key, obj); err != nil {
		t.Fatal(err)
	}
	if !recoveredReady(statusFromObject(obj)) || observations != 2 || probes != 2 {
		t.Fatalf("replacement did not re-observe: %+v", statusFromObject(obj))
	}
	// Secret version changes invalidate the saved execution key, even when the
	// new credentials resolve to the same target. That must re-observe, not apply.
	if err := cl.Get(ctx, types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}, secret); err != nil {
		t.Fatal(err)
	}
	secret.SetAnnotations(map[string]string{"autosql.test/rotation": "1"})
	if err := cl.Update(ctx, secret); err != nil {
		t.Fatal(err)
	}
	if _, err := replacement.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if observations != 3 || probes != 2 {
		t.Fatalf("reference change did not select observation: observations=%d probes=%d", observations, probes)
	}
}

func completionIntegrationClient(t *testing.T) client.WithWatch {
	t.Helper()
	if os.Getenv("AUTOSQL_OPERATOR_ENVTEST") != "1" {
		return fake.NewClientBuilder().WithScheme(NewScheme()).WithStatusSubresource(&unstructured.Unstructured{Object: map[string]any{"apiVersion": "autosql.io/v1alpha1", "kind": "AutoSQLSchema"}}).Build()
	}
	directory := t.TempDir()
	crd, err := os.ReadFile(filepath.Join("..", "..", "deploy", "operator", "crd.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(directory, "autosqlschema.yaml"), crd, 0600); err != nil {
		t.Fatal(err)
	}
	environment := &envtest.Environment{CRDDirectoryPaths: []string{directory}}
	config, err := environment.Start()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := environment.Stop(); err != nil {
			t.Error(err)
		}
	})
	cl, err := client.NewWithWatch(config, client.Options{Scheme: NewScheme()})
	if err != nil {
		t.Fatal(err)
	}
	return cl
}
