package operator

import (
	"context"
	"testing"
)

func resource() Resource {
	return Resource{Name: "orders", Spec: Spec{Kind: Declarative, Generation: 1, Source: Source{RegistryDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}, DatabaseURL: &SecretKeyRef{Name: "db", Key: "url"}, RequireApproval: true}}
}
func TestReconcileIsIdempotentAndApprovalGated(t *testing.T) {
	s := NewMemoryStore()
	calls := 0
	r := Reconciler{Store: s, Apply: func(context.Context, Resource, string) error { calls++; return nil }}
	st, e := r.Reconcile(context.Background(), resource(), false)
	if e != nil || st.Conditions[len(st.Conditions)-1].Type != Approval {
		t.Fatalf("approval: %v %#v", e, st)
	}
	st, e = r.Reconcile(context.Background(), resource(), true)
	if e != nil || st.Conditions[len(st.Conditions)-1].Type != Ready {
		t.Fatalf("apply: %v %#v", e, st)
	}
	_, e = r.Reconcile(context.Background(), resource(), true)
	if e != nil || calls != 1 {
		t.Fatalf("duplicate apply calls=%d err=%v", calls, e)
	}
}
func TestRestartUsesPersistentRecord(t *testing.T) {
	s := NewMemoryStore()
	calls := 0
	f := func(context.Context, Resource, string) error { calls++; return nil }
	r := Reconciler{Store: s, Apply: f}
	_, _ = r.Reconcile(context.Background(), resource(), true)
	r = Reconciler{Store: s, Apply: f}
	_, _ = r.Reconcile(context.Background(), resource(), true)
	if calls != 1 {
		t.Fatalf("restart duplicated apply: %d", calls)
	}
}

func TestVersionedSourcesAndSecretValuesNeverEnterStatus(t *testing.T) {
	s := NewMemoryStore()
	obj := resource()
	obj.Spec.Kind = Versioned
	obj.Spec.Generation = 2
	obj.Spec.Source = Source{SecretRef: &SecretKeyRef{Name: "migration-secret", Key: "artifact"}}
	obj.Spec.DatabaseURL = &SecretKeyRef{Name: "db", Key: "url"}
	obj.Spec.RequireApproval = false
	r := Reconciler{Store: s, Apply: func(context.Context, Resource, string) error { return nil }}
	st, err := r.Reconcile(context.Background(), obj, true)
	if err != nil || st.AppliedDigest != "" {
		t.Fatalf("unexpected status: %#v err=%v", st, err)
	}
	for _, c := range st.Conditions {
		if c.Message == "migration-secret" || c.Message == "artifact" || c.Message == "url" {
			t.Fatal("secret reference leaked into status")
		}
	}
}
