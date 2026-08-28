package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"autosql/pkg/bootstrap"
	"autosql/pkg/plan"
	"autosql/pkg/schema"

	"github.com/jackc/pgx/v5"
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

var (
	ErrBootstrapCollision = errors.New("bootstrap target exists without matching AutoSQL execution state")
	ErrBootstrapIdentity  = errors.New("bootstrap execution identity does not match the requested plan and target")
	ErrBootstrapReconcile = errors.New("bootstrap has an intended but unconfirmed non-transactional step")
)

type BootstrapExecutionEvent struct {
	Type       string    `json:"type"`
	PlanDigest string    `json:"plan_digest"`
	PhaseID    string    `json:"phase_id,omitempty"`
	Checkpoint string    `json:"checkpoint,omitempty"`
	StepID     string    `json:"step_id,omitempty"`
	At         time.Time `json:"at"`
}

type BootstrapExecutionHooks struct {
	BeforePhase func(context.Context, bootstrap.BootstrapPhase) error
	AfterStep   func(context.Context, bootstrap.BootstrapStep) error
	Confirm     func(context.Context, *pgx.Conn, plan.Step) error
	Now         func() time.Time
}

type BootstrapExecutionResult struct {
	PlanDigest       string                    `json:"plan_digest"`
	CreatedDatabase  bool                      `json:"created_database"`
	Resumed          bool                      `json:"resumed"`
	Completed        bool                      `json:"completed"`
	AppliedSteps     int                       `json:"applied_steps"`
	LastPhaseID      string                    `json:"last_phase_id,omitempty"`
	LastCheckpoint   string                    `json:"last_checkpoint,omitempty"`
	LastConfirmed    string                    `json:"last_confirmed_step,omitempty"`
	PendingStep      string                    `json:"pending_step,omitempty"`
	RecoveryGuidance string                    `json:"recovery_guidance,omitempty"`
	Events           []BootstrapExecutionEvent `json:"events,omitempty"`
}

const bootstrapHistoryDDL = `create schema if not exists autosql_internal;
create table if not exists autosql_internal.bootstrap_execution (
 target_name text not null, plan_digest text not null, schema_plan_digest text not null,
 target_digest text not null, state text not null, phase_id text not null default '',
 checkpoint text not null default '', last_step_id text not null default '',
 started_at timestamptz not null default clock_timestamp(), updated_at timestamptz not null default clock_timestamp(),
 primary key(target_name,plan_digest)
);
create table if not exists autosql_internal.bootstrap_steps (
 plan_digest text not null, step_id text not null, step_hash text not null,
 phase_id text not null, checkpoint text not null, state text not null,
 intended_at timestamptz not null default clock_timestamp(), confirmed_at timestamptz,
 primary key(plan_digest,step_id)
)`

