package operator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"autosql/pkg/bootstrap"
	"autosql/pkg/executor"
)

func provenCompletion(obj Resource, now time.Time) CompletionResult {
	return CompletionResult{Evidence: executor.CompletionEvidence{Outcome: executor.CompletionProven, ArtifactDigest: obj.Spec.ArtifactDigest, PlanDigest: "plan", TargetIdentity: "database", Environment: "test", Fingerprint: "managed-fingerprint", Attempt: 1, ConfirmedSteps: 3, ObservedAt: now}, AuthorizationExpiresAt: now.Add(time.Hour)}
}

func TestOperatorCompletionRecoversFailedCheckpointWithoutReplay(t *testing.T) {
	now := time.Now().UTC()
	obj := resource()
	obj.MetadataGeneration = 7
	store := &failingCheckpointStore{Store: NewMemoryStore(), failAt: 2}
	applyCalls, observations := 0, 0
	r := Reconciler{Store: store, Now: func() time.Time { return now }, Apply: func(context.Context, Resource, string) (ApplyResult, error) {
		applyCalls++
		return ApplyResult{AppliedSteps: 3, PlanDigest: "plan"}, nil
	}, Observe: func(_ context.Context, observed Resource) CompletionResult {
		observations++
		return provenCompletion(observed, now)
	}}
	failed, err := r.Reconcile(context.Background(), obj, true)
	var persistence *StatePersistenceError
	if !errors.As(err, &persistence) || persistence.Stage != PersistencePostApply || failed.AppliedDigest != "" {
		t.Fatalf("post-apply failure: %+v %v", failed, err)
	}
	// Reconstructing the reconciler simulates losing process-local retry state.
	r = Reconciler{Store: store, Now: r.Now, Apply: r.Apply, Observe: r.Observe}
	status, err := r.Reconcile(context.Background(), obj, true)
	if err != nil || applyCalls != 1 || observations != 1 {
		t.Fatalf("recovery replayed: calls=%d observations=%d status=%+v err=%v", applyCalls, observations, status, err)
	}
	if status.AppliedDigest != obj.Spec.ArtifactDigest || status.ObservedGeneration != 7 || status.AppliedFingerprint != "managed-fingerprint" || status.AppliedSteps != 3 || status.ExecutionID != obj.Spec.ArtifactDigest || !status.CompletionObservedAt.Equal(now) || status.RecoveryState != "none" || status.RecoveryGuidance != "" {
		t.Fatalf("incomplete recovered checkpoint: %+v", status)
	}
	last := status.Conditions[len(status.Conditions)-1]
	if last.Type != Ready || last.Reason != "CompletionRecovered" || last.ObservedGeneration != 7 {
		t.Fatalf("recovered readiness: %+v", last)
	}
	if _, err = r.Reconcile(context.Background(), obj, true); err != nil || applyCalls != 1 || observations != 1 {
		t.Fatalf("unchanged successful checkpoint re-executed or re-observed: %v", err)
	}
}

func TestOperatorCompletionFailureNeverFallsThroughToApply(t *testing.T) {
	for _, test := range []struct {
		outcome      executor.CompletionOutcome
		reason, want string
	}{
		{executor.CompletionIneligible, "history_missing_bindings", "RecoveryEvidenceInvalid"},
		{executor.CompletionIneligible, "unsupported_plan", "ArtifactExpired"},
		{executor.CompletionRefused, "history_incomplete", "RecoveryEvidenceInvalid"},
		{executor.CompletionRefused, "state_drifted", "RecoveryStateDrifted"},
		{executor.CompletionRefused, "authorization_invalid", "ReleaseAuthorizationInvalid"},
		{executor.CompletionRetryable, "audit_unavailable", "RecoveryAuditUnavailable"},
		{executor.CompletionRetryable, "database_unavailable", "DependencyUnavailable"},
		{executor.CompletionRefused, "password=seeded-secret", "RecoveryEvidenceInvalid"},
	} {
		t.Run(test.reason, func(t *testing.T) {
			obj := resource()
			obj.CompletionRequired = true
			store := NewMemoryStore()
			prior := Status{ObservedGeneration: 1, AppliedDigest: "previous", AppliedFingerprint: "previous", RetryCount: 3}
			if err := store.Save(obj.Name, Record{Status: prior, AppliedKey: "previous"}); err != nil {
				t.Fatal(err)
			}
			r := Reconciler{Store: store, Apply: func(context.Context, Resource, string) (ApplyResult, error) {
				t.Fatal("observation failure reached Apply")
				return ApplyResult{}, nil
			}, Observe: func(context.Context, Resource) CompletionResult {
				return CompletionResult{Evidence: executor.CompletionEvidence{Outcome: test.outcome, Reason: test.reason}}
			}}
			status, err := r.Reconcile(context.Background(), obj, true)
			var failure *CompletionError
			if !errors.As(err, &failure) || failure.Reason != test.want || failure.Retryable != (test.outcome == executor.CompletionRetryable) {
				t.Fatalf("diagnostic: %+v %v", status, err)
			}
			if status.AppliedDigest != prior.AppliedDigest || status.ObservedGeneration != prior.ObservedGeneration || status.AppliedFingerprint != prior.AppliedFingerprint || status.RetryCount != 4 || strings.Contains(status.RecoveryGuidance, "seeded-secret") {
				t.Fatalf("unsafe failed observation status: %+v", status)
			}
		})
	}
}

