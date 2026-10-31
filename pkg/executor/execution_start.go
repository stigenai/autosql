package executor

import (
	"context"
	"errors"
	"strings"
	"time"

	"autosql/pkg/artifact"
	"github.com/jackc/pgx/v5"
)

// ExecutionStartCheck is routing evidence, not execution authorization or proof
// of completion. Started work must enter the separate completion observer.
// An unstarted result still requires every normal execution check at apply time.
type ExecutionStartCheck struct {
	Started bool
	Failure CompletionEvidence
}

// CheckPostgreSQLExecutionStart uses the same bounded read-only transaction and
// target coordination as completion observation. No history or audit is created.
func CheckPostgreSQLExecutionStart(ctx context.Context, verified artifact.VerifiedArtifact, config PostgreSQLCompletionConfig) ExecutionStartCheck {
	a, err := verified.Payload()
	if err != nil || a.DatabaseIdentity != config.DatabaseIdentity || a.TargetEnvironment != config.Environment || a.SourceRevision != config.SourceRevision {
		return ExecutionStartCheck{Failure: completionFailure(CompletionRefused, "authorization_invalid")}
	}
	// Empty plans retain their normal no-op apply behavior; they cannot prove
	// completion of an executable change through the recovery path.
	if len(a.Plan.Steps) == 0 {
		return ExecutionStartCheck{}
	}
	if strings.TrimSpace(config.URL) == "" || len(config.Schemas) == 0 || config.Audit == nil {
		return ExecutionStartCheck{Failure: completionFailure(CompletionRefused, "evidence_reader_missing")}
	}
	ctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, config.URL)
	if err != nil {
		return ExecutionStartCheck{Failure: completionFailure(CompletionRetryable, "database_unavailable")}
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		_ = conn.Close(cleanup)
	}()
	var started bool
	failure := withCompletionTransaction(ctx, conn, a, config, func(reader *postgresCompletionReader) CompletionEvidence {
		result := checkExecutionStart(ctx, reader, a)
		started = result.Started
		return result.Failure
	})
	return ExecutionStartCheck{Started: started, Failure: failure}
}

func checkExecutionStart(ctx context.Context, reader *postgresCompletionReader, a artifact.Artifact) ExecutionStartCheck {
	// A desired-state match is never a fresh execution, even when all local
	// checkpoints and Kubernetes status have disappeared. Other drift also
	// requires observation/refusal, never a blind apply.
	for pass := 0; pass < 2; pass++ {
		state, err := reader.State(ctx)
		if err != nil {
			return ExecutionStartCheck{Failure: completionFailure(CompletionRetryable, "state_unavailable")}
		}
		if !strings.EqualFold(state.Fingerprint, a.Plan.FromFingerprint) || strings.EqualFold(state.Fingerprint, a.Plan.ToFingerprint) {
			return ExecutionStartCheck{Started: true}
		}
		present, err := executionHistoryPresent(ctx, reader, a.Digest)
		if err != nil {
			return ExecutionStartCheck{Failure: completionFailure(CompletionRetryable, "history_unavailable")}
		}
		if present {
			return ExecutionStartCheck{Started: true}
		}
		if pass == 0 {
			result := executionAuditStarted(ctx, reader.config.Audit, a, reader.config.RequireExistingAudit)
			if result.Started || result.Failure.Outcome != "" {
				return result
			}
		}
	}
	return ExecutionStartCheck{}
}

func executionHistoryPresent(ctx context.Context, reader *postgresCompletionReader, digest string) (bool, error) {
	var exists bool
	if err := reader.tx.QueryRow(ctx, `select to_regclass('autosql_migration_history') is not null`).Scan(&exists); err != nil || !exists {
		return false, err
	}
	// Any state or attempt is evidence of execution; do not hide legacy,
	// partial, or uncertain records by filtering to confirmed rows.
	var present bool
	err := reader.tx.QueryRow(ctx, `select exists(select 1 from autosql_migration_history where artifact_digest=$1)`, digest).Scan(&present)
	return present, err
}

func executionAuditStarted(ctx context.Context, audit LifecycleReader, a artifact.Artifact, requireExisting bool) ExecutionStartCheck {
	events, err := audit.ReadExecution(ctx, a.Digest)
	if errors.Is(err, ErrAuditMissing) && !requireExisting {
		return ExecutionStartCheck{}
	}
	if err != nil {
		outcome := CompletionRetryable
		if errors.Is(err, ErrAuditEvidence) {
			outcome = CompletionRefused
		}
		return ExecutionStartCheck{Failure: completionFailure(outcome, "audit_unavailable")}
	}
	if requireExisting && len(events) == 0 {
		return ExecutionStartCheck{Failure: completionFailure(CompletionRefused, "audit_incomplete")}
	}
	for _, event := range events {
		if !unstartedAuditEvent(a, event, time.Now().UTC()) {
			return ExecutionStartCheck{Started: true}
		}
	}
	return ExecutionStartCheck{}
}

func unstartedAuditEvent(a artifact.Artifact, event LifecycleEvent, now time.Time) bool {
	if event.ArtifactDigest != a.Digest || event.ExecutionID != a.Digest || event.PlanDigest != a.Plan.Digest || event.BundleDigest != a.GuardrailDigest || event.Target != a.DatabaseIdentity+"/"+a.TargetEnvironment || event.Attempt != 1 || event.StepID != "" || event.EventID != lifecycleEventID(event) || event.At.Before(a.CreatedAt) || event.At.After(now) {
		return false
	}
	switch event.Type {
	case "requested", "lock_acquired", "contended", "authorization_refused", "expiry_refused", "stale", "transaction_begin_failed":
		return true
	default:
		return false
	}
}
