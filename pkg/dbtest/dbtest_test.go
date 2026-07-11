package dbtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type factory struct{ db *fakeDB }

func (f factory) OpenIsolated(_ context.Context, name string) (Database, error) {
	f.db.events = append(f.db.events, "open:"+name)
	return f.db, nil
}

type fakeDB struct {
	events []string
	counts []int64
	fail   string
	block  bool
}

func (d *fakeDB) Exec(ctx context.Context, q string) error {
	d.events = append(d.events, q)
	if d.block {
		<-ctx.Done()
		return ctx.Err()
	}
	if strings.Contains(q, d.fail) && d.fail != "" {
		return errors.New("boom")
	}
	return nil
}
func (d *fakeDB) QueryCount(context.Context, string, ...any) (int64, error) {
	n := d.counts[0]
	d.counts = d.counts[1:]
	return n, nil
}
func (d *fakeDB) Close(context.Context) error { d.events = append(d.events, "close"); return nil }

func TestBlankSchemaAndMultiVersionUpgrade(t *testing.T) {
	db := &fakeDB{counts: []int64{1, 2, 3}}
	c := Case{Name: "upgrade", Variables: map[string]string{"schema": "case_a"}, Setup: []Command{{SQL: "CREATE SCHEMA ${schema}"}}, Fixtures: []Command{{SQL: "FIXTURE"}}, Seed: []Command{{SQL: "SEED"}}, Versions: []Version{{Name: "v1", Migrations: []Command{{SQL: "CREATE V1"}}, Assertions: []Assertion{{Name: "v1 exists", SQL: "SELECT", Want: 1, File: "case.sql", Line: 10}}}, {Name: "v2", Migrations: []Command{{SQL: "MIGRATE V2"}}, Plan: []Command{{SQL: "APPLY PLAN V2"}}, Assertions: []Assertion{{Name: "v2 exists", SQL: "SELECT", Want: 2, File: "case.sql", Line: 20}}}}, Assertions: []Assertion{{Name: "final", SQL: "SELECT", Want: 3, File: "case.sql", Line: 30}}, Teardown: []Command{{SQL: "DROP OUTER"}, {SQL: "DROP INNER"}}}
	got, err := (Runner{Factory: factory{db}}).Run(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Versions) != 2 || got.Assertions != 3 {
		t.Fatalf("result %+v", got)
	}
	tail := db.events[len(db.events)-3:]
	want := []string{"DROP INNER", "DROP OUTER", "close"}
	for i := range want {
		if tail[i] != want[i] {
			t.Fatalf("cleanup %v", tail)
		}
	}
}
func TestFailureHasSourceAndStillCleans(t *testing.T) {
	db := &fakeDB{fail: "BAD"}
	_, err := (Runner{Factory: factory{db}}).Run(context.Background(), Case{Name: "fail", Setup: []Command{{SQL: "BAD", File: "case.sql", Line: 17}}, Teardown: []Command{{SQL: "DROP"}}})
	if err == nil || !strings.Contains(err.Error(), "case.sql:17") {
		t.Fatalf("error %v", err)
	}
	if got := db.events[len(db.events)-2:]; got[0] != "DROP" || got[1] != "close" {
		t.Fatalf("cleanup %v", got)
	}
}
func TestTimeoutStillCleans(t *testing.T) {
	db := &fakeDB{block: true}
	_, err := (Runner{Factory: factory{db}, CleanupTimeout: time.Millisecond}).Run(context.Background(), Case{Name: "timeout", Timeout: time.Millisecond, Setup: []Command{{SQL: "WAIT"}}, Teardown: []Command{{SQL: "DROP"}}})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v", err)
	}
	if db.events[len(db.events)-1] != "close" {
		t.Fatalf("events %v", db.events)
	}
}

func TestCancellationStillCleans(t *testing.T) {
	db := &fakeDB{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := (Runner{Factory: factory{db}}).Run(ctx, Case{Name: "cancel", Setup: []Command{{SQL: "NEVER"}}, Teardown: []Command{{SQL: "DROP"}}})
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("error %v", err)
	}
	if got := db.events[len(db.events)-2:]; got[0] != "DROP" || got[1] != "close" {
		t.Fatalf("cleanup %v", got)
	}
}
func TestAssertionFailureIdentifiesAssertion(t *testing.T) {
	db := &fakeDB{counts: []int64{4}}
	_, err := (Runner{Factory: factory{db}}).Run(context.Background(), Case{Name: "assert", Assertions: []Assertion{{Name: "row count", SQL: "SELECT", Want: 3, File: "assert.sql", Line: 9, Column: 4}}})
	if err == nil || !strings.Contains(err.Error(), "row count") || !strings.Contains(err.Error(), "assert.sql:9:4") {
		t.Fatalf("error %v", err)
	}
}

func TestAssertionIdentityValidationHappensBeforeOpen(t *testing.T) {
	for _, a := range []Assertion{{SQL: "SELECT", File: "a.sql", Line: 1}, {Name: "named", File: "a.sql", Line: 1}, {Name: "named", SQL: "SELECT", Line: 1}, {Name: "named", SQL: "SELECT", File: "a.sql"}, {Name: "named", SQL: "SELECT", File: "a.sql", Line: 1, Column: -1}} {
		db := &fakeDB{}
		_, err := (Runner{Factory: factory{db}}).Run(context.Background(), Case{Name: "invalid", Assertions: []Assertion{a}})
		if err == nil || len(db.events) != 0 {
			t.Fatalf("assertion=%+v error=%v events=%v", a, err, db.events)
		}
	}
}

type cleanupDB struct{ events []string }

func (d *cleanupDB) Exec(ctx context.Context, q string) error {
	d.events = append(d.events, q)
	switch q {
	case "PRIMARY":
		return errors.New("primary failure")
	case "HANG":
		<-ctx.Done()
		return ctx.Err()
	case "CLEANUP FAIL":
		return errors.New("cleanup failure")
	}
	return nil
}
func (d *cleanupDB) QueryCount(context.Context, string, ...any) (int64, error) { return 0, nil }
func (d *cleanupDB) Close(context.Context) error                               { d.events = append(d.events, "close"); return nil }

type cleanupFactory struct{ db *cleanupDB }

func (f cleanupFactory) OpenIsolated(context.Context, string) (Database, error) { return f.db, nil }

func TestEveryCleanupGetsFreshBudgetAndErrorsAreJoined(t *testing.T) {
	db := &cleanupDB{}
	_, err := (Runner{Factory: cleanupFactory{db}, CleanupTimeout: time.Millisecond}).Run(context.Background(), Case{Name: "cleanup-errors", Setup: []Command{{SQL: "PRIMARY"}}, Teardown: []Command{{SQL: "OK"}, {SQL: "CLEANUP FAIL"}, {SQL: "HANG"}}})
	if err == nil || !strings.Contains(err.Error(), "primary failure") || !strings.Contains(err.Error(), "cleanup failure") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error %v", err)
	}
	want := []string{"PRIMARY", "HANG", "CLEANUP FAIL", "OK", "close"}
	if len(db.events) != len(want) {
		t.Fatalf("events %v", db.events)
	}
	for i := range want {
		if db.events[i] != want[i] {
			t.Fatalf("events %v", db.events)
		}
	}
}