func TestOperatorCompletionCheckpointMustPersistBeforeReady(t *testing.T) {
	now := time.Now().UTC()
	obj := resource()
	obj.CompletionRequired = true
	store := &failingCheckpointStore{Store: NewMemoryStore(), failAt: 1}
	observations := 0
	r := Reconciler{Store: store, Now: func() time.Time { return now }, Apply: func(context.Context, Resource, string) (ApplyResult, error) {
		t.Fatal("recovery reached Apply")
		return ApplyResult{}, nil
	}, Observe: func(_ context.Context, obj Resource) CompletionResult {
		observations++
		return provenCompletion(obj, now)
	}}
	status, err := r.Reconcile(context.Background(), obj, true)
	var persistence *StatePersistenceError
	if !errors.As(err, &persistence) || persistence.Stage != PersistenceRecovery || status.AppliedDigest != "" || !status.CompletionObservedAt.IsZero() || status.Conditions[len(status.Conditions)-1].Reason != "StatePersistenceFailed" {
		t.Fatalf("unpersisted recovery published success: %+v %v", status, err)
	}
	if _, ok := store.Load(obj.Name); ok {
		t.Fatal("failed recovery inserted a checkpoint")
	}
	if _, err := r.Reconcile(context.Background(), obj, true); err != nil || observations != 2 {
		t.Fatalf("recovery did not re-observe after failed persistence: count=%d err=%v", observations, err)
	}
}

func TestOperatorCompletionRejectsMalformedOrExpiredProof(t *testing.T) {
	for _, mutation := range []string{"digest", "future", "expiry", "attempt", "empty"} {
		t.Run(mutation, func(t *testing.T) {
			now := time.Now().UTC()
			obj := resource()
			obj.CompletionRequired = true
			r := Reconciler{Store: NewMemoryStore(), Now: func() time.Time { return now }, Apply: func(context.Context, Resource, string) (ApplyResult, error) {
				t.Fatal("invalid completion reached Apply")
				return ApplyResult{}, nil
			}, Observe: func(_ context.Context, obj Resource) CompletionResult {
				result := provenCompletion(obj, now)
				switch mutation {
				case "digest":
					result.Evidence.ArtifactDigest = "other"
				case "future":
					result.Evidence.ObservedAt = now.Add(time.Second)
				case "expiry":
					result.AuthorizationExpiresAt = now
				case "attempt":
					result.Evidence.Attempt = 0
				case "empty":
					result.Evidence.ConfirmedSteps = 0
				}
				return result
			}}
			status, err := r.Reconcile(context.Background(), obj, true)
			if err == nil || status.Conditions[len(status.Conditions)-1].Reason != "ReleaseAuthorizationInvalid" {
				t.Fatalf("accepted invalid proof: %+v %v", status, err)
			}
		})
	}
}

func TestOperatorCompletionSuspensionAndScope(t *testing.T) {
	for _, kind := range []string{"suspended", "versioned", "bootstrap", "adoption", "missing-observer"} {
		t.Run(kind, func(t *testing.T) {
			obj := resource()
			obj.CompletionRequired = true
			switch kind {
			case "suspended":
				obj.Spec.Suspend = true
			case "versioned":
				obj.Spec.Kind = Versioned
			case "bootstrap":
				obj.Spec.DatabaseTarget = &bootstrap.DatabaseTarget{}
			case "adoption":
				obj.Spec.AdoptionPolicy = AdoptIfEquivalent
			}
			r := Reconciler{Store: NewMemoryStore(), Apply: func(context.Context, Resource, string) (ApplyResult, error) {
				t.Fatal("unsupported completion reached Apply")
				return ApplyResult{}, nil
			}, Observe: func(context.Context, Resource) CompletionResult {
				t.Fatal("unsupported resource reached observation")
				return CompletionResult{}
			}}
			if kind == "missing-observer" {
				r.Observe = nil
			}
			status, err := r.Reconcile(context.Background(), obj, true)
			if kind == "suspended" {
				if err != nil || status.Conditions[len(status.Conditions)-1].Reason != "Suspended" {
					t.Fatalf("suspension: %+v %v", status, err)
				}
			} else if err == nil {
				t.Fatal("unsupported completion accepted")
			}
		})
	}
}

func TestOperatorCheckpointBindsResourceUID(t *testing.T) {
	first := resource()
	second := first
	first.UID, second.UID = "original", "replacement"
	if applyKey(first) == applyKey(second) {
		t.Fatal("replacement resource inherited old execution key")
	}
}

func TestOperatorCompletionExpiredCheckpointCannotReturnToApply(t *testing.T) {
	now := time.Now().UTC()
	obj := resource()
	obj.CompletionRequired = true
	observations := 0
	r := Reconciler{Store: NewMemoryStore(), Now: func() time.Time { return now }, Apply: func(context.Context, Resource, string) (ApplyResult, error) {
		t.Fatal("expired recovered checkpoint reached Apply")
		return ApplyResult{}, nil
	}, Observe: func(_ context.Context, obj Resource) CompletionResult {
		observations++
		if observations == 1 {
			return provenCompletion(obj, now)
		}
		return CompletionResult{Evidence: executor.CompletionEvidence{Outcome: executor.CompletionRefused, Reason: "authorization_invalid"}}
	}}
	if _, err := r.Reconcile(context.Background(), obj, true); err != nil {
		t.Fatal(err)
	}
	obj.CompletionRequired = false
	now = now.Add(2 * time.Hour)
	status, err := r.Reconcile(context.Background(), obj, true)
	if err == nil || observations != 2 || status.Conditions[len(status.Conditions)-1].Reason != "ReleaseAuthorizationInvalid" {
		t.Fatalf("expired completion reused: %+v %v", status, err)
	}
}
