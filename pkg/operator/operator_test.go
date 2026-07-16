package operator

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosql/pkg/bootstrap"
)

func TestFileStorePersistsRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s, err := NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Record{ApplyingKey: "apply", AppliedKey: "done", Status: Status{AppliedDigest: "digest"}}
	if err := s.Save("default/orders", want); err != nil {
		t.Fatal(err)
	}
	s, err = NewFileStore(path)
	if err != nil {
		t.Fatal(err)
	}
	got, ok := s.Load("default/orders")
	if !ok || got.AppliedKey != want.AppliedKey || got.Status.AppliedDigest != want.Status.AppliedDigest {
		t.Fatalf("persisted record=%#v ok=%v", got, ok)
	}
}

func resource() Resource {
	return Resource{Name: "orders", Spec: Spec{Kind: Declarative, Generation: 1, ArtifactDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef", Source: Source{RegistryDigest: "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"}, DatabaseURL: &SecretKeyRef{Name: "db", Key: "url"}, RequireApproval: true}}
}
func TestReconcileIsIdempotentAndApprovalGated(t *testing.T) {
	s := NewMemoryStore()
	calls := 0
	r := Reconciler{Store: s, Apply: func(context.Context, Resource, string) (ApplyResult, error) { calls++; return ApplyResult{}, nil }}
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
	f := func(context.Context, Resource, string) (ApplyResult, error) { calls++; return ApplyResult{}, nil }
	r := Reconciler{Store: s, Apply: f}
	_, _ = r.Reconcile(context.Background(), resource(), true)
	r = Reconciler{Store: s, Apply: f}
	_, _ = r.Reconcile(context.Background(), resource(), true)
	if calls != 1 {
		t.Fatalf("restart duplicated apply: %d", calls)
	}
}

func TestPodReplacementReverifiesInsteadOfTrustingKubernetesStatus(t *testing.T) {
	obj := resource()
	obj.Status = Status{ObservedGeneration: obj.Spec.Generation, AppliedDigest: obj.Spec.ArtifactDigest, PlanDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", RecoveryState: "none", ExecutionID: "exec-1", AppliedFingerprint: "sha256:" + strings.Repeat("f", 64), AuthorizationExpiresAt: time.Now().Add(time.Hour)}
	calls := 0
	r := Reconciler{Store: NewMemoryStore(), Apply: func(context.Context, Resource, string) (ApplyResult, error) {
		calls++
		return ApplyResult{}, nil
	}}
	status, err := r.Reconcile(context.Background(), obj, true)
	if err != nil || calls != 1 || status.AppliedFingerprint == "" {
		t.Fatalf("reverified status=%#v calls=%d err=%v", status, calls, err)
	}
}

func TestSourceChangeInvalidatesApplyKey(t *testing.T) {
	s := NewMemoryStore()
	calls := 0
	r := Reconciler{Store: s, Apply: func(context.Context, Resource, string) (ApplyResult, error) { calls++; return ApplyResult{}, nil }}
	obj := resource()
	obj.Spec.RequireApproval = false
	if _, err := r.Reconcile(context.Background(), obj, true); err != nil {
		t.Fatal(err)
	}
	obj.Spec.Source = Source{URL: "https://schemas.example.test/orders-v2.sql"}
	if _, err := r.Reconcile(context.Background(), obj, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("source change was treated as idempotent: calls=%d", calls)
	}
}

func TestMetadataAndRuntimeReferenceBindingsInvalidateApplyKey(t *testing.T) {
	store := NewMemoryStore()
	calls := 0
	r := Reconciler{Store: store, Apply: func(context.Context, Resource, string) (ApplyResult, error) {
		calls++
		return ApplyResult{SourceDigest: "sha256:" + strings.Repeat("a", 64)}, nil
	}}
	obj := resource()
	obj.Spec.RequireApproval = false
	obj.MetadataGeneration = 1
	obj.AuthorizationManifestBinding = RuntimeReferenceBinding{ResourceVersion: "1", ContentDigest: "sha256:" + strings.Repeat("b", 64)}
	if _, err := r.Reconcile(context.Background(), obj, true); err != nil {
		t.Fatal(err)
	}
	obj.AuthorizationManifestBinding.ResourceVersion = "2"
	obj.AuthorizationManifestBinding.ContentDigest = "sha256:" + strings.Repeat("c", 64)
	if _, err := r.Reconcile(context.Background(), obj, true); err != nil {
		t.Fatal(err)
	}
	obj.MetadataGeneration = 2
	if _, err := r.Reconcile(context.Background(), obj, true); err != nil {
		t.Fatal(err)
	}
	if calls != 3 {
		t.Fatalf("binding changes did not force verification: calls=%d", calls)
	}
}

func TestPostgresRenderContractInvalidatesApplyKey(t *testing.T) {
	obj := resource()
	base := applyKey(obj)
	obj.Spec.PostgresVersion = 16
	if got := applyKey(obj); got == base {
		t.Fatal("postgresVersion did not invalidate apply key")
	}
	versioned := applyKey(obj)
	obj.Spec.ConcurrentIndexes = true
	if got := applyKey(obj); got == versioned {
		t.Fatal("concurrentIndexes did not invalidate apply key")
	}
}

func TestAuthorizationExpiryForcesReverification(t *testing.T) {
	now := time.Date(2026, 7, 16, 12, 0, 0, 0, time.UTC)
	calls := 0
	r := Reconciler{Store: NewMemoryStore(), Now: func() time.Time { return now }, Apply: func(context.Context, Resource, string) (ApplyResult, error) {
		calls++
		return ApplyResult{AuthorizationState: AuthorizationAccepted, AuthorizationExpiresAt: now.Add(time.Minute)}, nil
	}}
	obj := resource()
	obj.Spec.RequireApproval = false
	if _, err := r.Reconcile(context.Background(), obj, true); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * time.Second)
	if _, err := r.Reconcile(context.Background(), obj, true); err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Second)
	if _, err := r.Reconcile(context.Background(), obj, true); err != nil {
		t.Fatal(err)
	}
	if calls != 2 {
		t.Fatalf("expired authorization was not reverified: calls=%d", calls)
	}
}

func TestApplyFailureIsRetryable(t *testing.T) {
	s := NewMemoryStore()
	calls := 0
	r := Reconciler{Store: s, Apply: func(context.Context, Resource, string) (ApplyResult, error) {
		calls++
		if calls == 1 {
			return ApplyResult{}, errors.New("temporary")
		}
		return ApplyResult{}, nil
	}}
	obj := resource()
	obj.Spec.RequireApproval = false
	if _, err := r.Reconcile(context.Background(), obj, true); err == nil {
		t.Fatal("expected first apply failure")
	}
	failed, _ := s.Load(obj.Name)
	if failed.Status.RetryCount != 1 || failed.Status.Conditions[len(failed.Status.Conditions)-1].Type != Failed {
		t.Fatalf("failure status=%#v", failed.Status)
	}
	status, err := r.Reconcile(context.Background(), obj, true)
	if err != nil || status.Conditions[len(status.Conditions)-1].Type != Ready || calls != 2 {
		t.Fatalf("retry status=%#v calls=%d err=%v", status, calls, err)
	}
}

func TestAcceptedBootstrapAuthorizationRemainsVisibleWhenApplyFails(t *testing.T) {
	obj := resource()
	obj.Spec.RequireApproval = false
	r := Reconciler{Store: NewMemoryStore(), Apply: func(context.Context, Resource, string) (ApplyResult, error) {
		return ApplyResult{AuthorizationState: AuthorizationAccepted}, errors.New("database unavailable")
	}}
	status, err := r.Reconcile(context.Background(), obj, true)
	if err == nil {
		t.Fatal("apply failure was ignored")
	}
	accepted, applyFailed := false, false
	for _, condition := range status.Conditions {
		accepted = accepted || (condition.Type == Authorization && condition.Status == "True" && condition.Reason == string(AuthorizationAccepted))
		applyFailed = applyFailed || (condition.Type == Failed && condition.Status == "True")
	}
	if !accepted || !applyFailed {
		t.Fatalf("accepted authorization and apply failure were not independently reported: %#v", status.Conditions)
	}
}

func TestApplyOutcomeIsPersistedForRecovery(t *testing.T) {
	s := NewMemoryStore()
	obj := resource()
	obj.Spec.RequireApproval = false
	r := Reconciler{Store: s, Apply: func(context.Context, Resource, string) (ApplyResult, error) {
		return ApplyResult{Status: "uncertain", PlanDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", TargetIdentity: "prod/orders", ExecutionID: "exec-1", PendingStep: "step-2", RecoveryGuidance: "reconcile before retry", AppliedSteps: 1}, errors.New("commit outcome uncertain")
	}}
	status, err := r.Reconcile(context.Background(), obj, true)
	if err == nil || status.RecoveryState != "uncertain" || status.ExecutionID != "exec-1" || status.PendingStep != "step-2" || status.AppliedSteps != 1 {
		t.Fatalf("outcome status=%#v err=%v", status, err)
	}
}

func TestVersionedSourcesAndSecretValuesNeverEnterStatus(t *testing.T) {
	s := NewMemoryStore()
	obj := resource()
	obj.Spec.Kind = Versioned
	obj.Spec.Generation = 2
	obj.Spec.ArtifactDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
	obj.Spec.Source = Source{SecretRef: &SecretKeyRef{Name: "migration-secret", Key: "artifact"}}
	obj.Spec.DatabaseURL = &SecretKeyRef{Name: "db", Key: "url"}
	obj.Spec.RequireApproval = false
	r := Reconciler{Store: s, Apply: func(context.Context, Resource, string) (ApplyResult, error) { return ApplyResult{}, nil }}
	st, err := r.Reconcile(context.Background(), obj, true)
	if err != nil {
		t.Fatalf("unexpected status: %#v err=%v", st, err)
	}
	for _, c := range st.Conditions {
		if c.Message == "migration-secret" || c.Message == "artifact" || c.Message == "url" {
			t.Fatal("secret reference leaked into status")
		}
	}
}

func TestRegistryDigestMustMatchArtifactDigest(t *testing.T) {
	obj := resource()
	obj.Spec.Source.RegistryDigest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	obj.Spec.RequireApproval = false
	r := Reconciler{Store: NewMemoryStore(), Apply: func(context.Context, Resource, string) (ApplyResult, error) {
		t.Fatal("mismatched registry artifact was applied")
		return ApplyResult{}, nil
	}}
	if _, err := r.Reconcile(context.Background(), obj, true); err == nil {
		t.Fatal("mismatched registry and artifact digests accepted")
	}
}

func TestFreshDatabaseRequiresValidSharedBootstrapAuthority(t *testing.T) {
	obj := resource()
	obj.Spec.RequireApproval = false
	obj.Spec.CreateDatabase = true
	r := Reconciler{Store: NewMemoryStore(), Apply: func(context.Context, Resource, string) (ApplyResult, error) {
		t.Fatal("invalid bootstrap authority reached apply")
		return ApplyResult{}, nil
	}}
	if _, err := r.Reconcile(context.Background(), obj, true); err == nil {
		t.Fatal("createDatabase without authority accepted")
	}
	obj.Spec.BootstrapAuthority = &bootstrap.Contract{
		Identities:  []bootstrap.Identity{{Name: "operator", Subject: "system:serviceaccount:autosql:operator", Authentication: bootstrap.AWSIRSA, Capabilities: []bootstrap.Capability{bootstrap.CreateDatabase}}},
		Assignments: []bootstrap.Assignment{{Responsibility: bootstrap.DatabaseCreation, Identity: "operator"}},
	}
	calls := 0
	r = Reconciler{Store: NewMemoryStore(), Apply: func(context.Context, Resource, string) (ApplyResult, error) { calls++; return ApplyResult{}, nil }}
	if _, err := r.Reconcile(context.Background(), obj, true); err != nil || calls != 1 {
		t.Fatalf("shared authority rejected: calls=%d err=%v", calls, err)
	}
}

func TestManagedDatabaseTargetRequiresMaintenanceReferenceAndAuthority(t *testing.T) {
	obj := resource()
	obj.Spec.RequireApproval = false
	obj.Spec.DatabaseTarget = &bootstrap.DatabaseTarget{Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: "postgres.internal", Port: 5432, TLSMode: "require"}, MaintenanceDatabase: "postgres", Name: "cell", Owner: "cell_owner", Encoding: "UTF8", Template: "template0", Tablespace: "pg_default", ConnectionLimit: 20, AllowConnections: true}
	reconciler := Reconciler{Store: NewMemoryStore(), Apply: func(context.Context, Resource, string) (ApplyResult, error) {
		t.Fatal("invalid target applied")
		return ApplyResult{}, nil
	}}
	if _, err := reconciler.Reconcile(context.Background(), obj, true); err == nil {
		t.Fatal("managed target without authority/reference passed")
	}
	obj.Spec.MaintenanceDatabaseURL = &SecretKeyRef{Name: "maintenance-db", Key: "url"}
	obj.Spec.BootstrapAuthority = &bootstrap.Contract{Identities: []bootstrap.Identity{{Name: "cluster", Subject: "db-admin", Authentication: bootstrap.OIDC, Capabilities: []bootstrap.Capability{bootstrap.CreateDatabase}}}, Assignments: []bootstrap.Assignment{{Responsibility: bootstrap.DatabaseCreation, Identity: "cluster"}}}
	calls := 0
	reconciler = Reconciler{Store: NewMemoryStore(), Apply: func(context.Context, Resource, string) (ApplyResult, error) { calls++; return ApplyResult{}, nil }}
	if _, err := reconciler.Reconcile(context.Background(), obj, true); err != nil || calls != 1 {
		t.Fatalf("managed target rejected calls=%d err=%v", calls, err)
	}
}
