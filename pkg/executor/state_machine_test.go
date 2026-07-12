package executor

import (
	"autosql/pkg/artifact"
	"autosql/pkg/plan"
	"autosql/pkg/precheck"
	"context"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"reflect"
	"strings"
	"testing"
	"time"
)

type testTag int64

func (t testTag) RowsAffected() int64 { return int64(t) }

type testRow struct {
	vals []any
	err  error
}

func TestReconcileRefusedLifecycle(t *testing.T) {
	a := &failingAudit{}
	s := &testSession{row: testRow{vals: []any{true}}}
	now := time.Now()
	art := artifact.Artifact{Digest: "artifact", DatabaseIdentity: "db", TargetEnvironment: "prod", SourceRevision: "rev", CreatedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), Plan: plan.Plan{Digest: "plan", FromFingerprint: "from"}, GuardrailDigest: "bundle"}
	e := &PostgreSQL{artifact: art, config: Config{URL: "x", Connector: testConnector{s: s}, Audit: a, Now: func() time.Time { return now }, State: func(context.Context, Session) (RuntimeState, error) {
		return RuntimeState{Fingerprint: "from", SourceRevision: "rev", Environment: "prod", DatabaseIdentity: "db"}, nil
	}}}
	_, err := e.ApplyAuthorized(context.Background(), precheck.Plan{})
	if !errors.Is(err, ErrReconcile) || a.events[len(a.events)-1] != "reconcile_refused" {
		t.Fatalf("err=%v events=%v", err, a.events)
	}
}

func TestRefusalAuditWriteFailureIsFailClosed(t *testing.T) {
	a := &failingAudit{fail: "contended"}
	s := &testSession{row: testRow{vals: []any{false}}}
	e := &PostgreSQL{artifact: artifact.Artifact{Digest: "artifact", DatabaseIdentity: "db", TargetEnvironment: "prod", Plan: plan.Plan{Digest: "plan"}, GuardrailDigest: "bundle"}, config: Config{URL: "x", Connector: testConnector{s: s}, Audit: a, Now: time.Now}}
	_, err := e.ApplyAuthorized(context.Background(), precheck.Plan{})
	if err == nil || s.execs != 0 {
		t.Fatalf("err=%v execs=%d", err, s.execs)
	}
}

func TestConfirmationUpdateErrorIsUncertain(t *testing.T) {
	assertConfirmationUncertain(t, &testSession{execErrs: []error{nil, nil, errors.New("update secret=password")}}, "persistence")
}
func TestConfirmationReadbackQueryErrorIsUncertain(t *testing.T) {
	assertConfirmationUncertain(t, &testSession{row: testRow{err: errors.New("readback secret=password")}}, "readback")
}
func TestConfirmationReadbackStateMismatchIsUncertain(t *testing.T) {
	step := plan.Step{ID: "s", SQL: "ddl", Kind: plan.StepExecutable, Transaction: plan.TransactionProhibited}
	assertConfirmationUncertain(t, &testSession{row: testRow{vals: []any{"intended", stepHash(step)}}}, "readback")
}
func TestConfirmationReadbackHashMismatchIsUncertain(t *testing.T) {
	assertConfirmationUncertain(t, &testSession{row: testRow{vals: []any{"confirmed", "sha256:wrong"}}}, "readback")
}
func assertConfirmationUncertain(t *testing.T, s *testSession, guidance string) {
	t.Helper()
	a := &failingAudit{}
	e := stateExecutor(a)
	e.config.Audit = a
	step := plan.Step{ID: "s", SQL: "ddl", Kind: plan.StepExecutable, Transaction: plan.TransactionProhibited}
	err := e.nontransactionalPhase(context.Background(), s, plan.Phase{ID: "p", Transaction: plan.TransactionProhibited, StepIDs: []string{"s"}}, map[string]plan.Step{"s": step}, map[string]bool{})
	if !errors.Is(err, ErrReconcile) || !e.result.Uncertain || e.result.PendingStep != "s" || e.result.ExecutionID != "artifact" || !strings.Contains(e.result.RecoveryGuidance, guidance) || s.execs != 3 {
		t.Fatalf("err=%v result=%+v execs=%d", err, e.result, s.execs)
	}
	if a.events[len(a.events)-1] != "uncertain" {
		t.Fatalf("events=%v", a.events)
	}
}

