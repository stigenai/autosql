package operator

import (
	"context"
	"errors"
	"testing"
	"time"

	"autosql/pkg/executor"
)

func TestOperatorProbeRecoversWithoutAnyCheckpoint(t *testing.T) {
	obj := resource()
	now := time.Now().UTC()
	probes, observations := 0, 0
	r := Reconciler{Store: NewMemoryStore(), Probe: func(context.Context, Resource) executor.ExecutionStartCheck {
		probes++
		return executor.ExecutionStartCheck{Started: true}
	}, Observe: func(_ context.Context, obj Resource) CompletionResult {
		observations++
		return provenCompletion(obj, now)
	}, Apply: func(context.Context, Resource, string) (ApplyResult, error) {
		t.Fatal("lost state replayed apply")
		return ApplyResult{}, nil
	}}
	status, err := r.Reconcile(context.Background(), obj, true)
	if err != nil || probes != 1 || observations != 1 || status.AppliedDigest != obj.Spec.ArtifactDigest {
		t.Fatalf("lost state not recovered: %+v %v", status, err)
	}
}

func TestOperatorProbeAllowsVerifiedUnstartedRetry(t *testing.T) {
	obj := resource()
	probes, applies := 0, 0
	r := Reconciler{Store: NewMemoryStore(), Probe: func(_ context.Context, current Resource) executor.ExecutionStartCheck {
		probes++
		if current.ExecutionMayHaveStarted != (probes > 1) {
			t.Fatal("possible execution did not require audit proof")
		}
		return executor.ExecutionStartCheck{}
	}, Observe: func(context.Context, Resource) CompletionResult {
		t.Fatal("unstarted work treated as completion")
		return CompletionResult{}
	}, Apply: func(context.Context, Resource, string) (ApplyResult, error) {
		applies++
		if applies == 1 {
			return ApplyResult{}, errors.New("connection unavailable before execution")
		}
		return ApplyResult{AppliedSteps: 1}, nil
	}}
	if _, err := r.Reconcile(context.Background(), obj, true); err == nil {
		t.Fatal("initial failure hidden")
	}
	if _, err := r.Reconcile(context.Background(), obj, true); err != nil || applies != 2 || probes != 2 {
		t.Fatalf("verified unstarted retry blocked: applies=%d probes=%d err=%v", applies, probes, err)
	}
}

func TestOperatorProbeReobservesChangedReferencesForAppliedRelease(t *testing.T) {
	obj := resource()
	now := time.Now().UTC()
	applies, probes, observations := 0, 0, 0
	r := Reconciler{Store: NewMemoryStore(), Probe: func(context.Context, Resource) executor.ExecutionStartCheck {
		probes++
		return executor.ExecutionStartCheck{}
	}, Observe: func(_ context.Context, obj Resource) CompletionResult {
		observations++
		return provenCompletion(obj, now)
	}, Apply: func(context.Context, Resource, string) (ApplyResult, error) {
		applies++
		return ApplyResult{AppliedSteps: 1}, nil
	}}
	if _, err := r.Reconcile(context.Background(), obj, true); err != nil {
		t.Fatal(err)
	}
	obj.DatabaseBinding = RuntimeReferenceBinding{ResourceVersion: "new", ContentDigest: "rotated"}
	if _, err := r.Reconcile(context.Background(), obj, true); err != nil || applies != 1 || probes != 1 || observations != 1 {
		t.Fatalf("reference rotation replayed: applies=%d probes=%d observations=%d err=%v", applies, probes, observations, err)
	}
}

func TestOperatorProbeHonorsPauseAndCurrentApproval(t *testing.T) {
	for _, scenario := range []string{"pause", "approval withdrawn"} {
		t.Run(scenario, func(t *testing.T) {
			obj := resource()
			r := Reconciler{Store: NewMemoryStore(), Apply: func(context.Context, Resource, string) (ApplyResult, error) { return ApplyResult{}, nil }}
			if _, err := r.Reconcile(context.Background(), obj, true); err != nil {
				t.Fatal(err)
			}
			r.Apply = func(context.Context, Resource, string) (ApplyResult, error) {
				t.Fatal("paused/unapproved apply")
				return ApplyResult{}, nil
			}
			r.Probe = func(context.Context, Resource) executor.ExecutionStartCheck {
				t.Fatal("paused/unapproved database probe")
				return executor.ExecutionStartCheck{}
			}
			r.Observe = func(context.Context, Resource) CompletionResult {
				t.Fatal("paused/unapproved observation")
				return CompletionResult{}
			}
			obj.Spec.Suspend = scenario == "pause"
			status, err := r.Reconcile(context.Background(), obj, false)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "approval withdrawn" && status.Conditions[len(status.Conditions)-1].Type != Approval {
				t.Fatalf("cached Ready bypassed approval: %+v", status)
			}
			obj.Spec.Suspend = false
			status, err = r.Reconcile(context.Background(), obj, true)
			if err != nil || status.Conditions[len(status.Conditions)-1].Type != Ready {
				t.Fatalf("checkpoint did not resume: %+v %v", status, err)
			}
		})
	}
}

func TestOperatorProbeResumePreservesRecoveredAuthorizationBoundary(t *testing.T) {
	obj := resource()
	obj.CompletionRequired = true
	now := time.Now().UTC()
	r := Reconciler{Store: NewMemoryStore(), Apply: func(context.Context, Resource, string) (ApplyResult, error) {
		t.Fatal("resume replayed Apply")
		return ApplyResult{}, nil
	}, Observe: func(_ context.Context, obj Resource) CompletionResult { return provenCompletion(obj, now) }}
	if _, err := r.Reconcile(context.Background(), obj, true); err != nil {
		t.Fatal(err)
	}
	obj.CompletionRequired = false
	obj.Spec.Suspend = true
	if _, err := r.Reconcile(context.Background(), obj, true); err != nil {
		t.Fatal(err)
	}
	obj.Spec.Suspend = false
	status, err := r.Reconcile(context.Background(), obj, true)
	if err != nil || status.Conditions[len(status.Conditions)-1].Reason != "CompletionRecovered" || status.CompletionObservedAt.IsZero() {
		t.Fatalf("resume bypassed recovered publication checks: %+v %v", status, err)
	}
}

func TestOperatorProbeCannotTreatUncertaintyAsUnstarted(t *testing.T) {
	obj := resource()
	store := NewMemoryStore()
	if err := store.Save(obj.Name, Record{ApplyingKey: applyKey(obj), Status: Status{RecoveryState: "uncertain"}}); err != nil {
		t.Fatal(err)
	}
	r := Reconciler{Store: store, Probe: func(context.Context, Resource) executor.ExecutionStartCheck {
		t.Fatal("known uncertainty reached fresh-start probe")
		return executor.ExecutionStartCheck{}
	}, Observe: func(context.Context, Resource) CompletionResult {
		return CompletionResult{Evidence: executor.CompletionEvidence{Outcome: executor.CompletionRefused, Reason: "history_invalid"}}
	}, Apply: func(context.Context, Resource, string) (ApplyResult, error) {
		t.Fatal("uncertainty replayed")
		return ApplyResult{}, nil
	}}
	if _, err := r.Reconcile(context.Background(), obj, true); err == nil {
		t.Fatal("uncertainty accepted")
	}
}
