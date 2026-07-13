package terraform

import (
	"context"
	"errors"
	"testing"

	"autosql/pkg/integrations/deploy"
)

func config() ResourceConfig {
	return ResourceConfig{ID: "app-prod", Workflow: Declarative, SourceRef: "file://schema.json", ArtifactDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PolicyDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", TargetSnapshot: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", TargetID: "prod", Environment: "production", ConnectionRef: "env://AUTOSQL_DATABASE_URL"}
}

func TestPlanBindsArtifactAndPolicy(t *testing.T) {
	p, err := PlanResource(PlanRequest{Desired: config()})
	if err != nil {
		t.Fatal(err)
	}
	if p.Action != Create || p.PlanDigest == "" || !p.RequiresApply {
		t.Fatalf("plan=%+v", p)
	}
	if _, err := PlanResource(PlanRequest{Desired: ResourceConfig{ID: "bad", Workflow: Declarative, SourceRef: "x", ArtifactDigest: "x", PolicyDigest: "y", TargetSnapshot: "z", TargetID: "t", Environment: "e", ConnectionRef: "postgres://user:pass@host/db"}}); !errors.Is(err, ErrSensitive) {
		t.Fatalf("sensitive URL error=%v", err)
	}
}

type fakeExecutor struct{ request deploy.Request }

func (f *fakeExecutor) Apply(_ context.Context, r deploy.Request) error { f.request = r; return nil }
func (f *fakeExecutor) Refresh(_ context.Context, id string) (State, error) {
	s := config()
	s.ID = id
	return State{ID: id, Workflow: s.Workflow, SourceRef: s.SourceRef, ArtifactDigest: s.ArtifactDigest, PolicyDigest: s.PolicyDigest, TargetSnapshot: s.TargetSnapshot, TargetID: s.TargetID, Environment: s.Environment, ConnectionRef: s.ConnectionRef}, nil
}
func (f *fakeExecutor) Inspect(ctx context.Context, id string) (State, error) {
	return f.Refresh(ctx, id)
}

func TestApplyRequiresApprovalAndUsesLock(t *testing.T) {
	c := config()
	p, err := PlanResource(PlanRequest{Desired: c})
	if err != nil {
		t.Fatal(err)
	}
	x := &fakeExecutor{}
	l := NewMemoryLocker()
	if _, err = Apply(context.Background(), x, ApplyRequest{Plan: p, Desired: c, ApprovedPlan: "wrong", Operator: "ci"}, l); !errors.Is(err, ErrApproval) {
		t.Fatalf("approval=%v", err)
	}
	s, err := Apply(context.Background(), x, ApplyRequest{Plan: p, Desired: c, ApprovedPlan: p.PlanDigest, Operator: "ci"}, l)
	if err != nil || s.ArtifactDigest != c.ArtifactDigest || x.request.ConnectionRef != "env://AUTOSQL_DATABASE_URL" {
		t.Fatalf("state=%+v err=%v request=%+v", s, err, x.request)
	}
}

func TestImportRefreshNeverResolveSecrets(t *testing.T) {
	c := config()
	x := &fakeExecutor{}
	s, err := Import(context.Background(), x, "imported", c.ConnectionRef)
	if err != nil {
		t.Fatal(err)
	}
	if s.ConnectionRef != "env://AUTOSQL_DATABASE_URL" {
		t.Fatal("connection ref changed")
	}
	if _, err := s.MarshalJSON(); err != nil {
		t.Fatal(err)
	}
}

func TestMemoryLockConflict(t *testing.T) {
	l := NewMemoryLocker()
	release, err := l.Acquire(context.Background(), "x", "one")
	if err != nil {
		t.Fatal(err)
	}
	defer release()
	if _, err = l.Acquire(context.Background(), "x", "two"); !errors.Is(err, ErrStateConflict) {
		t.Fatalf("lock=%v", err)
	}
}
