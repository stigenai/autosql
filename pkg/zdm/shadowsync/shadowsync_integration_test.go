package shadowsync

import (
	"context"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"autosql/pkg/zdm/virtualschema"
	"github.com/jackc/pgx/v5"
)

func TestLiveBidirectionalConcurrentCompositionAndLifecycle(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	c, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close(ctx)
	for _, s := range []string{"sync_v1", "sync_v2", "sync_physical"} {
		_, _ = c.Exec(ctx, "drop schema if exists "+q(s)+" cascade")
	}
	defer func() {
		for _, s := range []string{"sync_v1", "sync_v2", "sync_physical"} {
			_, _ = c.Exec(context.Background(), "drop schema if exists "+q(s)+" cascade")
		}
	}()
	_, e = c.Exec(ctx, `create schema sync_physical;create table sync_physical.accounts(id bigint generated always as identity primary key,old_name text,new_name text,old_code text,new_code text)`)
	if e != nil {
		t.Fatal(e)
	}
	s := sample(t)
	cfg := Config{URL: url, Target: "db", Environment: "test", LockTimeoutMS: 500}
	p := Policy{AllowLossy: true}
	st, e := Apply(ctx, cfg, s, p)
	if e != nil {
		t.Fatal(e)
	}
	if !st.Installed || st.RollbackEligible {
		t.Fatalf("status: %+v", st)
	}
	if len(st.Tables) != 1 || st.Tables[0].Pairs != 2 || st.Tables[0].AssignmentsPerWrite != 2 {
		t.Fatalf("write amplification evidence: %+v", st.Tables)
	}
	old := virtualschema.SchemaVersion{Name: "sync_v1", Tables: []virtualschema.TableView{{Name: "accounts", PhysicalTable: "accounts", Columns: []virtualschema.ColumnView{{Name: "code", PhysicalColumn: "old_code"}, {Name: "id", PhysicalColumn: "id"}, {Name: "name", PhysicalColumn: "old_name"}}}}}
	cur := virtualschema.SchemaVersion{Name: "sync_v2", Tables: []virtualschema.TableView{{Name: "accounts", PhysicalTable: "accounts", Columns: []virtualschema.ColumnView{{Name: "code", PhysicalColumn: "new_code"}, {Name: "id", PhysicalColumn: "id"}, {Name: "name", PhysicalColumn: "new_name"}}}}}
	vs, e := virtualschema.New(strings.Repeat("a", 64), "sync_physical", old, cur)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = virtualschema.Apply(ctx, virtualschema.Config{URL: url, Target: "db", Environment: "test", LockTimeoutMS: 500}, vs); e != nil {
		t.Fatal(e)
	}
	var id int64
	if e = c.QueryRow(ctx, `insert into sync_v1.accounts(name,code) values('Alice','ab') returning id`).Scan(&id); e != nil {
		t.Fatal(e)
	}
	var name, code string
	if e = c.QueryRow(ctx, `select name,code from sync_v2.accounts where id=$1`, id).Scan(&name, &code); e != nil || name != "alice" || code != "AB" {
		t.Fatalf("forward: %v %q %q", e, name, code)
	}
	if _, e = c.Exec(ctx, `update sync_v2.accounts set name='bob',code='ZZ' where id=$1`, id); e != nil {
		t.Fatal(e)
	}
	if e = c.QueryRow(ctx, `select name,code from sync_v1.accounts where id=$1`, id).Scan(&name, &code); e != nil || name != "BOB" || code != "zz" {
		t.Fatalf("reverse: %v %q %q", e, name, code)
	}
	if _, e = c.Exec(ctx, `update sync_v1.accounts set name=null,code=null where id=$1`, id); e != nil {
		t.Fatal(e)
	}
	var nulls bool
	if e = c.QueryRow(ctx, `select new_name is null and new_code is null from sync_physical.accounts where id=$1`, id).Scan(&nulls); e != nil || !nulls {
		t.Fatalf("null propagation: %v", e)
	}
	if _, e = c.Exec(ctx, `update sync_physical.accounts set old_name='x',new_name='different' where id=$1`, id); e == nil {
		t.Fatal("conflicting dual write accepted")
	}
	// Concurrent writes to distinct rows exercise one trigger with no recursive UPDATE amplification.
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			cc, e := pgx.Connect(ctx, url)
			if e != nil {
				errs <- e
				return
			}
			defer cc.Close(ctx)
			schema := "sync_v1"
			if i%2 == 1 {
				schema = "sync_v2"
			}
			_, e = cc.Exec(ctx, "insert into "+q(schema, "accounts")+"(name,code) values($1,$2)", "N", "c")
			errs <- e
		}(i)
	}
	wg.Wait()
	close(errs)
	for e = range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var n int
	if e = c.QueryRow(ctx, `select count(*) from sync_physical.accounts where old_name='N' and new_name='n' and old_code='c' and new_code='C'`).Scan(&n); e != nil || n != 10 {
		t.Fatalf("old concurrent propagation %v %d", e, n)
	}
	trigger := st.Tables[0].Trigger
	if _, e = c.Exec(ctx, "alter table sync_physical.accounts disable trigger "+q(trigger)); e != nil {
		t.Fatal(e)
	}
	drift, e := Inspect(ctx, cfg, s, p)
	if e != nil || drift.Tables[0].Exact {
		t.Fatalf("disabled trigger drift not detected: %v", e)
	}
	if _, e = Apply(ctx, cfg, s, p); e == nil {
		t.Fatal("drifted trigger accepted")
	}
	if _, e = c.Exec(ctx, "alter table sync_physical.accounts enable trigger "+q(trigger)); e != nil {
		t.Fatal(e)
	}
	if _, e = Remove(ctx, cfg, s, p); e != nil {
		t.Fatal(e)
	}
	after, e := Inspect(ctx, cfg, s, p)
	if e != nil || after.Installed {
		t.Fatalf("remove: %v %+v", e, after)
	}
	if _, e = c.Exec(ctx, `insert into sync_physical.accounts(old_name) values('unsynced')`); e != nil {
		t.Fatal(e)
	}
	var isnull bool
	if e = c.QueryRow(ctx, `select new_name is null from sync_physical.accounts where old_name='unsynced'`).Scan(&isnull); e != nil || !isnull {
		t.Fatalf("trigger removal ineffective: %v", e)
	}
}

