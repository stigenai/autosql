package executor

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"
)

func TestExecutionStartAuditRefusesExecutionAndAmbiguity(t *testing.T) {
	f := newCompletionFixture(t)
	for _, typ := range []string{"requested", "lock_acquired", "contended", "authorization_refused", "expiry_refused", "stale", "transaction_begin_failed", "transaction_started", "transaction_committed", "confirmed", "intent", "completed", "completion_observed", "unknown"} {
		t.Run(typ, func(t *testing.T) {
			event := f.events[0]
			event.Type = typ
			event.EventID = lifecycleEventID(event)
			got := executionAuditStarted(context.Background(), completionAudit{events: []LifecycleEvent{event}}, f.a, false)
			want := typ == "transaction_started" || typ == "transaction_committed" || typ == "confirmed" || typ == "intent" || typ == "completed" || typ == "completion_observed" || typ == "unknown"
			if got.Started != want || got.Failure.Outcome != "" {
				t.Fatalf("start classification: %+v", got)
			}
		})
	}
	for _, mutate := range []func(*LifecycleEvent){func(e *LifecycleEvent) { e.Target = "other" }, func(e *LifecycleEvent) { e.Attempt = 2 }, func(e *LifecycleEvent) { e.At = time.Now().Add(time.Hour) }, func(e *LifecycleEvent) { e.EventID = "changed" }} {
		event := f.events[0]
		mutate(&event)
		if !executionAuditStarted(context.Background(), completionAudit{events: []LifecycleEvent{event}}, f.a, false).Started {
			t.Fatal("ambiguous event classified as unstarted")
		}
	}
	for _, err := range []error{ErrAuditEvidence, context.DeadlineExceeded} {
		if got := executionAuditStarted(context.Background(), completionAudit{err: err}, f.a, false); got.Failure.Outcome == "" {
			t.Fatalf("audit error ignored: %v", err)
		}
	}
	if got := executionAuditStarted(context.Background(), completionAudit{err: ErrAuditMissing}, f.a, false); got.Started || got.Failure.Outcome != "" {
		t.Fatalf("fresh absent audit refused: %+v", got)
	}
}

func TestExecutionStartLiveFreshAndCompleted(t *testing.T) {
	for _, applied := range []bool{false, true} {
		t.Run(map[bool]string{false: "unstarted", true: "completed"}[applied], func(t *testing.T) {
			f := newLiveCompletionFixtureWithApply(t, applied)
			beforeFP, beforeHistory, beforeAudit := completionSnapshot(t, f)
			got := CheckPostgreSQLExecutionStart(context.Background(), f.executable, f.config)
			if got.Started != applied || got.Failure.Outcome != "" {
				t.Fatalf("start check: %+v", got)
			}
			afterFP, afterHistory, afterAudit := completionSnapshot(t, f)
			if beforeFP != afterFP || beforeHistory != afterHistory || string(beforeAudit) != string(afterAudit) {
				t.Fatal("start check modified evidence")
			}
			if !applied {
				if _, err := os.Stat(f.audit.Path); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("start check created audit")
				}
			}
			assertCompletionLockAvailable(t, f)
		})
	}
}

func TestExecutionStartLiveDoesNotReplayMissingProofOrUncertainAttempt(t *testing.T) {
	for _, scenario := range []string{"completed without proof", "original state with history", "original state with started audit", "damaged audit", "canceled"} {
		t.Run(scenario, func(t *testing.T) {
			f := newLiveCompletionFixtureWithApply(t, scenario != "damaged audit" && scenario != "canceled")
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "completed without proof":
				if _, err := f.conn.Exec(ctx, `drop table autosql_migration_history`); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(f.audit.Path); err != nil {
					t.Fatal(err)
				}
			case "original state with history", "original state with started audit":
				if _, err := f.conn.Exec(ctx, `drop schema app cascade`); err != nil {
					t.Fatal(err)
				}
				if scenario == "original state with started audit" {
					if _, err := f.conn.Exec(ctx, `drop table autosql_migration_history`); err != nil {
						t.Fatal(err)
					}
				}
			case "damaged audit":
				if err := os.WriteFile(f.audit.Path, []byte("{}\n"), 0600); err != nil {
					t.Fatal(err)
				}
			case "canceled":
				cancel()
			}
			got := CheckPostgreSQLExecutionStart(ctx, f.executable, f.config)
			if !got.Started && got.Failure.Outcome == "" {
				t.Fatalf("unsafe execution retry: %+v", got)
			}
			assertCompletionLockAvailable(t, f)
		})
	}
}

func TestExecutionStartRequiresAuditAfterPossibleAttempt(t *testing.T) {
	f := newCompletionFixture(t)
	for _, audit := range []completionAudit{{err: ErrAuditMissing}, {}} {
		result := executionAuditStarted(context.Background(), audit, f.a, true)
		if result.Failure.Outcome != CompletionRefused {
			t.Fatalf("missing possible-attempt audit accepted: %+v", result)
		}
	}
	result := executionAuditStarted(context.Background(), completionAudit{events: f.events[:2]}, f.a, true)
	if result.Started || result.Failure.Outcome != "" {
		t.Fatalf("proven pre-execution attempt not retryable: %+v", result)
	}
}

func TestExecutionStartLiveInterruptedBeforeExecutionNeedsAudit(t *testing.T) {
	f := newLiveCompletionFixtureWithApply(t, false)
	f.config.RequireExistingAudit = true
	check := CheckPostgreSQLExecutionStart(context.Background(), f.executable, f.config)
	if check.Failure.Outcome != CompletionRefused {
		t.Fatalf("possible attempt with missing audit treated as fresh: %+v", check)
	}
	event := LifecycleEvent{Type: "requested", ExecutionID: f.a.Digest, ArtifactDigest: f.a.Digest, PlanDigest: f.a.Plan.Digest, BundleDigest: f.a.GuardrailDigest, Target: f.a.DatabaseIdentity + "/" + f.a.TargetEnvironment, Attempt: 1, At: time.Now().UTC()}
	event.EventID = lifecycleEventID(event)
	if err := f.audit.AppendDurable(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	check = CheckPostgreSQLExecutionStart(context.Background(), f.executable, f.config)
	if check.Started || check.Failure.Outcome != "" {
		t.Fatalf("intact pre-execution evidence refused retry: %+v", check)
	}
	assertCompletionLockAvailable(t, f)
}
