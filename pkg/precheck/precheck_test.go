package precheck

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

type fakeDB struct{ tx *fakeTx }

func (d fakeDB) Begin(context.Context) (Tx, error) {
	d.tx.events = append(d.tx.events, "begin")
	return d.tx, nil
}

type fakeTx struct {
	events   []string
	counts   []int64
	queryErr error
	execs    int
}

func (t *fakeTx) AcquireLock(context.Context) error { t.events = append(t.events, "lock"); return nil }
func (t *fakeTx) QueryCount(ctx context.Context, _ string, _ ...any) (int64, error) {
	t.events = append(t.events, "check")
	if t.queryErr != nil {
		return 0, t.queryErr
	}
	if len(t.counts) == 0 {
		<-ctx.Done()
		return 0, ctx.Err()
	}
	n := t.counts[0]
	t.counts = t.counts[1:]
	return n, nil
}
func (t *fakeTx) Exec(context.Context, string) error {
	t.events = append(t.events, "exec")
	t.execs++
	return nil
}
func (t *fakeTx) Commit(context.Context) error   { t.events = append(t.events, "commit"); return nil }
func (t *fakeTx) Rollback(context.Context) error { t.events = append(t.events, "rollback"); return nil }

func plan(max int64) Plan {
	p := Plan{ID: "p1", ChangeDigest: "change1", Statements: []string{"ALTER TABLE t ADD UNIQUE (n)"}}
	p.Assertions = []Assertion{{Name: "duplicates", Query: "SELECT count(*) FROM duplicates WHERE n = $1", Args: []any{7}, MaxAllowed: max, ChangeDigest: p.ChangeDigest, Timeout: time.Second, Source: Source{File: "checks.sql", Line: 12, Column: 3}}}
	p.Digest, _ = Digest(p)
	p.Assertions[0].PlanDigest = p.Digest
	return p
}

func TestGuardedApplyOrder(t *testing.T) {
	tx := &fakeTx{counts: []int64{0}}
	got, err := GuardedApply(context.Background(), fakeDB{tx}, plan(0))
	if err != nil || !got[0].Passed {
		t.Fatalf("got %v, %v", got, err)
	}
	want := []string{"begin", "lock", "check", "exec", "commit"}
	if !reflect.DeepEqual(tx.events, want) {
		t.Fatalf("events %v", tx.events)
	}
}
func TestFailurePreventsEveryMutation(t *testing.T) {
	tx := &fakeTx{counts: []int64{2}}
	_, err := GuardedApply(context.Background(), fakeDB{tx}, plan(0))
	if !errors.Is(err, ErrAssertion) {
		t.Fatalf("error %v", err)
	}
	if !strings.Contains(err.Error(), "checks.sql:12:3") {
		t.Fatalf("missing source in %v", err)
	}
	if tx.execs != 0 {
		t.Fatal("mutation executed")
	}
	want := []string{"begin", "lock", "check", "rollback"}
	if !reflect.DeepEqual(tx.events, want) {
		t.Fatalf("events %v", tx.events)
	}
}
func TestDigestMismatchDoesNotBegin(t *testing.T) {
	tx := &fakeTx{}
	p := plan(0)
	p.Assertions[0].ChangeDigest = "other"
	_, err := GuardedApply(context.Background(), fakeDB{tx}, p)
	if !errors.Is(err, ErrInvalidPlan) || len(tx.events) > 0 {
		t.Fatalf("error=%v events=%v", err, tx.events)
	}
}
func TestAssertionTimeoutRollsBack(t *testing.T) {
	tx := &fakeTx{}
	p := plan(0)
	p.Assertions[0].Timeout = time.Millisecond
	p.Digest, _ = Digest(p)
	p.Assertions[0].PlanDigest = p.Digest
	_, err := GuardedApply(context.Background(), fakeDB{tx}, p)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v", err)
	}
	if tx.execs != 0 {
		t.Fatal("mutation executed")
	}
}
func TestRejectsUnsafeEvidenceQuery(t *testing.T) {
	queries := []string{
		"DELETE FROM users",
		"SELECT email FROM users",
		"SELECT count(*) FROM users; DELETE FROM users",
		"SELECT count(side_effect()) FROM users",
		"SELECT count(*) FROM users WHERE side_effect() = $1",
		"SELECT count(*) FROM users WHERE id = $1 OR admin = $2",
	}
	for _, query := range queries {
		p := plan(0)
		p.Assertions[0].Query = query
		if _, err := GuardedApply(context.Background(), fakeDB{&fakeTx{}}, p); !errors.Is(err, ErrInvalidPlan) {
			t.Errorf("query %q: error %v", query, err)
		}
	}
}