func TestConfirmedHistoryBindingTamperVariants(t *testing.T) {
	step := plan.Step{ID: "s", SQL: "ddl", Kind: plan.StepExecutable, Transaction: plan.TransactionRequired}
	phase := plan.Phase{ID: "p", Transaction: plan.TransactionRequired, StepIDs: []string{"s"}}
	a := artifact.Artifact{Digest: "artifact", DatabaseIdentity: "db", TargetEnvironment: "prod", GuardrailDigest: "bundle", Plan: plan.Plan{Digest: "plan", Steps: []plan.Step{step}, Phases: []plan.Phase{phase}}}
	base := []any{"s", stepHash(step), "p", "required", "artifact", "db/prod", "plan", "bundle"}
	for i, name := range []string{"step_hash", "phase", "mode", "execution", "target", "plan", "bundle"} {
		t.Run(name, func(t *testing.T) {
			row := append([]any(nil), base...)
			row[i+1] = "tampered"
			s := &testSession{rows: &valueRows{values: [][]any{row}}}
			got, err := confirmedSteps(context.Background(), s, a)
			if err == nil || len(got) != 0 || s.execs != 0 {
				t.Fatalf("got=%v err=%v execs=%d", got, err, s.execs)
			}
		})
	}
}

func TestRollbackLifecycleTruth(t *testing.T) {
	for _, fail := range []bool{false, true} {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			a := &failingAudit{}
			e := stateExecutor(a)
			tx := &testTx{execAt: 1, execErr: errors.New("ddl")}
			if fail {
				tx.rollbackErr = errors.New("rollback")
			}
			step := plan.Step{ID: "s", SQL: "ddl", Kind: plan.StepExecutable, Transaction: plan.TransactionRequired}
			_, _ = e.transactionalPhase(context.Background(), &testSession{tx: tx}, plan.Phase{ID: "p", Transaction: plan.TransactionRequired, StepIDs: []string{"s"}}, map[string]plan.Step{"s": step}, nil, precheck.Plan{}, false)
			want := "transaction_rollback"
			if fail {
				want = "rollback_failed"
				if !e.result.Uncertain {
					t.Fatal("rollback failure not uncertain")
				}
			}
			if a.events[len(a.events)-1] != want {
				t.Fatalf("events=%v", a.events)
			}
		})
	}
}

func TestPostConfirmationAuditFailureReportsPartial(t *testing.T) {
	a := &failingAudit{fail: "confirmed"}
	e := stateExecutor(a)
	step := plan.Step{ID: "s", SQL: "ddl", Kind: plan.StepExecutable, Transaction: plan.TransactionProhibited}
	s := &testSession{row: testRow{vals: []any{"confirmed", stepHash(step)}}}
	err := e.nontransactionalPhase(context.Background(), s, plan.Phase{ID: "p", Transaction: plan.TransactionProhibited, StepIDs: []string{"s"}}, map[string]plan.Step{"s": step}, map[string]bool{})
	if !errors.Is(err, ErrPartial) || !e.result.Partial || e.result.AppliedSteps != 1 {
		t.Fatalf("err=%v result=%+v", err, e.result)
	}
}

func TestPostCommitAuditFailureReportsKnownPartial(t *testing.T) {
	a := &failingAudit{fail: "transaction_committed"}
	e := stateExecutor(a)
	step := plan.Step{ID: "s", SQL: "ddl", Kind: plan.StepExecutable, Transaction: plan.TransactionRequired}
	tx := &testTx{}
	_, err := e.transactionalPhase(context.Background(), &testSession{tx: tx}, plan.Phase{ID: "p", Transaction: plan.TransactionRequired, StepIDs: []string{"s"}}, map[string]plan.Step{"s": step}, map[string]bool{}, precheck.Plan{}, false)
	if !errors.Is(err, ErrPartial) || !e.result.Partial || e.result.AppliedSteps != 1 || e.result.LastConfirmed != "s" {
		t.Fatalf("err=%v result=%+v events=%v", err, e.result, a.events)
	}
}

func (r testRow) Scan(dst ...any) error {
	if r.err != nil {
		return r.err
	}
	for i, v := range r.vals {
		switch p := dst[i].(type) {
		case *string:
			*p = v.(string)
		case *bool:
			*p = v.(bool)
		case *int64:
			*p = v.(int64)
		}
	}
	return nil
}

type testRows struct{}

func (testRows) Next() bool        { return false }
func (testRows) Scan(...any) error { return nil }
func (testRows) Err() error        { return nil }
func (testRows) Close()            {}

type valueRows struct {
	values [][]any
	i      int
	err    error
}

func (r *valueRows) Next() bool {
	if r.i < len(r.values) {
		r.i++
		return true
	}
	return false
}
func (r *valueRows) Scan(dst ...any) error { return testRow{vals: r.values[r.i-1]}.Scan(dst...) }
func (r *valueRows) Err() error            { return r.err }
func (r *valueRows) Close()                {}

type testTx struct {
	execAt, execs      int
	execErr, commitErr error
	rolled             bool
	rollbackErr        error
	row                Row
}

