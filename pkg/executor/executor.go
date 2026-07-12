// Package executor applies verified migration artifacts through PostgreSQL.
package executor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/plan"
	"autosql/pkg/precheck"
)

var (
	ErrBusy      = errors.New("migration apply already in progress")
	ErrStale     = errors.New("migration source state is stale")
	ErrExpired   = errors.New("migration artifact expired")
	ErrReconcile = errors.New("migration has an intended but unconfirmed step; reconcile, explicitly skip, or sign a new plan")
	ErrPartial   = errors.New("migration partially applied")
)

type RuntimeState struct{ Fingerprint, SourceRevision, Environment, DatabaseIdentity string }
type StateReader func(context.Context, Session) (RuntimeState, error)
type Clock func() time.Time
type LifecycleEvent struct {
	Type, ExecutionID, Target, ArtifactDigest, PlanDigest, BundleDigest, StepID, Guidance string
	At                                                                                    time.Time
}
type LifecycleAudit interface {
	AppendDurable(context.Context, LifecycleEvent) error
}
type StepConfirmer func(context.Context, Session, plan.Step) error

type Config struct {
	URL         string
	State       StateReader
	Now         Clock
	Reauthorize func(context.Context, artifact.Artifact) error
	Confirm     StepConfirmer
	Audit       LifecycleAudit
	Connector   Connector
}

type Result struct {
	AppliedSteps                               int
	LastConfirmed                              string
	Partial                                    bool
	Uncertain                                  bool
	PendingStep, ExecutionID, RecoveryGuidance string
}

// PostgreSQL can be constructed only with a cryptographically verified artifact.
type PostgreSQL struct {
	config   Config
	artifact artifact.Artifact
	result   Result
}