// ExecuteDatabaseBootstrapURL creates or verifies the target, reconnects, and
// applies every digest-bound phase with durable checkpoints. Errors are
// intentionally generic so URLs, SQL bodies, and hook details cannot escape.
func ExecuteDatabaseBootstrapURL(ctx context.Context, maintenanceURL string, whole bootstrap.Plan, hooks BootstrapExecutionHooks) (BootstrapExecutionResult, error) {
	result := BootstrapExecutionResult{PlanDigest: whole.Digest}
	if err := whole.Validate(); err != nil {
		return result, ErrBootstrapIdentity
	}
	if hooks.Now == nil {
		hooks.Now = time.Now
	}
	// Extension readiness is a read-only gate and must run before target
	// creation, bootstrap history initialization, or any schema mutation.
	extensionReport, err := PreflightBootstrapExtensionsURL(ctx, maintenanceURL, whole)
	if err != nil {
		return result, safeError("preflight bootstrap extensions", maintenanceURL, err)
	}
	if !extensionReport.Ready {
		return result, extensionReadinessError(extensionReport)
	}
	emit := func(typ string, phase bootstrap.BootstrapPhase, step string) {
		result.Events = append(result.Events, BootstrapExecutionEvent{Type: typ, PlanDigest: whole.Digest, PhaseID: phase.ID, Checkpoint: phase.Checkpoint, StepID: step, At: hooks.Now().UTC()})
	}

	conn, created, resumed, restoreConnections, err := openBootstrapTarget(ctx, maintenanceURL, whole)
	if err != nil {
		if errors.Is(err, ErrBootstrapCollision) || errors.Is(err, ErrBootstrapIdentity) {
			result.RecoveryGuidance = "verify the target name, then explicitly drop it or adopt it with a reviewed external target plan"
		}
		return result, err
	}
	defer conn.Close(context.WithoutCancel(ctx))
	if restoreConnections != nil {
		defer restoreConnections(context.WithoutCancel(ctx))
	}
	result.CreatedDatabase, result.Resumed = created, resumed
	if _, err := conn.Exec(ctx, bootstrapHistoryDDL); err != nil {
		return result, errors.New("initialize bootstrap history")
	}
	targetDigest, err := bootstrapTargetDigest(whole.Target)
	if err != nil {
		return result, ErrBootstrapIdentity
	}
	if err := bindBootstrapIdentity(ctx, conn, whole, targetDigest, resumed); err != nil {
		return result, err
	}
	lockKey := "autosql.bootstrap/v1:" + whole.Target.Name
	var locked bool
	if err := conn.QueryRow(ctx, `select pg_try_advisory_lock(hashtextextended($1,0))`, lockKey).Scan(&locked); err != nil {
		return result, errors.New("acquire bootstrap lock")
	}
	if !locked {
		return result, errors.New("bootstrap target is busy")
	}
	defer conn.Exec(context.WithoutCancel(ctx), `select pg_advisory_unlock(hashtextextended($1,0))`, lockKey)

	confirmed, intended, err := readBootstrapSteps(ctx, conn, whole)
	if err != nil {
		return result, err
	}
	if intended != "" {
		reconciled, reconcileErr := reconcileConcurrentIndexIntent(ctx, conn, whole, intended)
		if reconcileErr != nil {
			return result, reconcileErr
		}
		if reconciled {
			confirmed, intended, err = readBootstrapSteps(ctx, conn, whole)
			if err != nil {
				return result, err
			}
		}
	}
	if intended != "" {
		result.PendingStep = intended
		result.RecoveryGuidance = "inspect the non-transactional object, then explicitly confirm or remove the remnant before retry"
		return result, ErrBootstrapReconcile
	}
	if len(confirmed) == 0 {
		if err := verifyBootstrapState(ctx, conn, whole, false); err != nil {
			return result, err
		}
	}

	schemaSteps := map[string]plan.Step{}
	for _, step := range whole.SchemaPlan.Steps {
		schemaSteps[step.ID] = step
	}
	bootstrapSteps := map[string]bootstrap.BootstrapStep{}
	for _, step := range whole.Steps {
		bootstrapSteps[step.ID] = step
	}
	for _, phase := range whole.Phases {
		if phase.Stage == bootstrap.StageDatabaseTarget {
			continue
		}
		if phaseAlreadyConfirmed(phase, confirmed) {
			result.LastPhaseID, result.LastCheckpoint = phase.ID, phase.Checkpoint
			continue
		}
		if err := verifyBootstrapPhasePreconditions(ctx, conn, whole, phase, bootstrapSteps, confirmed); err != nil {
			return result, err
		}
		if hooks.BeforePhase != nil && hooks.BeforePhase(ctx, phase) != nil {
			return result, errors.New("bootstrap phase interrupted before execution")
		}
		emit("phase_started", phase, "")
		if phase.Transaction == plan.TransactionRequired {
			err = executeBootstrapTransactionalPhase(ctx, conn, whole, phase, bootstrapSteps, schemaSteps, confirmed, hooks, &result, emit)
		} else {
			err = executeBootstrapNonTransactionalPhase(ctx, conn, whole, phase, bootstrapSteps, schemaSteps, confirmed, hooks, &result, emit)
		}
		if err != nil {
			return result, err
		}
		result.LastPhaseID, result.LastCheckpoint = phase.ID, phase.Checkpoint
		emit("phase_confirmed", phase, result.LastConfirmed)
	}
	if err := verifyBootstrapState(ctx, conn, whole, true); err != nil {
		result.RecoveryGuidance = "inspect target drift before marking bootstrap complete"
		return result, err
	}
	if _, err := conn.Exec(ctx, `update autosql_internal.bootstrap_execution set state='completed',updated_at=clock_timestamp() where target_name=$1 and plan_digest=$2`, whole.Target.Name, whole.Digest); err != nil {
		return result, errors.New("persist bootstrap completion")
	}
	result.Completed = true
	emit("completed", bootstrap.BootstrapPhase{}, result.LastConfirmed)
	return result, nil
}

// reconcileConcurrentIndexIntent handles the two catalog states that can be
// proven safe after an interrupted CREATE INDEX CONCURRENTLY. An absent index
// proves the statement did not take effect and its intent may be retried. An
// exact, valid, ready index proves the statement completed and may be
// confirmed. Invalid or different remnants remain operator-reconciled.
func reconcileConcurrentIndexIntent(ctx context.Context, conn *pgx.Conn, whole bootstrap.Plan, stepID string) (bool, error) {
	var bound bootstrap.BootstrapStep
	for _, candidate := range whole.Steps {
		if candidate.ID == stepID {
			bound = candidate
			break
		}
	}
	if bound.ID == "" || bound.Transaction != plan.TransactionProhibited {
		return false, nil
	}
	var schemaStep plan.Step
	for _, candidate := range whole.SchemaPlan.Steps {
		if candidate.ID == bound.SchemaStepID {
			schemaStep = candidate
			break
		}
	}
	if schemaStep.ID == "" || schemaStep.Kind != plan.StepExecutable {
		return false, nil
	}
	var desired schema.Resource
	for _, change := range whole.SchemaPlan.Changes.Changes {
		if change.ID == schemaStep.ChangeID && change.Operation == schema.OperationCreate && change.After != nil && change.After.Kind == schema.KindIndex {
			desired = *change.After
			break
		}
	}
	if desired.ID == "" {
		return false, nil
	}
	expectedSQL, concurrent, err := canonicalConcurrentIndexSQL(schemaStep.SQL)
	if err != nil || !concurrent {
		return false, nil
	}
	var valid, ready bool
	var actualDefinition string
	err = conn.QueryRow(ctx, `select x.indisvalid,x.indisready,pg_get_indexdef(c.oid) from pg_class c join pg_namespace n on n.oid=c.relnamespace join pg_index x on x.indexrelid=c.oid where n.nspname=$1 and c.relname=$2`, desired.Name.Schema, desired.Name.Name).Scan(&valid, &ready, &actualDefinition)
	if errors.Is(err, pgx.ErrNoRows) {
		tag, deleteErr := conn.Exec(ctx, `delete from autosql_internal.bootstrap_steps where plan_digest=$1 and step_id=$2 and state='intended' and step_hash=$3`, whole.Digest, stepID, bootstrapStepHash(bound, whole.SchemaPlan))
		if deleteErr != nil || tag.RowsAffected() != 1 {
			return false, ErrBootstrapIdentity
		}
		return true, nil
	}
	if err != nil {
		return false, errors.New("inspect concurrent index intent")
	}
	actualSQL, _, err := canonicalConcurrentIndexSQL(actualDefinition)
	if err != nil || !valid || !ready || actualSQL != expectedSQL {
		return false, nil
	}
	tag, err := conn.Exec(ctx, `update autosql_internal.bootstrap_steps set state='confirmed',confirmed_at=clock_timestamp() where plan_digest=$1 and step_id=$2 and state='intended' and step_hash=$3`, whole.Digest, stepID, bootstrapStepHash(bound, whole.SchemaPlan))
	if err != nil || tag.RowsAffected() != 1 {
		return false, ErrBootstrapIdentity
	}
	return true, nil
}