func (t *testTx) Exec(context.Context, string, ...any) (Tag, error) {
	t.execs++
	if t.execs == t.execAt {
		return testTag(0), t.execErr
	}
	return testTag(1), nil
}
func (t *testTx) QueryRow(context.Context, string, ...any) Row {
	if t.row != nil {
		return t.row
	}
	return testRow{}
}
func (t *testTx) Commit(context.Context) error   { return t.commitErr }
func (t *testTx) Rollback(context.Context) error { t.rolled = true; return t.rollbackErr }

type testSession struct {
	tx       *testTx
	execTags []Tag
	execErrs []error
	execs    int
	row      Row
	rowQueue []Row
	rows     Rows
	beginErr error
}

func (s *testSession) Exec(context.Context, string, ...any) (Tag, error) {
	i := s.execs
	s.execs++
	if i < len(s.execErrs) && s.execErrs[i] != nil {
		return testTag(0), s.execErrs[i]
	}
	if i < len(s.execTags) {
		return s.execTags[i], nil
	}
	return testTag(1), nil
}
func (s *testSession) QueryRow(context.Context, string, ...any) Row {
	if len(s.rowQueue) > 0 {
		r := s.rowQueue[0]
		s.rowQueue = s.rowQueue[1:]
		return r
	}
	if s.row != nil {
		return s.row
	}
	return testRow{}
}
func (s *testSession) Query(context.Context, string, ...any) (Rows, error) {
	if s.rows != nil {
		return s.rows, nil
	}
	return testRows{}, nil
}
func (s *testSession) Begin(context.Context) (Tx, error) { return s.tx, s.beginErr }
func (*testSession) Close(context.Context) error         { return nil }
func (*testSession) Raw() *pgx.Conn                      { return nil }

type failingAudit struct {
	fail    string
	events  []string
	records []LifecycleEvent
}

func (a *failingAudit) AppendDurable(_ context.Context, e LifecycleEvent) error {
	a.events = append(a.events, e.Type)
	a.records = append(a.records, e)
	if e.Type == a.fail {
		return errors.New("audit fail")
	}
	return nil
}
func stateExecutor(audit LifecycleAudit) *PostgreSQL {
	return &PostgreSQL{artifact: artifact.Artifact{Digest: "artifact", DatabaseIdentity: "db", TargetEnvironment: "prod", Plan: plan.Plan{Digest: "plan"}, GuardrailDigest: "bundle"}, config: Config{Audit: audit, Now: func() time.Time { return time.Now() }}}
}
func TestTransactionalFailureAndCommitAmbiguityAccounting(t *testing.T) {
	step := plan.Step{ID: "s", SQL: "ddl", Kind: plan.StepExecutable, Transaction: plan.TransactionRequired}
	phase := plan.Phase{ID: "p", Transaction: plan.TransactionRequired, StepIDs: []string{"s"}}
	for name, tx := range map[string]*testTx{"statement": {execAt: 1, execErr: errors.New("ddl")}, "history": {execAt: 2, execErr: errors.New("history")}, "commit": {commitErr: errors.New("lost")}} {
		t.Run(name, func(t *testing.T) {
			e := stateExecutor(nil)
			s := &testSession{tx: tx}
			_, err := e.transactionalPhase(context.Background(), s, phase, map[string]plan.Step{"s": step}, map[string]bool{}, precheck.Plan{}, false)
			if err == nil || e.result.AppliedSteps != 0 {
				t.Fatalf("err=%v result=%+v", err, e.result)
			}
			if name == "commit" && (!e.result.Uncertain || !errors.Is(err, ErrReconcile)) {
				t.Fatalf("commit=%+v %v", e.result, err)
			}
			if name != "commit" && !tx.rolled {
				t.Fatal("rollback not attempted")
			}
		})
	}
}
func TestConfirmationZeroIsUncertainAndTopologyNotCounted(t *testing.T) {
	step := plan.Step{ID: "s", SQL: "ddl", Kind: plan.StepExecutable, Transaction: plan.TransactionProhibited}
	phase := plan.Phase{ID: "p", Transaction: plan.TransactionProhibited, StepIDs: []string{"s"}}
	e := stateExecutor(nil)
	s := &testSession{execTags: []Tag{testTag(1), testTag(1), testTag(0)}}
	err := e.nontransactionalPhase(context.Background(), s, phase, map[string]plan.Step{"s": step}, map[string]bool{})
	if !errors.Is(err, ErrReconcile) || !e.result.Uncertain || e.result.PendingStep != "s" || e.result.AppliedSteps != 0 {
		t.Fatalf("err=%v result=%+v", err, e.result)
	}
	top := plan.Step{ID: "top", Kind: plan.StepTopology, Transaction: plan.TransactionRequired}
	tx := &testTx{}
	e = stateExecutor(nil)
	_, err = e.transactionalPhase(context.Background(), &testSession{tx: tx}, plan.Phase{ID: "p", Transaction: plan.TransactionRequired, StepIDs: []string{"top"}}, map[string]plan.Step{"top": top}, map[string]bool{}, precheck.Plan{}, false)
	if err != nil || e.result.AppliedSteps != 0 || tx.execs != 0 {
		t.Fatalf("topology result=%+v execs=%d err=%v", e.result, tx.execs, err)
	}
}