func TestEverySemanticFieldIsDigestBound(t *testing.T) {
	tests := map[string]func(*Plan){
		"plan id":       func(p *Plan) { p.ID = "other" },
		"statement":     func(p *Plan) { p.Statements[0] = "DROP TABLE t" },
		"name":          func(p *Plan) { p.Assertions[0].Name = "changed" },
		"query":         func(p *Plan) { p.Assertions[0].Query = "SELECT count(*) FROM other WHERE n = $1" },
		"args":          func(p *Plan) { p.Assertions[0].Args[0] = 8 },
		"args type":     func(p *Plan) { p.Assertions[0].Args[0] = int64(7) },
		"maximum":       func(p *Plan) { p.Assertions[0].MaxAllowed++ },
		"timeout":       func(p *Plan) { p.Assertions[0].Timeout++ },
		"change digest": func(p *Plan) { p.Assertions[0].ChangeDigest = "other" },
		"source":        func(p *Plan) { p.Assertions[0].Source.Line++ },
	}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			p := plan(0)
			p.Assertions[0].Args = append([]any(nil), p.Assertions[0].Args...)
			mutate(&p)
			tx := &fakeTx{}
			if _, err := GuardedApply(context.Background(), fakeDB{tx}, p); !errors.Is(err, ErrInvalidPlan) || len(tx.events) != 0 {
				t.Fatalf("error=%v events=%v", err, tx.events)
			}
		})
	}
}

func TestDigestRejectsNonCanonicalArgumentType(t *testing.T) {
	p := plan(0)
	p.Assertions[0].Args = []any{map[string]any{"secret": "value"}}
	if _, err := Digest(p); !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("error %v", err)
	}
}

func TestValidationErrorIncludesAssertionSource(t *testing.T) {
	p := plan(0)
	p.Assertions[0].Name = ""
	_, err := GuardedApply(context.Background(), fakeDB{&fakeTx{}}, p)
	var validationErr *ValidationError
	if !errors.As(err, &validationErr) || validationErr.Source.File != "checks.sql" || validationErr.Source.Line != 12 || validationErr.Source.Column != 3 {
		t.Fatalf("error %#v", err)
	}
}

func TestMissingAssertionSourceIsRejectedBeforeBegin(t *testing.T) {
	for _, source := range []Source{{}, {File: "checks.sql"}, {File: "checks.sql", Line: 1, Column: -1}} {
		p := plan(0)
		p.Assertions[0].Source = source
		p.Digest, _ = Digest(p)
		p.Assertions[0].PlanDigest = p.Digest
		tx := &fakeTx{}
		if _, err := GuardedApply(context.Background(), fakeDB{tx}, p); !errors.Is(err, ErrInvalidPlan) || len(tx.events) != 0 {
			t.Fatalf("source=%+v error=%v events=%v", source, err, tx.events)
		}
	}
}

func TestCanonicalQueryIsExecuted(t *testing.T) {
	p := plan(0)
	p.Assertions[0].Query = " select COUNT ( * ) from DUPLICATES where N=$1 "
	p.Digest, _ = Digest(p)
	p.Assertions[0].PlanDigest = p.Digest
	tx := &queryCapturingTx{fakeTx: fakeTx{counts: []int64{0}}}
	if _, err := GuardedApply(context.Background(), capturingDB{tx}, p); err != nil {
		t.Fatal(err)
	}
	if tx.query != "SELECT count(*) FROM duplicates WHERE n = $1" {
		t.Fatalf("query %q", tx.query)
	}
}

type queryCapturingTx struct {
	fakeTx
	query string
}

func (t *queryCapturingTx) QueryCount(ctx context.Context, q string, args ...any) (int64, error) {
	t.query = q
	return t.fakeTx.QueryCount(ctx, q, args...)
}

type capturingDB struct{ tx *queryCapturingTx }

func (d capturingDB) Begin(context.Context) (Tx, error) {
	d.tx.events = append(d.tx.events, "begin")
	return d.tx, nil
}
