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
	c := Case{Name: "upgrade", Variables: map[string]string{"schema": "case_a"}, Setup: []Command{{SQL: "CREATE SCHEMA ${schema}"}}, Fixtures: []Command{{SQL: "FIXTURE"}}, Seed: []Command{{SQL: "SEED"}}, Versions: []Version{{Name: "v1", Migrations: []Command{{SQL: "CREATE V1"}}, Assertions: []Assertion{{Name: "v1 exists", SQL: "SELECT", Want: 1}}}, {Name: "v2", Migrations: []Command{{SQL: "MIGRATE V2"}}, Plan: []Command{{SQL: "APPLY PLAN V2"}}, Assertions: []Assertion{{Name: "v2 exists", SQL: "SELECT", Want: 2}}}}, Assertions: []Assertion{{Name: "final", SQL: "SELECT", Want: 3}}, Teardown: []Command{{SQL: "DROP OUTER"}, {SQL: "DROP INNER"}}}
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
	_, err := (Runner{Factory: factory{db}}).Run(context.Background(), Case{Name: "assert", Assertions: []Assertion{{Name: "row count", SQL: "SELECT", Want: 3, File: "assert.sql", Line: 9}}})
	if err == nil || !strings.Contains(err.Error(), "row count") || !strings.Contains(err.Error(), "assert.sql:9") {
		t.Fatalf("error %v", err)
	}
}
