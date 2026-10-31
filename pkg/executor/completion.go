package executor

import (
	"context"
	"errors"
	"strings"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/plan"

	"github.com/jackc/pgx/v5/pgconn"
)

type CompletionOutcome string

const (
	CompletionProven     CompletionOutcome = "proven"
	CompletionIneligible CompletionOutcome = "ineligible"
	CompletionRefused    CompletionOutcome = "refused"
	CompletionRetryable  CompletionOutcome = "retryable"
)

// CompletionEvidence describes observed execution, never permission to execute.
// Identity fields are populated only for a proven result. ObservedAt is not the
// original execution time. Reason is a bounded code, not a database error.
type CompletionEvidence struct {
	Outcome                                                              CompletionOutcome
	Reason                                                               string
	ArtifactDigest, PlanDigest, TargetIdentity, Environment, Fingerprint string
	Attempt, ConfirmedSteps                                              int
	ObservedAt                                                           time.Time
}

// CompletionReader deliberately has no execution or history repair method.
// Its owner must hold target coordination and a bounded read-only transaction
// through the entire observation; State must inspect the managed database.
type CompletionReader interface {
	Query(context.Context, string, ...any) (Rows, error)
	State(context.Context) (RuntimeState, error)
}

type LifecycleReader interface {
	ReadExecution(context.Context, string) ([]LifecycleEvent, error)
}

type historyBinding struct {
	ID, Hash, PhaseID, Mode, ExecutionID, Target, PlanDigest, BundleDigest string
}

func (b historyBinding) matches(a artifact.Artifact, s plan.Step, p plan.Phase) bool {
	return b.matchesStep(s, p) && b.matchesExecution(a)
}

func (b historyBinding) matchesStep(s plan.Step, p plan.Phase) bool {
	return s.ID != "" && p.ID != "" && s.Kind != plan.StepTopology && b.ID == s.ID && b.Hash == stepHash(s) && b.PhaseID == p.ID && b.Mode == string(p.Transaction)
}

func (b historyBinding) matchesExecution(a artifact.Artifact) bool {
	return b.ExecutionID == a.Digest && b.Target == a.DatabaseIdentity+"/"+a.TargetEnvironment && b.PlanDigest == a.Plan.Digest && b.BundleDigest == a.GuardrailDigest
}

func historyBindings(a artifact.Artifact) (map[string]plan.Step, map[string]plan.Phase) {
	steps, phases := map[string]plan.Step{}, map[string]plan.Phase{}
	for _, s := range a.Plan.Steps {
		steps[s.ID] = s
	}
	for _, p := range a.Plan.Phases {
		for _, id := range p.StepIDs {
			phases[id] = p
		}
	}
	return steps, phases
}

func completionFailure(outcome CompletionOutcome, reason string) CompletionEvidence {
	return CompletionEvidence{Outcome: outcome, Reason: reason}
}

// ObserveCompletion is for ordinary declarative resources only. The caller
// must also validate current resource kind, external approval policy, and
// release/reference identity before using a proven result for status recovery.
func ObserveCompletion(ctx context.Context, verified artifact.VerifiedObservation, reader CompletionReader, audit LifecycleReader, now time.Time) CompletionEvidence {
	a, err := verified.Payload()
	if err != nil || now.IsZero() || now.Before(a.CreatedAt) {
		return completionFailure(CompletionRefused, "authorization_invalid")
	}
	if !completionEligible(a) {
		return completionFailure(CompletionIneligible, "unsupported_plan")
	}
	if reader == nil || audit == nil {
		return completionFailure(CompletionRefused, "evidence_reader_missing")
	}
	attempt, failed := readCompletedAttempt(ctx, reader, a)
	if failed.Outcome != "" {
		return failed
	}
	if _, failed = readCompletionState(ctx, reader, a); failed.Outcome != "" {
		return failed
	}
	if failed = readCompletionAudit(ctx, audit, a, attempt, now); failed.Outcome != "" {
		return failed
	}
	// The second fresh inspection catches changes crossing the audit read.
	state, failed := readCompletionState(ctx, reader, a)
	if failed.Outcome != "" {
		return failed
	}
	// The target lock coordinates AutoSQL writers, but a manual history edit
	// may cross the audit read. Never publish evidence from that earlier view.
	finalAttempt, failed := readCompletedAttempt(ctx, reader, a)
	if failed.Outcome != "" {
		return failed
	}
	if finalAttempt != attempt {
		return completionFailure(CompletionRefused, "history_invalid")
	}
	return CompletionEvidence{Outcome: CompletionProven, Reason: "completion_proven", ArtifactDigest: a.Digest, PlanDigest: a.Plan.Digest, TargetIdentity: a.DatabaseIdentity, Environment: a.TargetEnvironment, Fingerprint: state.Fingerprint, Attempt: attempt, ConfirmedSteps: len(a.Plan.Steps), ObservedAt: now.UTC()}
}