func NewPostgreSQL(config Config, verified artifact.VerifiedArtifact) (*PostgreSQL, error) {
	payload, err := verified.Payload()
	if err != nil {
		return nil, err
	}
	if config.URL == "" || config.State == nil {
		return nil, errors.New("executor configuration invalid")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Connector == nil {
		config.Connector = PGXConnector{}
	}
	return &PostgreSQL{config: config, artifact: payload}, nil
}

func (e *PostgreSQL) Result() Result { return e.result }
func (e *PostgreSQL) audit(ctx context.Context, typ, step, guidance string) error {
	if e.config.Audit == nil {
		return nil
	}
	return e.config.Audit.AppendDurable(ctx, LifecycleEvent{Type: typ, ExecutionID: e.artifact.Digest, Target: e.artifact.DatabaseIdentity + "/" + e.artifact.TargetEnvironment, ArtifactDigest: e.artifact.Digest, PlanDigest: e.artifact.Plan.Digest, BundleDigest: e.artifact.GuardrailDigest, StepID: step, Guidance: guidance, At: e.config.Now().UTC()})
}

func (e *PostgreSQL) ApplyAuthorized(ctx context.Context, checks precheck.Plan) ([]precheck.Result, error) {
	e.result = Result{}
	if err := e.audit(ctx, "requested", "", ""); err != nil {
		return nil, errors.New("durable lifecycle audit failed")
	}
	conn, err := e.config.Connector.Connect(ctx, e.config.URL)
	if err != nil {
		return nil, errors.New("connect executor database")
	}
	defer conn.Close(context.WithoutCancel(ctx))
	var locked bool
	identity := fmt.Sprintf("%d:%s%d:%s", len(e.artifact.DatabaseIdentity), e.artifact.DatabaseIdentity, len(e.artifact.TargetEnvironment), e.artifact.TargetEnvironment)
	if err = conn.QueryRow(ctx, `select pg_try_advisory_lock(hashtextextended($1, 0))`, identity).Scan(&locked); err != nil || !locked {
		if err == nil {
			err = ErrBusy
		}
		if auditErr := e.audit(ctx, "contended", "", ""); auditErr != nil {
			return nil, errors.New("durable lifecycle audit failed")
		}
		return nil, err
	}
	defer conn.Exec(context.WithoutCancel(ctx), `select pg_advisory_unlock(hashtextextended($1, 0))`, identity)
	if err := e.audit(ctx, "lock_acquired", "", ""); err != nil {
		return nil, errors.New("durable lifecycle audit failed")
	}
	if e.config.Reauthorize != nil {
		if err := e.config.Reauthorize(ctx, e.artifact); err != nil {
			if auditErr := e.audit(ctx, "authorization_refused", "", ""); auditErr != nil {
				return nil, errors.New("durable lifecycle audit failed")
			}
			return nil, ErrExpired
		}
	}

	// Time and live state are intentionally checked only after the session lock.
	now := e.config.Now().UTC()
	if now.Before(e.artifact.CreatedAt) || !now.Before(e.artifact.ExpiresAt) {
		if auditErr := e.audit(ctx, "expiry_refused", "", ""); auditErr != nil {
			return nil, errors.New("durable lifecycle audit failed")
		}
		return nil, ErrExpired
	}
	state, err := e.config.State(ctx, conn)
	if err != nil {
		return nil, errors.New("inspect locked database state")
	}
	if strings.EqualFold(state.Fingerprint, e.artifact.Plan.ToFingerprint) && state.SourceRevision == e.artifact.SourceRevision && state.Environment == e.artifact.TargetEnvironment && state.DatabaseIdentity == e.artifact.DatabaseIdentity {
		return nil, nil
	}
	if !strings.EqualFold(state.Fingerprint, e.artifact.Plan.FromFingerprint) || state.SourceRevision != e.artifact.SourceRevision || state.Environment != e.artifact.TargetEnvironment || state.DatabaseIdentity != e.artifact.DatabaseIdentity {
		if auditErr := e.audit(ctx, "stale", "", ""); auditErr != nil {
			return nil, errors.New("durable lifecycle audit failed")
		}
		return nil, ErrStale
	}
	var results []precheck.Result
	if err = ensureHistory(ctx, conn); err != nil {
		return nil, err
	}
	if err = refuseUncertain(ctx, conn, e.artifact.Digest); err != nil {
		if errors.Is(err, ErrReconcile) {
			if auditErr := e.audit(ctx, "reconcile_refused", "", "reconcile pending execution before retry"); auditErr != nil {
				return nil, errors.New("durable lifecycle audit failed")
			}
		}
		return nil, err
	}
	confirmed, err := confirmedSteps(ctx, conn, e.artifact)
	if err != nil {
		return nil, err
	}
	steps := map[string]plan.Step{}
	for _, step := range e.artifact.Plan.Steps {
		steps[step.ID] = step
	}
	checksPending := true
	for _, phase := range e.artifact.Plan.Phases {
		if phase.Transaction == plan.TransactionRequired {
			var phaseChecks []precheck.Result
			phaseChecks, err = e.transactionalPhase(ctx, conn, phase, steps, confirmed, checks, checksPending)
			if checksPending {
				results = phaseChecks
				checksPending = false
			}
			if err != nil {
				return results, err
			}
		} else {
			if checksPending {
				results, err = runChecks(ctx, conn, checks)
				checksPending = false
				if err != nil {
					return results, err
				}
			}
			if err = e.nontransactionalPhase(ctx, conn, phase, steps, confirmed); err != nil {
				return results, err
			}
		}
	}
	return results, e.finish(ctx)
}
func (e *PostgreSQL) finish(ctx context.Context) error {
	if auditErr := e.audit(ctx, "completed", e.result.LastConfirmed, ""); auditErr != nil {
		if e.result.AppliedSteps > 0 {
			e.result.Partial = true
			e.result.ExecutionID = e.artifact.Digest
			e.result.RecoveryGuidance = "repair lifecycle audit before retry"
			return ErrPartial
		}
		return errors.New("durable lifecycle audit failed")
	}
	return nil
}

func confirmedSteps(ctx context.Context, conn Session, a artifact.Artifact) (map[string]bool, error) {
	rows, err := conn.Query(ctx, `select step_id,step_hash,phase_id,phase_mode,execution_id,target_identity,plan_digest,bundle_digest from autosql_migration_history where artifact_digest=$1 and state='confirmed'`, a.Digest)
	if err != nil {
		return nil, errors.New("read confirmed migration history")
	}
	defer rows.Close()
	out := map[string]bool{}
	steps := map[string]plan.Step{}
	phases := map[string]plan.Phase{}
	for _, s := range a.Plan.Steps {
		steps[s.ID] = s
	}
	for _, p := range a.Plan.Phases {
		for _, id := range p.StepIDs {
			phases[id] = p
		}
	}
	for rows.Next() {
		var id, hash, phaseID, mode, executionID, target, planDigest, bundleDigest string
		if err := rows.Scan(&id, &hash, &phaseID, &mode, &executionID, &target, &planDigest, &bundleDigest); err != nil {
			return nil, errors.New("read confirmed migration history")
		}
		s, ok := steps[id]
		p, pok := phases[id]
		if !ok || !pok || s.Kind == plan.StepTopology || hash != stepHash(s) || phaseID != p.ID || mode != string(p.Transaction) || executionID != a.Digest || target != a.DatabaseIdentity+"/"+a.TargetEnvironment || planDigest != a.Plan.Digest || bundleDigest != a.GuardrailDigest {
			return nil, errors.New("migration history binding conflict")
		}
		if out[id] {
			return nil, errors.New("duplicate confirmed migration history")
		}
		out[id] = true
	}
	return out, rows.Err()
}

const historyDDL = `create table if not exists autosql_migration_history (
 artifact_digest text not null, step_id text not null, step_hash text not null,
 execution_id text not null, target_identity text not null, plan_digest text not null, bundle_digest text not null,
 attempt integer not null, phase_id text not null, phase_mode text not null,
 state text not null, intended_at timestamptz, confirmed_at timestamptz,
 last_confirmed_step text not null default '', recovery_guidance text not null default '',
 primary key (artifact_digest, step_id, attempt))`

func ensureHistory(ctx context.Context, conn Session) error {
	_, err := conn.Exec(ctx, historyDDL)
	if err != nil {
		return errors.New("initialize migration history")
	}
	_, err = conn.Exec(ctx, `alter table autosql_migration_history add column if not exists execution_id text, add column if not exists target_identity text, add column if not exists plan_digest text, add column if not exists bundle_digest text`)
	if err != nil {
		return errors.New("upgrade migration history")
	}
	return nil
}

func refuseUncertain(ctx context.Context, conn Session, digest string) error {
	var exists bool
	err := conn.QueryRow(ctx, `select exists(select 1 from autosql_migration_history where artifact_digest=$1 and state='intended')`, digest).Scan(&exists)
	if err != nil {
		return errors.New("read migration recovery state")
	}
	if exists {
		return ErrReconcile
	}
	return nil
}

func runChecks(ctx context.Context, conn interface {
	QueryRow(context.Context, string, ...any) Row
}, p precheck.Plan) ([]precheck.Result, error) {
	var out []precheck.Result
	for _, a := range p.Assertions {
		checkCtx, cancel := context.WithCancel(ctx)
		if a.Timeout > 0 {
			checkCtx, cancel = context.WithTimeout(ctx, a.Timeout)
		}
		var observed int64
		err := conn.QueryRow(checkCtx, a.Query, a.Args...).Scan(&observed)
		cancel()
		if err != nil {
			return out, errors.New("migration precheck query failed")
		}
		r := precheck.Result{Name: a.Name, Observed: observed, MaxAllowed: a.MaxAllowed, Passed: observed <= a.MaxAllowed, Source: a.Source}
		out = append(out, r)
		if !r.Passed {
			return out, &precheck.Failure{Result: r}
		}
	}
	return out, nil
}

func (e *PostgreSQL) transactionalPhase(ctx context.Context, conn Session, phase plan.Phase, steps map[string]plan.Step, confirmed map[string]bool, checks precheck.Plan, runPrechecks bool) (results []precheck.Result, err error) {
	if auditErr := e.audit(ctx, "transaction_started", "", ""); auditErr != nil {
		return nil, errors.New("durable lifecycle audit failed")
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		if auditErr := e.audit(ctx, "transaction_begin_failed", "", ""); auditErr != nil {
			return nil, errors.Join(errors.New("begin migration phase"), errors.New("durable lifecycle audit failed"))
		}
		return nil, errors.New("begin migration phase")
	}
	committed := false
	commitAttempted := false
	defer func() {
		if err != nil && !committed && !commitAttempted {
			rollbackErr := tx.Rollback(context.WithoutCancel(ctx))
			if rollbackErr != nil {
				e.result.Uncertain = true
				e.result.ExecutionID = e.artifact.Digest
				e.result.RecoveryGuidance = "reconcile failed rollback before retry"
				auditErr := e.audit(context.WithoutCancel(ctx), "rollback_failed", "", e.result.RecoveryGuidance)
				if auditErr != nil {
					err = errors.Join(err, ErrReconcile, errors.New("rollback and lifecycle audit failed"))
				} else {
					err = errors.Join(err, ErrReconcile)
				}
			} else if auditErr := e.audit(context.WithoutCancel(ctx), "transaction_rollback", "", ""); auditErr != nil {
				err = errors.Join(err, errors.New("transaction rollback audit failed"))
			}
		}
	}()
	if runPrechecks {
		results, err = runChecks(ctx, tx, checks)
		if err != nil {
			if auditErr := e.audit(ctx, "transaction_failed", "", ""); auditErr != nil {
				return results, errors.Join(err, errors.New("durable lifecycle audit failed"))
			}
			return results, err
		}
	}
	pendingCount := 0
	last := e.result.LastConfirmed
	for _, id := range phase.StepIDs {
		if confirmed[id] {
			continue
		}
		step := steps[id]
		if step.Kind == plan.StepTopology {
			continue
		}
		if step.Kind == plan.StepExecutable {
			if _, err = tx.Exec(ctx, step.SQL); err != nil {
				if auditErr := e.audit(ctx, "transaction_failed", step.ID, ""); auditErr != nil {
					return results, errors.Join(errors.New("execute transactional migration step"), errors.New("durable lifecycle audit failed"))
				}
				return results, errors.New("execute transactional migration step")
			}
		}
		if err = insertHistory(ctx, tx, e.artifact, step, phase, "confirmed", last, ""); err != nil {
			if auditErr := e.audit(ctx, "transaction_failed", step.ID, ""); auditErr != nil {
				return results, errors.Join(err, errors.New("durable lifecycle audit failed"))
			}
			return results, err
		}
		pendingCount++
		last = step.ID
	}
	commitAttempted = true
	if err = tx.Commit(ctx); err != nil {
		e.result.Uncertain = true
		e.result.PendingStep = last
		e.result.ExecutionID = e.artifact.Digest
		e.result.RecoveryGuidance = "reconcile transaction outcome before retry"
		if auditErr := e.audit(ctx, "uncertain", last, e.result.RecoveryGuidance); auditErr != nil {
			return results, errors.Join(ErrReconcile, errors.New("durable lifecycle audit failed"))
		}
		return results, ErrReconcile
	}
	committed = true
	e.result.AppliedSteps += pendingCount
	e.result.LastConfirmed = last
	if auditErr := e.audit(ctx, "transaction_committed", last, ""); auditErr != nil {
		e.result.Partial = true
		e.result.ExecutionID = e.artifact.Digest
		e.result.RecoveryGuidance = "repair transaction commit audit before retry"
		return results, ErrPartial
	}
	for _, id := range phase.StepIDs {
		if s := steps[id]; s.Kind == plan.StepExecutable && !confirmed[id] {
			if auditErr := e.audit(ctx, "confirmed", id, ""); auditErr != nil {
				e.result.Partial = true
				e.result.ExecutionID = e.artifact.Digest
				e.result.RecoveryGuidance = "repair lifecycle audit before retry"
				return results, ErrPartial
			}
		}
	}
	return results, nil
}

func (e *PostgreSQL) nontransactionalPhase(ctx context.Context, conn Session, phase plan.Phase, steps map[string]plan.Step, confirmed map[string]bool) error {
	for _, id := range phase.StepIDs {
		if confirmed[id] {
			continue
		}
		step := steps[id]
		if step.Kind == plan.StepTopology {
			continue
		}
		if err := insertHistory(ctx, conn, e.artifact, step, phase, "intended", e.result.LastConfirmed, "reconcile, explicitly skip, or create a new signed plan"); err != nil {
			return err
		}
		if err := e.audit(ctx, "intent", step.ID, ""); err != nil {
			return errors.New("durable lifecycle audit failed")
		}
		if step.Kind == plan.StepExecutable {
			if _, err := conn.Exec(ctx, step.SQL); err != nil {
				e.result.PendingStep = step.ID
				e.result.ExecutionID = e.artifact.Digest
				e.result.RecoveryGuidance = "reconcile statement outcome before retry"
				e.result.Uncertain = true
				if auditErr := e.audit(ctx, "uncertain", step.ID, e.result.RecoveryGuidance); auditErr != nil {
					return errors.Join(ErrReconcile, errors.New("durable lifecycle audit failed"))
				}
				return ErrReconcile
			}
		}
		if e.config.Confirm != nil {
			if err := e.config.Confirm(ctx, conn, step); err != nil {
				e.result.Uncertain = true
				e.result.PendingStep = step.ID
				e.result.ExecutionID = e.artifact.Digest
				e.result.RecoveryGuidance = "reconcile postcondition before retry"
				if auditErr := e.audit(ctx, "uncertain", step.ID, e.result.RecoveryGuidance); auditErr != nil {
					return errors.Join(ErrReconcile, errors.New("durable lifecycle audit failed"))
				}
				return ErrReconcile
			}
		}
		tag, err := conn.Exec(ctx, `update autosql_migration_history set state='confirmed', confirmed_at=clock_timestamp(), last_confirmed_step=$4, recovery_guidance='' where artifact_digest=$1 and step_id=$2 and attempt=$3 and state='intended' and step_hash=$5 and phase_id=$6 and phase_mode=$7 and execution_id=$1 and target_identity=$8 and plan_digest=$9 and bundle_digest=$10`, e.artifact.Digest, step.ID, 1, step.ID, stepHash(step), phase.ID, phase.Transaction, e.artifact.DatabaseIdentity+"/"+e.artifact.TargetEnvironment, e.artifact.Plan.Digest, e.artifact.GuardrailDigest)
		if err != nil || tag.RowsAffected() != 1 {
			e.result.Uncertain = true
			e.result.PendingStep = step.ID
			e.result.ExecutionID = e.artifact.Digest
			e.result.RecoveryGuidance = "reconcile confirmation persistence before retry"
			if auditErr := e.audit(ctx, "uncertain", step.ID, e.result.RecoveryGuidance); auditErr != nil {
				return errors.Join(ErrReconcile, errors.New("durable lifecycle audit failed"))
			}
			return ErrReconcile
		}
		var state, hash string
		if err := conn.QueryRow(ctx, `select state,step_hash from autosql_migration_history where artifact_digest=$1 and step_id=$2 and attempt=1`, e.artifact.Digest, step.ID).Scan(&state, &hash); err != nil || state != "confirmed" || hash != stepHash(step) {
			e.result.Uncertain = true
			e.result.PendingStep = step.ID
			e.result.ExecutionID = e.artifact.Digest
			e.result.RecoveryGuidance = "reconcile confirmation readback"
			if auditErr := e.audit(ctx, "uncertain", step.ID, e.result.RecoveryGuidance); auditErr != nil {
				return errors.Join(ErrReconcile, errors.New("durable lifecycle audit failed"))
			}
			return ErrReconcile
		}
		e.result.AppliedSteps++
		e.result.LastConfirmed = step.ID
		if err := e.audit(ctx, "confirmed", step.ID, ""); err != nil {
			e.result.Partial = true
			e.result.ExecutionID = e.artifact.Digest
			e.result.RecoveryGuidance = "repair lifecycle audit before retry"
			return ErrPartial
		}
	}
	return nil
}

func insertHistory(ctx context.Context, x interface {
	Exec(context.Context, string, ...any) (Tag, error)
}, a artifact.Artifact, step plan.Step, phase plan.Phase, state, last, guidance string) error {
	target := a.DatabaseIdentity + "/" + a.TargetEnvironment
	_, err := x.Exec(ctx, `insert into autosql_migration_history(artifact_digest,step_id,step_hash,attempt,phase_id,phase_mode,state,intended_at,confirmed_at,last_confirmed_step,recovery_guidance,execution_id,target_identity,plan_digest,bundle_digest) values($1,$2,$3,1,$4,$5,$6,clock_timestamp(),case when $6='confirmed' then clock_timestamp() end,$7,$8,$1,$9,$10,$11)`, a.Digest, step.ID, stepHash(step), phase.ID, phase.Transaction, state, last, guidance, target, a.Plan.Digest, a.GuardrailDigest)
	if err != nil {
		return errors.New("write durable migration history")
	}
	return nil
}

func stepHash(step plan.Step) string {
	s := sha256.Sum256([]byte(step.ID + "\x00" + step.SQL + "\x00" + string(step.Transaction)))
	return "sha256:" + hex.EncodeToString(s[:])
}