func TestLiveTransformErrorAndNonReversibleWrite(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	ctx := context.Background()
	c, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close(ctx)
	_, _ = c.Exec(ctx, `drop schema if exists sync_errors cascade`)
	defer c.Exec(context.Background(), `drop schema if exists sync_errors cascade`)
	if _, e = c.Exec(ctx, `create schema sync_errors;create table sync_errors.values(id bigint primary key,old_value text,new_value integer)`); e != nil {
		t.Fatal(e)
	}
	s, e := New(strings.Repeat("d", 64), "sync_errors", []Table{{Name: "values", Pairs: []Pair{{ID: "p01", OldColumn: "old_value", NewColumn: "new_value", Forward: "value::integer", Lossy: true}}}})
	if e != nil {
		t.Fatal(e)
	}
	cfg := Config{URL: url, Target: "errors", Environment: "test", LockTimeoutMS: 500}
	p := Policy{AllowNonReversible: true, AllowLossy: true}
	if _, e = Apply(ctx, cfg, s, p); e != nil {
		t.Fatal(e)
	}
	if _, e = c.Exec(ctx, `insert into sync_errors.values(id,old_value) values(1,'not-a-number')`); e == nil {
		t.Fatal("transform error did not abort write")
	}
	var n int
	if e = c.QueryRow(ctx, `select count(*) from sync_errors.values`).Scan(&n); e != nil || n != 0 {
		t.Fatalf("failed transform mutated row: %v %d", e, n)
	}
	if _, e = c.Exec(ctx, `insert into sync_errors.values(id,new_value) values(2,42)`); e == nil {
		t.Fatal("non-reversible new-side write accepted")
	}
	if _, e = Remove(ctx, cfg, s, p); e != nil {
		t.Fatal(e)
	}
}

func TestLiveInstallationIsAtomic(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	ctx := context.Background()
	c, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close(ctx)
	_, _ = c.Exec(ctx, `drop schema if exists sync_atomic cascade`)
	defer c.Exec(context.Background(), `drop schema if exists sync_atomic cascade`)
	if _, e = c.Exec(ctx, `create schema sync_atomic;create table sync_atomic.a(old_value text,new_value text);create table sync_atomic.b(old_value text)`); e != nil {
		t.Fatal(e)
	}
	s, e := New(strings.Repeat("f", 64), "sync_atomic", []Table{{Name: "a", Pairs: []Pair{{ID: "p01", OldColumn: "old_value", NewColumn: "new_value", Forward: "value", Reverse: "value"}}}, {Name: "b", Pairs: []Pair{{ID: "p02", OldColumn: "old_value", NewColumn: "new_value", Forward: "value", Reverse: "value"}}}})
	if e != nil {
		t.Fatal(e)
	}
	cfg := Config{URL: url, Target: "atomic", Environment: "test", LockTimeoutMS: 500}
	if _, e = Apply(ctx, cfg, s, Policy{}); e == nil {
		t.Fatal("missing column accepted")
	}
	var n int
	if e = c.QueryRow(ctx, `select count(*) from pg_trigger where tgrelid='sync_atomic.a'::regclass and not tgisinternal`).Scan(&n); e != nil || n != 0 {
		t.Fatalf("partial trigger survived rollback: %v %d", e, n)
	}
}