func canonicalConcurrentIndexSQL(sql string) (string, bool, error) {
	parsed, err := pg_query.Parse(sql)
	if err != nil || len(parsed.GetStmts()) != 1 {
		return "", false, err
	}
	statement := parsed.GetStmts()[0].GetStmt().GetIndexStmt()
	if statement == nil {
		return "", false, nil
	}
	concurrent := statement.GetConcurrent()
	statement.Concurrent = false
	canonical, err := pg_query.Deparse(parsed)
	return canonical, concurrent, err
}

func openBootstrapTarget(ctx context.Context, maintenanceURL string, whole bootstrap.Plan) (*pgx.Conn, bool, bool, func(context.Context), error) {
	report, err := PreflightDatabaseTargetURL(ctx, maintenanceURL, whole.Target)
	if err != nil {
		return nil, false, false, nil, safeError("preflight bootstrap target", maintenanceURL, err)
	}
	if report.Supported {
		prepared, err := PrepareDatabaseURL(ctx, maintenanceURL, whole.Target)
		if err != nil {
			return nil, false, false, nil, safeError("prepare bootstrap target", maintenanceURL, err)
		}
		return prepared.Connection, prepared.Created, false, nil, nil
	}
	if whole.Target.Mode != bootstrap.ManagedDatabase || !report.Exists {
		return nil, false, false, nil, databaseTargetReportError(report)
	}
	// A managed-name collision is resumable only when the existing database
	// exactly matches the declared target and already contains matching state.
	external := whole.Target
	external.Mode = bootstrap.ExternalDatabase
	verification, err := PreflightDatabaseTargetURL(ctx, maintenanceURL, external)
	if err != nil || !verification.Supported {
		return nil, false, false, nil, ErrBootstrapIdentity
	}
	restore, err := temporarilyAllowBootstrapConnections(ctx, maintenanceURL, whole.Target)
	if err != nil {
		return nil, false, false, nil, err
	}
	conn, err := connectBootstrapTarget(ctx, maintenanceURL, whole.Target.Name)
	if err != nil {
		if restore != nil {
			restore(context.WithoutCancel(ctx))
		}
		return nil, false, false, nil, errors.New("connect existing bootstrap target")
	}
	var exists bool
	if err := conn.QueryRow(ctx, `select to_regclass('autosql_internal.bootstrap_execution') is not null`).Scan(&exists); err != nil || !exists {
		conn.Close(context.WithoutCancel(ctx))
		if restore != nil {
			restore(context.WithoutCancel(ctx))
		}
		return nil, false, false, nil, ErrBootstrapCollision
	}
	return conn, false, true, restore, nil
}

func temporarilyAllowBootstrapConnections(ctx context.Context, maintenanceURL string, target bootstrap.DatabaseTarget) (func(context.Context), error) {
	if target.AllowConnections {
		return nil, nil
	}
	conn, err := pgx.Connect(ctx, maintenanceURL)
	if err != nil {
		return nil, errors.New("connect maintenance database")
	}
	if _, err := conn.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{target.Name}.Sanitize()+" ALLOW_CONNECTIONS true"); err != nil {
		conn.Close(context.WithoutCancel(ctx))
		return nil, errors.New("open bootstrap target for resume")
	}
	return func(closeCtx context.Context) {
		_, _ = conn.Exec(closeCtx, "ALTER DATABASE "+pgx.Identifier{target.Name}.Sanitize()+" ALLOW_CONNECTIONS false")
		_ = conn.Close(closeCtx)
	}, nil
}

func connectBootstrapTarget(ctx context.Context, maintenanceURL, database string) (*pgx.Conn, error) {
	config, err := pgx.ParseConfig(maintenanceURL)
	if err != nil {
		return nil, err
	}
	config.Database = database
	return pgx.ConnectConfig(ctx, config)
}

func bootstrapTargetDigest(target bootstrap.DatabaseTarget) (string, error) {
	raw, err := json.Marshal(target.Normalize())
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(append([]byte("autosql.bootstrap-target/v1\x00"), raw...))
	return "sha256:" + hex.EncodeToString(sum[:]), nil
}

