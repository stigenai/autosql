package compatmatrix

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"autosql/pkg/zdm/backfill"
	"autosql/pkg/zdm/shadowsync"
	"autosql/pkg/zdm/virtualschema"
	"github.com/jackc/pgx/v5"
)

func live(t testing.TB) (context.Context, string, *pgx.Conn, func()) {
	t.Helper()
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	c, e := pgx.Connect(ctx, url)
	if e != nil {
		cancel()
		t.Fatal(e)
	}
	names := []string{"mx_backfill", "mx_v1", "mx_v2", "mx_app"}
	for _, n := range names {
		_, _ = c.Exec(ctx, "drop schema if exists "+pgx.Identifier{n}.Sanitize()+" cascade")
	}
	cleanup := func() {
		for _, n := range names {
			_, _ = c.Exec(context.Background(), "drop schema if exists "+pgx.Identifier{n}.Sanitize()+" cascade")
		}
		c.Close(context.Background())
		cancel()
	}
	return ctx, url, c, cleanup
}
func install(t testing.TB, ctx context.Context, url string, c *pgx.Conn, rows int) {
	t.Helper()
	_, e := c.Exec(ctx, `create schema mx_app;create table mx_app.items(id bigint primary key,old_name text,new_name text)`)
	if e != nil {
		t.Fatal(e)
	}
	for i := 1; i <= rows; i++ {
		if _, e = c.Exec(ctx, `insert into mx_app.items values($1,$2,null)`, i, fmt.Sprintf("NAME%06d", i)); e != nil {
			t.Fatal(e)
		}
	}
	artifact := strings.Repeat("e", 64)
	ss, e := shadowsync.New(artifact, "mx_app", []shadowsync.Table{{Name: "items", Pairs: []shadowsync.Pair{{ID: "name", OldColumn: "old_name", NewColumn: "new_name", Forward: "lower(value)", Reverse: "upper(value)", Lossy: true}}}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = shadowsync.Apply(ctx, shadowsync.Config{URL: url, Target: "matrix", Environment: "test", LockTimeoutMS: 1000}, ss, shadowsync.Policy{AllowLossy: true}); e != nil {
		t.Fatal(e)
	}
	bf, e := backfill.New(artifact, "names", "mx_app", "items", "id", "old_name", "new_name", "lower(value)")
	if e != nil {
		t.Fatal(e)
	}
	if _, e = backfill.Run(ctx, backfill.Config{URL: url, Schema: "mx_backfill", Target: "matrix", Environment: "test", BatchSize: 37, MaxRetries: 3, LockTimeoutMS: 500, StatementTimeoutMS: 5000, MaxRowsPerSecond: 100000, Backoff: time.Millisecond}, bf); e != nil {
		t.Fatal(e)
	}
	view1 := virtualschema.TableView{Name: "items", PhysicalTable: "items", Columns: []virtualschema.ColumnView{{Name: "id", PhysicalColumn: "id"}, {Name: "name", PhysicalColumn: "old_name"}}}
	view2 := virtualschema.TableView{Name: "items", PhysicalTable: "items", Columns: []virtualschema.ColumnView{{Name: "id", PhysicalColumn: "id"}, {Name: "name", PhysicalColumn: "new_name"}}}
	vs, e := virtualschema.New(artifact, "mx_app", virtualschema.SchemaVersion{Name: "mx_v1", Tables: []virtualschema.TableView{view1}}, virtualschema.SchemaVersion{Name: "mx_v2", Tables: []virtualschema.TableView{view2}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = virtualschema.Apply(ctx, virtualschema.Config{URL: url, Target: "matrix", Environment: "test", LockTimeoutMS: 1000}, vs); e != nil {
		t.Fatal(e)
	}
}

func TestLiveOldNewConcurrentReadWriteEquivalence(t *testing.T) {
	ctx, url, c, cleanup := live(t)
	defer cleanup()
	install(t, ctx, url, c, 200)
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for worker := 0; worker < 8; worker++ {
		worker := worker
		wg.Add(1)
		go func() {
			defer wg.Done()
			conn, e := pgx.Connect(ctx, url)
			if e != nil {
				errs <- e
				return
			}
			defer conn.Close(ctx)
			for i := 0; i < 100; i++ {
				id := int64((worker*100+i)%200 + 1)
				value := fmt.Sprintf("W%02d_%03d", worker, i)
				schema := "mx_v1"
				if (worker+i)%2 == 1 {
					schema = "mx_v2"
					value = strings.ToLower(value)
				}
				if _, e = conn.Exec(ctx, "update "+schema+".items set name=$1 where id=$2", value, id); e != nil {
					errs <- e
					return
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		t.Fatal(e)
	}
	var mismatches int
	if e := c.QueryRow(ctx, `select count(*) from mx_app.items where new_name is distinct from lower(old_name)`).Scan(&mismatches); e != nil || mismatches != 0 {
		t.Fatalf("old/new divergence=%d err=%v", mismatches, e)
	}
	var oldCount, newCount int
	if e := c.QueryRow(ctx, `select (select count(*) from mx_v1.items),(select count(*) from mx_v2.items)`).Scan(&oldCount, &newCount); e != nil || oldCount != newCount || oldCount != 200 {
		t.Fatalf("read mismatch %d/%d %v", oldCount, newCount, e)
	}
}

func TestLiveLongTransactionAndPerformanceBudgets(t *testing.T) {
	ctx, url, c, cleanup := live(t)
	defer cleanup()
	install(t, ctx, url, c, 1000)
	reader, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer reader.Close(ctx)
	tx, e := reader.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if e != nil {
		t.Fatal(e)
	}
	defer tx.Rollback(ctx)
	var before int
	if e = tx.QueryRow(ctx, "select count(*) from mx_v1.items").Scan(&before); e != nil {
		t.Fatal(e)
	}
	start := time.Now()
	for i := 0; i < 500; i++ {
		if _, e = c.Exec(ctx, "update mx_v1.items set name=$1 where id=$2", fmt.Sprintf("LOAD%d", i), i%1000+1); e != nil {
			t.Fatal(e)
		}
	}
	elapsed := time.Since(start)
	if elapsed > 10*time.Second {
		t.Fatalf("trigger write regression: 500 writes took %v", elapsed)
	}
	var stable int
	if e = tx.QueryRow(ctx, "select count(*) from mx_v1.items").Scan(&stable); e != nil || stable != before {
		t.Fatalf("long transaction changed view: %d/%d %v", before, stable, e)
	}
	t.Logf("trigger_writes_per_second=%.2f write_amplification_columns=2 max_budget_seconds=10", 500/elapsed.Seconds())
}

func TestLiveManagedServiceLeastPrivilegeProfile(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	ctx := context.Background()
	admin, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close(ctx)
	role := "mx_zdm_operator"
	var db string
	if e = admin.QueryRow(ctx, "select current_database()").Scan(&db); e != nil {
		t.Fatal(e)
	}
	for _, n := range []string{"mx_lp_backfill", "mx_lp_v1", "mx_lp_v2", "mx_lp_app"} {
		_, _ = admin.Exec(ctx, "drop schema if exists "+pgx.Identifier{n}.Sanitize()+" cascade")
	}
	var roleExists bool
	_ = admin.QueryRow(ctx, "select exists(select 1 from pg_roles where rolname=$1)", role).Scan(&roleExists)
	if roleExists {
		_, _ = admin.Exec(ctx, "drop owned by "+pgx.Identifier{role}.Sanitize()+";drop role "+pgx.Identifier{role}.Sanitize())
	}
	defer func() {
		for _, n := range []string{"mx_lp_backfill", "mx_lp_v1", "mx_lp_v2", "mx_lp_app"} {
			_, _ = admin.Exec(context.Background(), "drop schema if exists "+pgx.Identifier{n}.Sanitize()+" cascade")
		}
		_, _ = admin.Exec(context.Background(), "drop owned by "+pgx.Identifier{role}.Sanitize()+";drop role if exists "+pgx.Identifier{role}.Sanitize())
	}()
	if _, e = admin.Exec(ctx, "create role "+pgx.Identifier{role}.Sanitize()+" login password 'limited_password' nosuperuser nocreatedb nocreaterole noinherit;grant create on database "+pgx.Identifier{db}.Sanitize()+" to "+pgx.Identifier{role}.Sanitize()+";create schema mx_lp_app authorization "+pgx.Identifier{role}.Sanitize()+";create table mx_lp_app.items(id bigint primary key,old_name text,new_name text);alter table mx_lp_app.items owner to "+pgx.Identifier{role}.Sanitize()+";insert into mx_lp_app.items values(1,'ALICE',null)"); e != nil {
		t.Fatal(e)
	}
	pc, e := pgx.ParseConfig(url)
	if e != nil {
		t.Fatal(e)
	}
	pc.User = role
	pc.Password = "limited_password"
	limited := pc.ConnString()
	artifact := strings.Repeat("f", 64)
	ss, e := shadowsync.New(artifact, "mx_lp_app", []shadowsync.Table{{Name: "items", Pairs: []shadowsync.Pair{{ID: "name", OldColumn: "old_name", NewColumn: "new_name", Forward: "lower(value)", Reverse: "upper(value)", Lossy: true}}}})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = shadowsync.Apply(ctx, shadowsync.Config{URL: limited, Target: "limited", Environment: "test", LockTimeoutMS: 500}, ss, shadowsync.Policy{AllowLossy: true}); e != nil {
		t.Fatal(e)
	}
	bf, _ := backfill.New(artifact, "names", "mx_lp_app", "items", "id", "old_name", "new_name", "lower(value)")
	if _, e = backfill.Run(ctx, backfill.Config{URL: limited, Schema: "mx_lp_backfill", Target: "limited", Environment: "test", BatchSize: 10, MaxRetries: 1, LockTimeoutMS: 500, StatementTimeoutMS: 2000}, bf); e != nil {
		t.Fatal(e)
	}
	tv := virtualschema.TableView{Name: "items", PhysicalTable: "items", Columns: []virtualschema.ColumnView{{Name: "id", PhysicalColumn: "id"}, {Name: "name", PhysicalColumn: "new_name"}}}
	vs, _ := virtualschema.New(artifact, "mx_lp_app", virtualschema.SchemaVersion{Name: "mx_lp_v1", Tables: []virtualschema.TableView{tv}}, virtualschema.SchemaVersion{Name: "mx_lp_v2", Tables: []virtualschema.TableView{tv}})
	if _, e = virtualschema.Apply(ctx, virtualschema.Config{URL: limited, Target: "limited", Environment: "test", LockTimeoutMS: 500}, vs); e != nil {
		t.Fatal(e)
	}
	var super, createdb, createrole bool
	if e = admin.QueryRow(ctx, "select rolsuper,rolcreatedb,rolcreaterole from pg_roles where rolname=$1", role).Scan(&super, &createdb, &createrole); e != nil || super || createdb || createrole {
		t.Fatalf("privilege escalation: super=%t createdb=%t createrole=%t err=%v", super, createdb, createrole, e)
	}
}
