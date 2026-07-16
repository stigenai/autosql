package operatorcontroller

import (
	"context"
	"testing"

	"autosql/pkg/bootstrap"
	"autosql/pkg/operator"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func testObject() *unstructured.Unstructured {
	o := &unstructured.Unstructured{Object: map[string]any{
		"apiVersion": "autosql.io/v1alpha1", "kind": "AutoSQLSchema",
		"metadata": map[string]any{"name": "orders", "namespace": "app"},
		"spec": map[string]any{
			"kind": "VersionedMigration", "generation": int64(1), "requireApproval": true,
			"artifactDigest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef",
			"source":         map[string]any{"registryDigest": "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"},
			"databaseURL":    map[string]any{"name": "db", "key": "url"},
		},
	}}
	o.SetGroupVersionKind(GroupVersionKind)
	return o
}

func TestReconcileUpdatesStatusAndHonorsApproval(t *testing.T) {
	obj := testObject()
	scheme := NewScheme()
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "app"}, Data: map[string][]byte{"url": []byte("postgres://operator:secret@db/app")}}
	cl := fake.NewClientBuilder().WithScheme(scheme).WithObjects(obj, secret).WithStatusSubresource(obj).Build()
	calls := 0
	c := &Controller{Client: cl, Store: operator.NewMemoryStore(), Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
		calls++
		return operator.ApplyResult{PlanDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TargetIdentity: "prod/orders", ExecutionID: "exec-1"}, nil
	}}
	ctx := context.Background()
	if _, err := c.Reconcile(ctx, requestFor(obj)); err != nil {
		t.Fatal(err)
	}
	if calls != 0 {
		t.Fatalf("approval gate bypassed: %d calls", calls)
	}
	got := &unstructured.Unstructured{}
	got.SetGroupVersionKind(GroupVersionKind)
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "app", Name: "orders"}, got); err != nil {
		t.Fatal(err)
	}
	conditions, _, err := unstructured.NestedSlice(got.Object, "status", "conditions")
	approvalFound := false
	for _, raw := range conditions {
		if condition, ok := raw.(map[string]any); ok && condition["type"] == string(operator.Approval) && condition["status"] == "True" {
			approvalFound = true
		}
	}
	if err != nil || !approvalFound {
		t.Fatalf("approval status: %#v err=%v", conditions, err)
	}
	annotations := got.GetAnnotations()
	if annotations == nil {
		annotations = map[string]string{}
	}
	annotations["autosql.io/approved"] = "true"
	got.SetAnnotations(annotations)
	if err := cl.Update(ctx, got); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Reconcile(ctx, requestFor(got)); err != nil {
		t.Fatal(err)
	}
	if calls != 1 {
		t.Fatalf("apply calls=%d", calls)
	}
	if err := cl.Get(ctx, types.NamespacedName{Namespace: "app", Name: "orders"}, got); err != nil {
		t.Fatal(err)
	}
	if got := got.Object["status"].(map[string]any)["executionID"]; got != "exec-1" {
		t.Fatalf("execution ID status=%v", got)
	}
}

func requestFor(obj *unstructured.Unstructured) reconcile.Request {
	return reconcile.Request{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}}
}

func TestResourceFromObjectParsesOIDCBootstrapAuthority(t *testing.T) {
	obj := testObject()
	spec := obj.Object["spec"].(map[string]any)
	spec["createDatabase"] = true
	spec["bootstrapAuthority"] = map[string]any{
		"identities": []any{map[string]any{
			"name": "operator", "subject": "system:serviceaccount:autosql:operator", "authentication": "oidc",
			"capabilities": []any{"create_database", "manage_schema_objects", "transfer_ownership"},
		}},
		"assignments": []any{
			map[string]any{"responsibility": "database_creation", "identity": "operator"},
			map[string]any{"responsibility": "schema_objects", "identity": "operator"},
			map[string]any{"responsibility": "ownership_handoff", "identity": "operator"},
		},
	}
	resource, _, err := resourceFromObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	if !resource.Spec.CreateDatabase || resource.Spec.BootstrapAuthority == nil || resource.Spec.BootstrapAuthority.Identities[0].Authentication != bootstrap.OIDC {
		t.Fatalf("resource=%+v", resource.Spec)
	}
}

func TestResourceFromObjectParsesDatabaseTargetContract(t *testing.T) {
	obj := testObject()
	spec := obj.Object["spec"].(map[string]any)
	spec["maintenanceDatabaseURL"] = map[string]any{"name": "maintenance-db", "key": "url"}
	spec["databaseTarget"] = map[string]any{
		"mode": "managed", "name": "cell", "owner": "cell_owner", "maintenanceDatabase": "postgres",
		"endpoint":        map[string]any{"host": "postgres.internal", "port": int64(5432), "tlsMode": "require"},
		"connectionLimit": int64(20), "allowConnections": true,
	}
	resource, _, err := resourceFromObject(obj)
	if err != nil {
		t.Fatal(err)
	}
	if resource.Spec.DatabaseTarget == nil || resource.Spec.DatabaseTarget.Name != "cell" || resource.Spec.MaintenanceDatabaseURL == nil || resource.Spec.MaintenanceDatabaseURL.Name != "maintenance-db" {
		t.Fatalf("resource=%+v", resource.Spec)
	}
}