func bindBootstrapIdentity(ctx context.Context, conn *pgx.Conn, whole bootstrap.Plan, targetDigest string, _ bool) error {
	var schemaDigest, storedTarget string
	err := conn.QueryRow(ctx, `select schema_plan_digest,target_digest from autosql_internal.bootstrap_execution where target_name=$1 and plan_digest=$2`, whole.Target.Name, whole.Digest).Scan(&schemaDigest, &storedTarget)
	if err == nil {
		if schemaDigest != whole.SchemaPlan.Digest || storedTarget != targetDigest {
			return ErrBootstrapIdentity
		}
		return nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return ErrBootstrapIdentity
	}
	var incomplete bool
	if err := conn.QueryRow(ctx, `select exists(select 1 from autosql_internal.bootstrap_execution where target_name=$1 and state<>'completed')`, whole.Target.Name).Scan(&incomplete); err != nil || incomplete {
		return ErrBootstrapIdentity
	}
	_, err = conn.Exec(ctx, `insert into autosql_internal.bootstrap_execution(target_name,plan_digest,schema_plan_digest,target_digest,state) values($1,$2,$3,$4,'running')`, whole.Target.Name, whole.Digest, whole.SchemaPlan.Digest, targetDigest)
	if err != nil {
		return errors.New("bind bootstrap identity")
	}
	var planDigest string
	if err := conn.QueryRow(ctx, `select plan_digest,schema_plan_digest,target_digest from autosql_internal.bootstrap_execution where target_name=$1 and plan_digest=$2`, whole.Target.Name, whole.Digest).Scan(&planDigest, &schemaDigest, &storedTarget); err != nil || planDigest != whole.Digest || schemaDigest != whole.SchemaPlan.Digest || storedTarget != targetDigest {
		return ErrBootstrapIdentity
	}
	return nil
}

func readBootstrapSteps(ctx context.Context, conn *pgx.Conn, whole bootstrap.Plan) (map[string]bool, string, error) {
	rows, err := conn.Query(ctx, `select step_id,step_hash,phase_id,checkpoint,state from autosql_internal.bootstrap_steps where plan_digest=$1 order by intended_at,step_id`, whole.Digest)
	if err != nil {
		return nil, "", errors.New("read bootstrap history")
	}
	defer rows.Close()
	steps, phases := map[string]bootstrap.BootstrapStep{}, map[string]bootstrap.BootstrapPhase{}
	for _, step := range whole.Steps {
		steps[step.ID] = step
	}
	for _, phase := range whole.Phases {
		for _, id := range phase.StepIDs {
			phases[id] = phase
		}
	}
	confirmed := map[string]bool{}
	intended := ""
	for rows.Next() {
		var id, hash, phaseID, checkpoint, state string
		if err := rows.Scan(&id, &hash, &phaseID, &checkpoint, &state); err != nil {
			return nil, "", errors.New("read bootstrap history")
		}
		step, ok := steps[id]
		phase, phaseOK := phases[id]
		if !ok || !phaseOK || hash != bootstrapStepHash(step, whole.SchemaPlan) || phaseID != phase.ID || checkpoint != phase.Checkpoint || state != "confirmed" && state != "intended" {
			return nil, "", ErrBootstrapIdentity
		}
		if state == "intended" {
			intended = id
		} else {
			confirmed[id] = true
		}
	}
	return confirmed, intended, rows.Err()
}

func phaseAlreadyConfirmed(phase bootstrap.BootstrapPhase, confirmed map[string]bool) bool {
	for _, id := range phase.StepIDs {
		if !confirmed[id] {
			return false
		}
	}
	return true
}

func executeBootstrapTransactionalPhase(ctx context.Context, conn *pgx.Conn, whole bootstrap.Plan, phase bootstrap.BootstrapPhase, bootstrapSteps map[string]bootstrap.BootstrapStep, schemaSteps map[string]plan.Step, confirmed map[string]bool, hooks BootstrapExecutionHooks, result *BootstrapExecutionResult, emit func(string, bootstrap.BootstrapPhase, string)) error {
	tx, err := conn.Begin(ctx)
	if err != nil {
		return errors.New("begin bootstrap phase")
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback(context.WithoutCancel(ctx))
		}
	}()
	pending := 0
	last := result.LastConfirmed
	for _, id := range phase.StepIDs {
		if confirmed[id] {
			continue
		}
		bound, step := bootstrapSteps[id], schemaSteps[bootstrapSteps[id].SchemaStepID]
		if step.Kind == plan.StepExecutable {
			if _, err := tx.Exec(ctx, step.SQL); err != nil {
				return errors.New("execute transactional bootstrap step")
			}
		}
		if hooks.AfterStep != nil && hooks.AfterStep(ctx, bound) != nil {
			return errors.New("bootstrap phase interrupted after step")
		}
		if _, err := tx.Exec(ctx, `insert into autosql_internal.bootstrap_steps(plan_digest,step_id,step_hash,phase_id,checkpoint,state,confirmed_at) values($1,$2,$3,$4,$5,'confirmed',clock_timestamp())`, whole.Digest, id, bootstrapStepHash(bound, whole.SchemaPlan), phase.ID, phase.Checkpoint); err != nil {
			return errors.New("persist transactional bootstrap step")
		}
		pending++
		last = id
	}
	if _, err := tx.Exec(ctx, `update autosql_internal.bootstrap_execution set phase_id=$2,checkpoint=$3,last_step_id=$4,updated_at=clock_timestamp() where target_name=$1 and plan_digest=$5`, whole.Target.Name, phase.ID, phase.Checkpoint, last, whole.Digest); err != nil {
		return errors.New("persist bootstrap checkpoint")
	}
	if err := tx.Commit(ctx); err != nil {
		result.PendingStep, result.RecoveryGuidance = last, "reconcile the transaction outcome before retry"
		return ErrBootstrapReconcile
	}
	committed = true
	result.AppliedSteps += pending
	result.LastConfirmed = last
	for _, id := range phase.StepIDs {
		if !confirmed[id] {
			confirmed[id] = true
			emit("step_confirmed", phase, id)
		}
	}
	return nil
}

