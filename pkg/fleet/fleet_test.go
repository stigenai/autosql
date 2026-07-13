package fleet

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

const testDigest = "sha256:0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func targets() []Target {
	return []Target{{ID: "db-a", Environment: "prod", ConnectionRef: "env://DB_A"}, {ID: "db-b", Environment: "prod", ConnectionRef: "env://DB_B"}}
}

func plan() RolloutPlan {
	ts := targets()
	return RolloutPlan{ID: "rollout-1", ArtifactDigest: testDigest, TargetSnapshotDigest: testDigest, SnapshotID: testDigest, Groups: []Group{{Name: "canary", TargetIDs: []string{"db-a"}, MaxParallel: 1, BatchSize: 1, FailPolicy: FailFast}, {Name: "rest", TargetIDs: []string{"db-b"}, DependsOn: []string{"canary"}, MaxParallel: 1, BatchSize: 1, FailPolicy: FailFast}}, ExecutionGroups: []ExecutionGroup{{Name: "canary", Targets: []Target{ts[0]}, Batches: []Batch{{Targets: []Target{ts[0]}, Canary: true}}, MaxParallel: 1, BatchSize: 1, FailPolicy: FailFast}, {Name: "rest", DependsOn: []string{"canary"}, Targets: []Target{ts[1]}, Batches: []Batch{{Targets: []Target{ts[1]}}}, MaxParallel: 1, BatchSize: 1, FailPolicy: FailFast}}}
}

func TestTargetSnapshotMasksCredentialsAndRejectsDuplicates(t *testing.T) {
	s, err := NewSnapshot([]Target{{ID: "db", Environment: "prod", ConnectionRef: "postgres://user:password@db/x?token=secret"}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(s.Targets[0].ConnectionRef, "password") || strings.Contains(s.Targets[0].ConnectionRef, "secret") {
		t.Fatalf("unmasked %q", s.Targets[0].ConnectionRef)
	}
	if _, err = NewSnapshot([]Target{{ID: "db", Environment: "prod", ConnectionRef: "env://A"}, {ID: "db", Environment: "prod", ConnectionRef: "env://B"}}); !errors.Is(err, ErrDuplicateTarget) {
		t.Fatalf("duplicate err=%v", err)
	}
}

func TestResumableExecutionAndStatus(t *testing.T) {
	p, ts := plan(), targets()
	if err := p.Validate(ts); err != nil {
		t.Fatal(err)
	}
	sc := NewStore()
	calls := 0
	e := &Executor{Store: sc, Retry: RetryPolicy{MaxAttempts: 2}, Now: func() time.Time { return time.Unix(100, 0) }, Apply: func(context.Context, Target, string) error {
		calls++
		if calls == 1 {
			return transientError{}
		}
		return nil
	}}
	r, err := e.Execute(context.Background(), "run-1", p, ts)
	if err != nil || r.State != "succeeded" || calls != 3 {
		t.Fatalf("state=%+v calls=%d err=%v", r, calls, err)
	}
	rows := sc.Status(StatusFilter{ArtifactDigest: testDigest, States: []TargetState{Succeeded}})
	if len(rows) != 2 {
		t.Fatalf("status=%+v", rows)
	}
}

type transientError struct{}

func (transientError) Error() string   { return "temporary" }
func (transientError) Transient() bool { return true }

func TestGateHookAndRecoveryPolicies(t *testing.T) {
	now := time.Unix(100, 0)
	e := Evidence{RolloutID: "r", Stage: "prod", ArtifactDigest: testDigest, TargetSnapshotDigest: testDigest, Checks: map[string]string{"health": "pass"}}
	p := GatePolicy{RequiredChecks: []string{"health"}, AuthorizedApprovers: map[string]bool{"alice": true}}
	if err := p.Evaluate(e, &Approval{RolloutID: "r", Stage: "prod", ArtifactDigest: testDigest, TargetSnapshotDigest: testDigest, Approver: "alice", At: now}); err != nil {
		t.Fatal(err)
	}
	secret := "secret-value"
	hook := Hook{ID: "health", Timeout: time.Second, Retries: 1, Run: func(context.Context, HookInput) (HookOutput, error) {
		return HookOutput{Message: "token=" + secret}, nil
	}}
	result, err := RunHook(context.Background(), hook, HookInput{ArtifactDigest: testDigest, Secrets: map[string]string{"token": secret}})
	if err != nil || strings.Contains(result.Output.Message, secret) {
		t.Fatalf("hook=%+v err=%v", result, err)
	}
	store := NewStore()
	if _, err = store.Create("r", plan(), targets(), now); err != nil {
		t.Fatal(err)
	}
	recovery := &Recovery{Store: store, Policy: RecoveryPolicy{AllowSkip: true}, Now: func() time.Time { return now }}
	if _, err = recovery.Skip(context.Background(), StatusFilter{RolloutID: "r", TargetID: "db-a"}, "operator-approved"); err != nil {
		t.Fatal(err)
	}
}
