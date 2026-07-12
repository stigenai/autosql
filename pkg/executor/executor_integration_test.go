package executor

import (
	"context"
	"errors"
	"fmt"
	"os"
	"testing"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/plan"
	"autosql/pkg/precheck"
	"github.com/jackc/pgx/v5"
)

func liveURL(t *testing.T) string {
	t.Helper()
	u := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if u == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	return u
}

func testExecutor(t *testing.T, p plan.Plan, digest string) *PostgreSQL {
	t.Helper()
	now := time.Now().UTC()
	a := artifact.Artifact{Digest: digest, DatabaseIdentity: "executor-test", TargetEnvironment: "test", SourceRevision: "test-revision", CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Plan: p}
	return &PostgreSQL{config: Config{URL: liveURL(t), Connector: PGXConnector{}, Now: func() time.Time { return now }, State: func(context.Context, Session) (RuntimeState, error) {
		return RuntimeState{Fingerprint: p.FromFingerprint, SourceRevision: "test-revision", Environment: "test", DatabaseIdentity: "executor-test"}, nil
	}}, artifact: a}
}

func TestTransactionalDDLAndHistoryRollbackTogether(t *testing.T) {
	ctx := context.Background()
	name := fmt.Sprintf("autosql_exec_tx_%d", time.Now().UnixNano())
	digest := "tx-" + name
	p := plan.Plan{FromFingerprint: "sha256:" + string(make([]byte, 64)), Steps: []plan.Step{{ID: "one", SQL: "create table " + name + "(id int)", Kind: plan.StepExecutable, Transaction: plan.TransactionRequired}, {ID: "two", SQL: "alter table missing_autosql_table add column x int", Kind: plan.StepExecutable, Transaction: plan.TransactionRequired}}, Phases: []plan.Phase{{ID: "phase", Transaction: plan.TransactionRequired, StepIDs: []string{"one", "two"}}}}
	e := testExecutor(t, p, digest)
	_, err := e.ApplyAuthorized(ctx, precheck.Plan{})
	if err == nil {
		t.Fatal("expected transactional failure")
	}
	conn, err := pgx.Connect(ctx, liveURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	defer conn.Exec(ctx, "delete from autosql_migration_history where artifact_digest=$1", digest)
	var exists bool
	if err := conn.QueryRow(ctx, `select to_regclass($1) is not null`, name).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("transactional DDL survived rollback")
	}
	var count int
	if err := conn.QueryRow(ctx, `select count(*) from autosql_migration_history where artifact_digest=$1`, digest).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("history survived rollback: %d", count)
	}
}

func TestNontransactionalIntentAndRetryRefusal(t *testing.T) {
	ctx := context.Background()
	name := fmt.Sprintf("autosql_exec_nt_%d", time.Now().UnixNano())
	digest := "nt-" + name
	p := plan.Plan{FromFingerprint: "sha256:" + string(make([]byte, 64)), Steps: []plan.Step{{ID: "one", SQL: "create table " + name + "(id int)", Kind: plan.StepExecutable, Transaction: plan.TransactionProhibited}, {ID: "two", SQL: "alter table missing_autosql_table add column x int", Kind: plan.StepExecutable, Transaction: plan.TransactionProhibited}}, Phases: []plan.Phase{{ID: "phase", Transaction: plan.TransactionProhibited, StepIDs: []string{"one", "two"}}}}
	e := testExecutor(t, p, digest)
	_, err := e.ApplyAuthorized(ctx, precheck.Plan{})
	if !errors.Is(err, ErrReconcile) || !e.Result().Uncertain || e.Result().LastConfirmed != "one" {
		t.Fatalf("err=%v result=%+v", err, e.Result())
	}
	_, err = e.ApplyAuthorized(ctx, precheck.Plan{})
	if !errors.Is(err, ErrReconcile) {
		t.Fatalf("retry err=%v", err)
	}
	conn, err := pgx.Connect(ctx, liveURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	defer conn.Exec(ctx, "drop table if exists "+name)
	defer conn.Exec(ctx, "delete from autosql_migration_history where artifact_digest=$1", digest)
	var state, last, guidance string
	if err := conn.QueryRow(ctx, `select state,last_confirmed_step,recovery_guidance from autosql_migration_history where artifact_digest=$1 and step_id='two'`, digest).Scan(&state, &last, &guidance); err != nil {
		t.Fatal(err)
	}
	if state != "intended" || last != "one" || guidance == "" {
		t.Fatalf("state=%s last=%s guidance=%q", state, last, guidance)
	}
}

func TestCompetingApplyFailsDeterministically(t *testing.T) {
	ctx := context.Background()
	digest := fmt.Sprintf("lock-%d", time.Now().UnixNano())
	p := plan.Plan{FromFingerprint: "from", Steps: []plan.Step{{ID: "sleep", SQL: "select pg_sleep(0.5)", Kind: plan.StepExecutable, Transaction: plan.TransactionRequired}}, Phases: []plan.Phase{{ID: "phase", Transaction: plan.TransactionRequired, StepIDs: []string{"sleep"}}}}
	first, second := testExecutor(t, p, digest), testExecutor(t, p, digest)
	done := make(chan error, 1)
	go func() { _, err := first.ApplyAuthorized(ctx, precheck.Plan{}); done <- err }()
	time.Sleep(100 * time.Millisecond)
	if _, err := second.ApplyAuthorized(ctx, precheck.Plan{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("competing err=%v", err)
	}
	if err := <-done; err != nil {
		t.Fatalf("first err=%v", err)
	}
	conn, err := pgx.Connect(ctx, liveURL(t))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	_, _ = conn.Exec(ctx, "delete from autosql_migration_history where artifact_digest=$1", digest)
}