func executeBootstrapNonTransactionalPhase(ctx context.Context, conn *pgx.Conn, whole bootstrap.Plan, phase bootstrap.BootstrapPhase, bootstrapSteps map[string]bootstrap.BootstrapStep, schemaSteps map[string]plan.Step, confirmed map[string]bool, hooks BootstrapExecutionHooks, result *BootstrapExecutionResult, emit func(string, bootstrap.BootstrapPhase, string)) error {
	for _, id := range phase.StepIDs {
		if confirmed[id] {
			continue
		}
		bound, step := bootstrapSteps[id], schemaSteps[bootstrapSteps[id].SchemaStepID]
		if _, err := conn.Exec(ctx, `insert into autosql_internal.bootstrap_steps(plan_digest,step_id,step_hash,phase_id,checkpoint,state) values($1,$2,$3,$4,$5,'intended')`, whole.Digest, id, bootstrapStepHash(bound, whole.SchemaPlan), phase.ID, phase.Checkpoint); err != nil {
			return errors.New("persist bootstrap step intent")
		}
		emit("step_intended", phase, id)
		if step.Kind == plan.StepExecutable {
			if _, err := conn.Exec(ctx, step.SQL); err != nil {
				result.PendingStep, result.RecoveryGuidance = id, "inspect the non-transactional object and reconcile the intended step"
				return ErrBootstrapReconcile
			}
		}
		if hooks.AfterStep != nil && hooks.AfterStep(ctx, bound) != nil {
			result.PendingStep, result.RecoveryGuidance = id, "inspect the non-transactional object and reconcile the intended step"
			return ErrBootstrapReconcile
		}
		if hooks.Confirm != nil && hooks.Confirm(ctx, conn, step) != nil {
			result.PendingStep, result.RecoveryGuidance = id, "verify the non-transactional postcondition before retry"
			return ErrBootstrapReconcile
		}
		tag, err := conn.Exec(ctx, `update autosql_internal.bootstrap_steps set state='confirmed',confirmed_at=clock_timestamp() where plan_digest=$1 and step_id=$2 and state='intended' and step_hash=$3`, whole.Digest, id, bootstrapStepHash(bound, whole.SchemaPlan))
		if err != nil || tag.RowsAffected() != 1 {
			result.PendingStep, result.RecoveryGuidance = id, "reconcile bootstrap confirmation persistence"
			return ErrBootstrapReconcile
		}
		confirmed[id] = true
		result.AppliedSteps++
		result.LastConfirmed = id
		emit("step_confirmed", phase, id)
	}
	if _, err := conn.Exec(ctx, `update autosql_internal.bootstrap_execution set phase_id=$2,checkpoint=$3,last_step_id=$4,updated_at=clock_timestamp() where target_name=$1 and plan_digest=$5`, whole.Target.Name, phase.ID, phase.Checkpoint, result.LastConfirmed, whole.Digest); err != nil {
		return errors.New("persist bootstrap checkpoint")
	}
	return nil
}

func bootstrapStepHash(step bootstrap.BootstrapStep, schemaPlan plan.Plan) string {
	sql := ""
	for _, candidate := range schemaPlan.Steps {
		if candidate.ID == step.SchemaStepID {
			sql = candidate.SQL
			break
		}
	}
	sum := sha256.Sum256([]byte(step.ID + "\x00" + step.SchemaStepID + "\x00" + sql + "\x00" + string(step.Transaction)))
	return "sha256:" + hex.EncodeToString(sum[:])
}

func verifyBootstrapState(ctx context.Context, conn *pgx.Conn, whole bootstrap.Plan, final bool) error {
	advanced := false
	managedIDs := map[string]bool{}
	expected := map[string]schema.Resource{}
	forbidden := map[string]bool{}
	for _, resource := range whole.Preconditions {
		managedIDs[resource.ID] = true
		for _, dependency := range resource.Dependencies {
			managedIDs[dependency.Target] = true
		}
		expected[resource.ID] = resource
		switch resource.Kind {
		case schema.KindRole, schema.KindGrant, schema.KindMembership, schema.KindDefaultPrivilege:
			advanced = true
		}
	}
	allCreate := true
	for _, change := range whole.SchemaPlan.Changes.Changes {
		allCreate = allCreate && change.Operation == schema.OperationCreate
		for _, resource := range []*schema.Resource{change.Before, change.After} {
			if resource == nil {
				continue
			}
			managedIDs[resource.ID] = true
			for _, dependency := range resource.Dependencies {
				managedIDs[dependency.Target] = true
			}
			switch resource.Kind {
			case schema.KindRole, schema.KindGrant, schema.KindMembership, schema.KindDefaultPrivilege:
				advanced = true
			}
		}
		if final {
			if change.After != nil {
				expected[change.After.ID] = *change.After
			}
			if change.Before != nil && (change.After == nil || change.Before.ID != change.After.ID) {
				forbidden[change.Before.ID] = true
			}
		} else {
			if change.Before != nil {
				expected[change.Before.ID] = *change.Before
			}
			if change.After != nil && (change.Before == nil || change.Before.ID != change.After.ID) {
				forbidden[change.After.ID] = true
			}
		}
	}
	inspected, err := InspectConn(ctx, conn, Options{Advanced: advanced})
	if err != nil {
		return errors.New("inspect bootstrap precondition")
	}
	inspected, err = New().Normalize(ctx, inspected)
	if err != nil {
		return errors.New("normalize bootstrap precondition")
	}
	actualByID := map[string]schema.Resource{}
	for _, resource := range inspected.Graph.Resources {
		actualByID[resource.ID] = resource
	}
	desiredByID := make(map[string]schema.Resource, len(actualByID)+len(expected))
	for id, resource := range actualByID {
		desiredByID[id] = resource
	}
	for id, resource := range expected {
		desiredByID[id] = resource
	}
	var mismatches []string
	for id, desired := range expected {
		actual, exists := actualByID[id]
		if exists {
			actual = projectInspectedBootstrapResource(actual, desired, managedIDs, actualByID, desiredByID)
		}
		desiredFingerprint, _ := schema.ResourceFingerprint(desired)
		actualFingerprint, _ := schema.ResourceFingerprint(actual)
		if !exists || desiredFingerprint != actualFingerprint {
			mismatches = append(mismatches, id+"["+bootstrapResourceDifferenceFields(actual, desired)+"]")
		}
	}
	for id := range forbidden {
		if _, exists := actualByID[id]; exists {
			mismatches = append(mismatches, id+"[unexpected]")
		}
	}
	if len(mismatches) > 0 {
		sort.Strings(mismatches)
		return fmt.Errorf("%w: %d managed resource postconditions differ (first %s)", ErrBootstrapIdentity, len(mismatches), mismatches[0])
	}
	preconditionDocument, err := New().Normalize(ctx, schema.Document{
		Version: schema.SchemaVersion,
		Graph:   schema.Graph{Resources: append([]schema.Resource(nil), whole.Preconditions...)},
	})
	if err != nil {
		return fmt.Errorf("normalize bootstrap preconditions: %w", err)
	}
	preconditionFingerprint, err := schema.SemanticFingerprint(preconditionDocument)
	if err != nil {
		return fmt.Errorf("fingerprint bootstrap preconditions: %w", err)
	}
	if !allCreate || !strings.EqualFold(whole.SchemaPlan.FromFingerprint, preconditionFingerprint) {
		return nil
	}
	var filtered []schema.Resource
	for _, resource := range inspected.Graph.Resources {
		if desired, keep := expected[resource.ID]; keep {
			resource = projectInspectedBootstrapResource(resource, desired, managedIDs, actualByID, desiredByID)
			filtered = append(filtered, resource)
		}
	}
	inspected.Graph.Resources = filtered
	inspected.Normalize()
	fingerprint, err := schema.SemanticFingerprint(inspected)
	if err != nil {
		return fmt.Errorf("fingerprint bootstrap state: %w", err)
	}
	want := whole.SchemaPlan.FromFingerprint
	if final {
		want = whole.SchemaPlan.ToFingerprint
	}
	if !strings.EqualFold(fingerprint, want) {
		return fmt.Errorf("%w: state fingerprint %s does not match %s", ErrBootstrapIdentity, fingerprint, want)
	}
	return nil
}

