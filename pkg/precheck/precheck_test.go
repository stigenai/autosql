package precheck

import (
	"context"
	"errors"
	"reflect"
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
	p.Digest = Digest(p)
	p.Assertions = []Assertion{{Name: "duplicates", Query: "SELECT count(*) FROM duplicates WHERE n = $1", Args: []any{7}, MaxAllowed: max, PlanDigest: p.Digest, ChangeDigest: p.ChangeDigest}}
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
	_, err := GuardedApply(context.Background(), fakeDB{tx}, p)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v", err)
	}
	if tx.execs != 0 {
		t.Fatal("mutation executed")
	}
}
func TestRejectsUnsafeEvidenceQuery(t *testing.T) {
	p := plan(0)
	p.Assertions[0].Query = "DELETE FROM users"
	_, err := GuardedApply(context.Background(), fakeDB{&fakeTx{}}, p)
	if !errors.Is(err, ErrInvalidPlan) {
		t.Fatalf("error %v", err)
	}
}
