package executor

import (
	"autosql/pkg/artifact"
	"autosql/pkg/plan"
	"autosql/pkg/precheck"
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"testing"
	"time"
)

type testTag int64

func (t testTag) RowsAffected() int64 { return int64(t) }

type testRow struct {
	vals []any
	err  error
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
		}
	}
	return nil
}

type testRows struct{}

func (testRows) Next() bool        { return false }
func (testRows) Scan(...any) error { return nil }
func (testRows) Err() error        { return nil }
func (testRows) Close()            {}

type testTx struct {
	execAt, execs      int
	execErr, commitErr error
	rolled             bool
}

func (t *testTx) Exec(context.Context, string, ...any) (Tag, error) {
	t.execs++
	if t.execs == t.execAt {
		return testTag(0), t.execErr
	}
	return testTag(1), nil
}
func (*testTx) QueryRow(context.Context, string, ...any) Row { return testRow{} }
func (t *testTx) Commit(context.Context) error               { return t.commitErr }
func (t *testTx) Rollback(context.Context) error             { t.rolled = true; return nil }

type testSession struct {
	tx       *testTx
	execTags []Tag
	execErrs []error
	execs    int
	row      Row
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
	if s.row != nil {
		return s.row
	}
	return testRow{}
}
func (*testSession) Query(context.Context, string, ...any) (Rows, error) { return testRows{}, nil }
func (s *testSession) Begin(context.Context) (Tx, error)                 { return s.tx, nil }
func (*testSession) Close(context.Context) error                         { return nil }
func (*testSession) Raw() *pgx.Conn                                      { return nil }

type failingAudit struct {
	fail   string
	events []string
}

func (a *failingAudit) AppendDurable(_ context.Context, e LifecycleEvent) error {
	a.events = append(a.events, e.Type)
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
			_, _ = e.ApplyAuthorized(context.Background(), precheck.Plan{})
			found := false
			for _, v := range audit.events {
				found = found || v == tc.want
			}
			if !found {
				t.Fatalf("events=%v want=%s", audit.events, tc.want)
			}
		})
	}
}