func bootstrapResourceDifferenceFields(actual, desired schema.Resource) string {
	var fields []string
	if actual.ID != desired.ID || actual.Kind != desired.Kind || !reflect.DeepEqual(actual.Name, desired.Name) {
		fields = append(fields, "identity")
	}
	if !reflect.DeepEqual(actual.Dependencies, desired.Dependencies) {
		fields = append(fields, "dependencies")
	}
	if !reflect.DeepEqual(actual.Annotations, desired.Annotations) {
		fields = append(fields, "annotations")
	}
	var actualSpec, desiredSpec map[string]any
	if json.Unmarshal(actual.Spec, &actualSpec) != nil || json.Unmarshal(desired.Spec, &desiredSpec) != nil {
		fields = append(fields, "spec")
	} else {
		for key, desiredValue := range desiredSpec {
			if !reflect.DeepEqual(actualSpec[key], desiredValue) {
				field := "spec." + key
				if key == "definition" {
					actualText, actualOK := actualSpec[key].(string)
					desiredText, desiredOK := desiredValue.(string)
					if actualOK && desiredOK {
						difference := firstStringDifference(actualText, desiredText)
						actualByte, desiredByte := -1, -1
						if difference < len(actualText) {
							actualByte = int(actualText[difference])
						}
						if difference < len(desiredText) {
							desiredByte = int(desiredText[difference])
						}
						field += fmt.Sprintf("(length=%d/%d,first_difference=%d,bytes=%d/%d)", len(actualText), len(desiredText), difference, actualByte, desiredByte)
					}
				}
				fields = append(fields, field)
			}
		}
	}
	if len(fields) == 0 {
		fields = append(fields, "canonical")
	}
	sort.Strings(fields)
	return strings.Join(fields, ",")
}

func firstStringDifference(a, b string) int {
	limit := min(len(a), len(b))
	for index := 0; index < limit; index++ {
		if a[index] != b[index] {
			return index
		}
	}
	return limit
}