func completionEligible(a artifact.Artifact) bool {
	if len(a.Plan.Steps) == 0 || a.Metadata["autosql.migration.manifest"] != "" {
		return false
	}
	if mode := a.Metadata["autosql.operator.mode"]; mode != "" && mode != "transition" {
		return false
	}
	for _, step := range a.Plan.Steps {
		if step.Kind != plan.StepExecutable {
			return false
		}
	}
	for _, phase := range a.Plan.Phases {
		if len(phase.StepIDs) == 0 {
			return false
		}
	}
	return true
}

func readCompletedAttempt(ctx context.Context, reader CompletionReader, a artifact.Artifact) (int, CompletionEvidence) {
	// Read every state and attempt. Filtering to confirmed rows could hide an
	// unresolved intent or a second execution attempt.
	rows, err := reader.Query(ctx, `select step_id,step_hash,phase_id,phase_mode,execution_id,target_identity,plan_digest,bundle_digest,attempt,state from autosql_migration_history where artifact_digest=$1`, a.Digest)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "42P01" || pgErr.Code == "42703") {
			return 0, completionFailure(CompletionIneligible, "history_missing_bindings")
		}
		return 0, completionFailure(CompletionRetryable, "history_unavailable")
	}
	defer rows.Close()
	steps, phases := historyBindings(a)
	seen, attempt := map[string]bool{}, int64(0)
	for rows.Next() {
		var b historyBinding
		var rowAttempt int64
		var state string
		if err := rows.Scan(&b.ID, &b.Hash, &b.PhaseID, &b.Mode, &b.ExecutionID, &b.Target, &b.PlanDigest, &b.BundleDigest, &rowAttempt, &state); err != nil {
			return 0, completionFailure(CompletionRefused, "history_invalid")
		}
		if !b.matches(a, steps[b.ID], phases[b.ID]) || state != "confirmed" || rowAttempt < 1 || seen[b.ID] || attempt != 0 && rowAttempt != attempt {
			return 0, completionFailure(CompletionRefused, "history_invalid")
		}
		seen[b.ID], attempt = true, rowAttempt
	}
	if rows.Err() != nil || ctx.Err() != nil {
		return 0, completionFailure(CompletionRetryable, "history_unavailable")
	}
	if len(seen) != len(steps) {
		return 0, completionFailure(CompletionRefused, "history_incomplete")
	}
	return int(attempt), CompletionEvidence{}
}

func completionStateMatches(a artifact.Artifact, state RuntimeState) bool {
	return strings.EqualFold(state.Fingerprint, a.Plan.ToFingerprint) && state.SourceRevision == a.SourceRevision && state.Environment == a.TargetEnvironment && state.DatabaseIdentity == a.DatabaseIdentity
}