type testConnector struct {
	s   Session
	err error
}

func (c testConnector) Connect(context.Context, string) (Session, error) { return c.s, c.err }

func TestRefusalLifecycleEventsUnderLock(t *testing.T) {
	base := artifact.Artifact{Digest: "artifact", DatabaseIdentity: "db", TargetEnvironment: "prod", SourceRevision: "rev", CreatedAt: time.Now().Add(-time.Hour), ExpiresAt: time.Now().Add(time.Hour), Plan: plan.Plan{Digest: "plan", FromFingerprint: "from", ToFingerprint: "to"}, GuardrailDigest: "bundle"}
	cases := []struct {
		name   string
		locked bool
		reauth error
		now    time.Time
		state  RuntimeState
		want   string
	}{{"contended", false, nil, time.Now(), RuntimeState{}, "contended"}, {"authorization", true, errors.New("revoked"), time.Now(), RuntimeState{}, "authorization_refused"}, {"expiry", true, nil, time.Now().Add(2 * time.Hour), RuntimeState{}, "expiry_refused"}, {"stale", true, nil, time.Now(), RuntimeState{Fingerprint: "other", SourceRevision: "rev", Environment: "prod", DatabaseIdentity: "db"}, "stale"}}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			audit := &failingAudit{}
			session := &testSession{row: testRow{vals: []any{tc.locked}}}
			e := &PostgreSQL{artifact: base, config: Config{URL: "x", Connector: testConnector{s: session}, Audit: audit, Now: func() time.Time { return tc.now }, Reauthorize: func(context.Context, artifact.Artifact) error { return tc.reauth }, State: func(context.Context, Session) (RuntimeState, error) { return tc.state, nil }}}
			_, applyErr := e.ApplyAuthorized(context.Background(), precheck.Plan{})
			found := false
			for _, v := range audit.events {
				found = found || v == tc.want
			}
			if !found {
				t.Fatalf("events=%v want=%s", audit.events, tc.want)
			}
			r := audit.records[len(audit.records)-1]
			if r.ExecutionID != "artifact" || r.Target != "db/prod" || r.ArtifactDigest != "artifact" || r.PlanDigest != "plan" || r.BundleDigest != "bundle" {
				t.Fatalf("unbound event=%+v", r)
			}
			if session.execs > 1 {
				t.Fatalf("migration/history execs=%d err=%v", session.execs, applyErr)
			}
		})
	}
}

func TestCommitAmbiguityLifecycleHasNoRollback(t *testing.T) {
	a := &failingAudit{}
	e := stateExecutor(a)
	step := plan.Step{ID: "s", SQL: "ddl", Kind: plan.StepExecutable, Transaction: plan.TransactionRequired}
	tx := &testTx{commitErr: errors.New("session lost")}
	_, err := e.transactionalPhase(context.Background(), &testSession{tx: tx}, plan.Phase{ID: "p", Transaction: plan.TransactionRequired, StepIDs: []string{"s"}}, map[string]plan.Step{"s": step}, map[string]bool{}, precheck.Plan{}, false)
	if !errors.Is(err, ErrReconcile) || tx.rolled {
		t.Fatalf("err=%v rolled=%v", err, tx.rolled)
	}
	want := []string{"transaction_started", "uncertain"}
	if !reflect.DeepEqual(a.events, want) {
		t.Fatalf("events=%v want=%v", a.events, want)
	}
}

func TestBeginPrecheckAndRollbackFailures(t *testing.T) {
	phase := plan.Phase{ID: "p", Transaction: plan.TransactionRequired}
	e := stateExecutor(nil)
	_, err := e.transactionalPhase(context.Background(), &testSession{beginErr: errors.New("begin")}, phase, nil, nil, precheck.Plan{}, false)
	if err == nil {
		t.Fatal("begin failure accepted")
	}
	tx := &testTx{rollbackErr: errors.New("rollback"), row: testRow{vals: []any{int64(1)}}}
	e = stateExecutor(nil)
	check := precheck.Assertion{Name: "blocked", MaxAllowed: 0}
	_, err = e.transactionalPhase(context.Background(), &testSession{tx: tx}, phase, nil, nil, precheck.Plan{Assertions: []precheck.Assertion{check}}, true)
	if err == nil || !tx.rolled || e.result.AppliedSteps != 0 {
		t.Fatalf("err=%v rolled=%v result=%+v", err, tx.rolled, e.result)
	}
}