func projectInspectedBootstrapResource(actual, desired schema.Resource, desiredIDs map[string]bool, actualResources, desiredResources map[string]schema.Resource) schema.Resource {
	// Do not reuse the inspected resource's backing array here. Verification
	// projects the same snapshot both resource-by-resource and as a complete
	// document; an in-place filter would corrupt the latter projection's input.
	dependencies := make([]schema.Dependency, 0, len(actual.Dependencies))
	desiredDependencies := map[string]bool{}
	for _, dependency := range desired.Dependencies {
		desiredDependencies[dependency.Target+"\x00"+string(dependency.Type)] = true
	}
	for _, dependency := range actual.Dependencies {
		if desiredIDs[dependency.Target] && desiredDependencies[dependency.Target+"\x00"+string(dependency.Type)] {
			dependencies = append(dependencies, dependency)
		}
	}
	actual.Dependencies = dependencies
	inspectedAnnotations := actual.Annotations
	actual.Annotations = map[string]string{}
	for key := range desired.Annotations {
		if value, ok := inspectedAnnotations[key]; ok {
			actual.Annotations[key] = value
		}
	}
	if len(desired.Annotations) == 0 {
		actual.Annotations = nil
	}
	var actualSpec, desiredSpec map[string]any
	if json.Unmarshal(actual.Spec, &actualSpec) == nil && json.Unmarshal(desired.Spec, &desiredSpec) == nil {
		for key := range actualSpec {
			if _, specified := desiredSpec[key]; !specified {
				delete(actualSpec, key)
			}
		}
		if actual.Kind == schema.KindExtension {
			if desiredOwner, specified := desiredSpec["owner"].(string); specified && desiredOwner == "" {
				actualSpec["owner"] = desiredOwner
			}
		}
		if actual.Kind == schema.KindFunction || actual.Kind == schema.KindProcedure {
			actualDefinition, actualOK := actualSpec["definition"].(string)
			desiredDefinition, desiredOK := desiredSpec["definition"].(string)
			if actualOK && desiredOK {
				actualFingerprint, actualErr := pg_query.Fingerprint(actualDefinition)
				desiredFingerprint, desiredErr := pg_query.Fingerprint(desiredDefinition)
				if actualErr == nil && desiredErr == nil && actualFingerprint == desiredFingerprint {
					actualSpec["definition"] = desiredDefinition
					actualSpec["body_digest"] = desiredSpec["body_digest"]
				}
			}
		}
		if actual.Kind == schema.KindCheckConstraint && desired.Kind == schema.KindCheckConstraint {
			_, actualOK := actualSpec["definition"].(string)
			desiredDefinition, desiredOK := desiredSpec["definition"].(string)
			if actualOK && desiredOK && bootstrapCheckDefinitionsEquivalent(actual, desired, actualResources, desiredResources) {
				actualSpec["definition"] = desiredDefinition
			}
		}
		actual.Spec, _ = json.Marshal(actualSpec)
	}
	return actual
}

func bootstrapCheckDefinitionsEquivalent(actual, desired schema.Resource, actualResources, desiredResources map[string]schema.Resource) bool {
	_, actualDefinition, actualErr := renderConstraintCreate(actual, actualResources)
	_, desiredDefinition, desiredErr := renderConstraintCreate(desired, desiredResources)
	return actualErr == nil && desiredErr == nil && actualDefinition == desiredDefinition
}

func verifyBootstrapPhasePreconditions(ctx context.Context, conn *pgx.Conn, whole bootstrap.Plan, phase bootstrap.BootstrapPhase, steps map[string]bootstrap.BootstrapStep, confirmed map[string]bool) error {
	var planDigest, schemaDigest, state, storedPhase, checkpoint string
	if err := conn.QueryRow(ctx, `select plan_digest,schema_plan_digest,state,phase_id,checkpoint from autosql_internal.bootstrap_execution where target_name=$1 and plan_digest=$2`, whole.Target.Name, whole.Digest).Scan(&planDigest, &schemaDigest, &state, &storedPhase, &checkpoint); err != nil || planDigest != whole.Digest || schemaDigest != whole.SchemaPlan.Digest || state != "running" && state != "completed" {
		return ErrBootstrapIdentity
	}
	if storedPhase != "" {
		valid := false
		for _, candidate := range whole.Phases {
			if candidate.ID == storedPhase && candidate.Checkpoint == checkpoint && phaseAlreadyConfirmed(candidate, confirmed) {
				valid = true
				break
			}
		}
		if !valid {
			return ErrBootstrapIdentity
		}
	}
	earlierInPhase := map[string]bool{}
	for _, id := range phase.StepIDs {
		step, ok := steps[id]
		if !ok {
			return ErrBootstrapIdentity
		}
		if confirmed[id] {
			earlierInPhase[id] = true
			continue
		}
		for _, dependency := range step.DependsOn {
			dependencyStep := steps[dependency]
			if dependencyStep.Action == bootstrap.ActionPrepareDatabase {
				continue
			}
			if !confirmed[dependency] && !earlierInPhase[dependency] {
				return ErrBootstrapIdentity
			}
		}
		earlierInPhase[id] = true
	}
	return nil
}