func readCompletionState(ctx context.Context, reader CompletionReader, a artifact.Artifact) (RuntimeState, CompletionEvidence) {
	state, err := reader.State(ctx)
	if err != nil || ctx.Err() != nil {
		return RuntimeState{}, completionFailure(CompletionRetryable, "state_unavailable")
	}
	if !completionStateMatches(a, state) {
		return RuntimeState{}, completionFailure(CompletionRefused, "state_drifted")
	}
	return state, CompletionEvidence{}
}

func readCompletionAudit(ctx context.Context, audit LifecycleReader, a artifact.Artifact, attempt int, now time.Time) CompletionEvidence {
	events, err := audit.ReadExecution(ctx, a.Digest)
	if err != nil {
		if errors.Is(err, ErrAuditEvidence) {
			return completionFailure(CompletionRefused, "audit_invalid")
		}
		return completionFailure(CompletionRetryable, "audit_unavailable")
	}
	if !completionAuditMatches(a, attempt, events, now) {
		return completionFailure(CompletionRefused, "audit_incomplete")
	}
	return CompletionEvidence{}
}

func completionAuditMatches(a artifact.Artifact, attempt int, events []LifecycleEvent, now time.Time) bool {
	seen := map[string]bool{}
	completed := false
	for _, event := range events {
		if event.ArtifactDigest != a.Digest && event.ExecutionID != a.Digest {
			continue
		}
		if event.ArtifactDigest != a.Digest || event.ExecutionID != a.Digest || event.PlanDigest != a.Plan.Digest || event.BundleDigest != a.GuardrailDigest || event.Target != a.DatabaseIdentity+"/"+a.TargetEnvironment || event.Attempt != attempt || event.At.Before(a.CreatedAt) || event.At.After(now) {
			return false
		}
		if event.Type == "completion_observed" {
			continue // Observations are not execution evidence.
		}
		if event.EventID != lifecycleEventID(event) || seen[event.EventID] || !completionEventShape(a, event) {
			return false
		}
		seen[event.EventID] = true
		switch event.Type {
		case "uncertain", "rollback_failed", "transaction_failed", "transaction_rollback", "transaction_begin_failed":
			return false
		case "confirmed", "intent", "transaction_committed":
			if completed {
				return false
			}
		case "completed":
			if !requiredCompletionAudit(a, attempt, seen) {
				return false
			}
			completed = true
		}
	}
	return completed
}

func completionEventShape(a artifact.Artifact, event LifecycleEvent) bool {
	switch event.Type {
	case "requested", "lock_acquired", "transaction_started", "contended", "authorization_refused", "expiry_refused", "stale":
		return event.StepID == ""
	case "confirmed", "intent":
		for _, step := range a.Plan.Steps {
			if step.ID == event.StepID {
				return true
			}
		}
	case "transaction_committed":
		for _, phase := range a.Plan.Phases {
			if phase.Transaction == plan.TransactionRequired && phase.StepIDs[len(phase.StepIDs)-1] == event.StepID {
				return true
			}
		}
	case "completed":
		last := a.Plan.Phases[len(a.Plan.Phases)-1]
		return event.StepID == last.StepIDs[len(last.StepIDs)-1]
	}
	return false
}

func requiredCompletionAudit(a artifact.Artifact, attempt int, seen map[string]bool) bool {
	has := func(typ, step string) bool {
		return seen[lifecycleEventID(LifecycleEvent{ExecutionID: a.Digest, Attempt: attempt, Type: typ, StepID: step})]
	}
	if !has("requested", "") || !has("lock_acquired", "") {
		return false
	}
	for _, phase := range a.Plan.Phases {
		for _, id := range phase.StepIDs {
			if !has("confirmed", id) || phase.Transaction == plan.TransactionProhibited && !has("intent", id) {
				return false
			}
		}
		if phase.Transaction == plan.TransactionRequired && (!has("transaction_started", "") || !has("transaction_committed", phase.StepIDs[len(phase.StepIDs)-1])) {
			return false
		}
	}
	return true
}