// DiagnoseDatabaseBootstrapURL returns only digest-bound execution state and
// repair guidance; it never returns SQL, routine bodies, or connection data.
func DiagnoseDatabaseBootstrapURL(ctx context.Context, maintenanceURL string, whole bootstrap.Plan) (BootstrapExecutionResult, error) {
	result := BootstrapExecutionResult{PlanDigest: whole.Digest}
	if err := whole.Validate(); err != nil {
		return result, ErrBootstrapIdentity
	}
	conn, err := connectBootstrapTarget(ctx, maintenanceURL, whole.Target.Name)
	if err != nil {
		return result, errors.New("connect bootstrap target")
	}
	defer conn.Close(context.WithoutCancel(ctx))
	confirmed, intended, err := readBootstrapSteps(ctx, conn, whole)
	if err != nil {
		return result, err
	}
	result.Resumed = len(confirmed) > 0
	if intended != "" {
		result.PendingStep = intended
		result.RecoveryGuidance = "inspect the non-transactional object, then explicitly confirm or remove the remnant before retry"
	}
	ids := make([]string, 0, len(confirmed))
	for id := range confirmed {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	result.AppliedSteps = len(ids)
	if len(ids) > 0 {
		result.LastConfirmed = ids[len(ids)-1]
	}
	var state string
	if err := conn.QueryRow(ctx, `select state,phase_id,checkpoint,last_step_id from autosql_internal.bootstrap_execution where target_name=$1 and plan_digest=$2`, whole.Target.Name, whole.Digest).Scan(&state, &result.LastPhaseID, &result.LastCheckpoint, &result.LastConfirmed); err != nil {
		return result, ErrBootstrapIdentity
	}
	result.Completed = state == "completed"
	return result, nil
}

// ConfirmBootstrapStepURL is an explicit repair operation after an operator
// has verified the postcondition of an intended non-transactional step.
func ConfirmBootstrapStepURL(ctx context.Context, maintenanceURL string, whole bootstrap.Plan, stepID string) error {
	if err := whole.Validate(); err != nil {
		return ErrBootstrapIdentity
	}
	conn, err := connectBootstrapTarget(ctx, maintenanceURL, whole.Target.Name)
	if err != nil {
		return errors.New("connect bootstrap target")
	}
	defer conn.Close(context.WithoutCancel(ctx))
	var bound bootstrap.BootstrapStep
	for _, step := range whole.Steps {
		if step.ID == stepID {
			bound = step
			break
		}
	}
	if bound.ID == "" || bound.Transaction != plan.TransactionProhibited {
		return ErrBootstrapIdentity
	}
	tag, err := conn.Exec(ctx, `update autosql_internal.bootstrap_steps set state='confirmed',confirmed_at=clock_timestamp() where plan_digest=$1 and step_id=$2 and state='intended' and step_hash=$3`, whole.Digest, stepID, bootstrapStepHash(bound, whole.SchemaPlan))
	if err != nil || tag.RowsAffected() != 1 {
		return ErrBootstrapIdentity
	}
	return nil
}

// AbortDatabaseBootstrapURL is an explicit destructive cleanup. Managed
// targets require dropManaged=true and are dropped from the maintenance
// connection; external targets retain user objects and remove only the
// AutoSQL execution ledger after identity verification.
func AbortDatabaseBootstrapURL(ctx context.Context, maintenanceURL string, whole bootstrap.Plan, dropManaged bool) error {
	if err := whole.Validate(); err != nil {
		return ErrBootstrapIdentity
	}
	restore, err := temporarilyAllowBootstrapConnections(ctx, maintenanceURL, whole.Target)
	if err != nil {
		return err
	}
	if restore != nil {
		defer func() {
			if restore != nil {
				restore(context.WithoutCancel(ctx))
			}
		}()
	}
	conn, err := connectBootstrapTarget(ctx, maintenanceURL, whole.Target.Name)
	if err != nil {
		return errors.New("connect bootstrap target")
	}
	targetDigest, _ := bootstrapTargetDigest(whole.Target)
	if err := bindBootstrapIdentity(ctx, conn, whole, targetDigest, true); err != nil {
		conn.Close(context.WithoutCancel(ctx))
		return err
	}
	confirmed, _, err := readBootstrapSteps(ctx, conn, whole)
	if err != nil {
		conn.Close(context.WithoutCancel(ctx))
		return err
	}
	if whole.Target.Mode == bootstrap.ManagedDatabase {
		conn.Close(context.WithoutCancel(ctx))
		if !dropManaged {
			return errors.New("managed bootstrap abort requires explicit database drop authorization")
		}
		if restore != nil {
			restore(context.WithoutCancel(ctx))
			restore = nil
		}
		if err := DropDatabaseURL(ctx, maintenanceURL, whole.Target.Name, true); err != nil {
			return err
		}
		return cleanupConfirmedClusterSecurity(ctx, maintenanceURL, whole, confirmed)
	}
	_, err = conn.Exec(ctx, `drop schema autosql_internal cascade`)
	conn.Close(context.WithoutCancel(ctx))
	if err != nil {
		return errors.New("remove external bootstrap ledger")
	}
	return nil
}

func cleanupConfirmedClusterSecurity(ctx context.Context, maintenanceURL string, whole bootstrap.Plan, confirmed map[string]bool) error {
	changeComplete := map[string]bool{}
	changeSeen := map[string]bool{}
	for _, bound := range whole.Steps {
		if bound.Action != bootstrap.ActionExecuteSchema {
			continue
		}
		if !changeSeen[bound.ChangeID] {
			changeComplete[bound.ChangeID] = true
			changeSeen[bound.ChangeID] = true
		}
		changeComplete[bound.ChangeID] = changeComplete[bound.ChangeID] && confirmed[bound.ID]
	}
	var memberships, roles []schema.Resource
	for _, change := range whole.SchemaPlan.Changes.Changes {
		if change.Operation != schema.OperationCreate || change.After == nil || !changeComplete[change.ID] {
			continue
		}
		switch change.After.Kind {
		case schema.KindMembership:
			memberships = append(memberships, *change.After)
		case schema.KindRole:
			roles = append(roles, *change.After)
		}
	}
	sort.Slice(memberships, func(i, j int) bool { return memberships[i].ID > memberships[j].ID })
	sort.Slice(roles, func(i, j int) bool { return roles[i].ID > roles[j].ID })
	conn, err := pgx.Connect(ctx, maintenanceURL)
	if err != nil {
		return errors.New("connect cluster security cleanup")
	}
	defer conn.Close(context.WithoutCancel(ctx))
	resources := map[string]schema.Resource{}
	for _, change := range whole.SchemaPlan.Changes.Changes {
		if change.After != nil {
			resources[change.After.ID] = *change.After
		}
	}
	for _, membership := range memberships {
		statements, err := renderMembershipRevoke(membership, resources, nil)
		if err != nil {
			return errors.New("render cluster membership cleanup")
		}
		for _, statement := range statements {
			if _, err := conn.Exec(ctx, statement); err != nil {
				return errors.New("remove bootstrap role membership")
			}
		}
	}
	for _, role := range roles {
		if _, err := conn.Exec(ctx, "DROP ROLE "+quote(role.Name.Name)); err != nil {
			return errors.New("remove bootstrap role")
		}
	}
	return nil
}
