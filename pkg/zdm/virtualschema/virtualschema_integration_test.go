package virtualschema

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestLiveVersionSchemasDMLAndSecurity(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	for _, s := range []string{"app_v1", "app_v2", "physical_app"} {
		_, _ = c.Exec(ctx, "drop schema if exists "+q(s)+" cascade")
	}
	defer func() {
		for _, s := range []string{"app_v1", "app_v2", "physical_app"} {
			_, _ = c.Exec(context.Background(), "drop schema if exists "+q(s)+" cascade")
		}
	}()
	_, err = c.Exec(ctx, `create schema physical_app; create table physical_app.accounts(id bigint generated always as identity primary key,name text not null,created_at timestamptz not null default clock_timestamp(),slug text generated always as (lower(name)) stored)`)
	if err != nil {
		t.Fatal(err)
	}
	s := testSpec(t)
	cfg := Config{URL: url, Target: "db1", Environment: "test", LockTimeoutMS: 500}
	st, err := Apply(ctx, cfg, s)
	if err != nil {
		t.Fatal(err)
	}
	if !st.Previous.Exact || !st.Current.Exact {
		t.Fatalf("not exact: %+v", st)
	}
	if len(st.Diagnostics) != 1 || st.Diagnostics[0].Code != "grants_ownership_review" {
		t.Fatalf("missing explicit grants/ownership diagnostic: %+v", st.Diagnostics)
	}
	other := cfg
	other.Target = "other"
	if _, err = Apply(ctx, other, s); err == nil {
		t.Fatal("expected cross-target marker refusal")
	}
	var id int64
	var slug string
	if err = c.QueryRow(ctx, `insert into app_v1.accounts(name) values('Alpha') returning id,slug`).Scan(&id, &slug); err != nil || slug != "alpha" {
		t.Fatalf("old insert returning: %v %s", err, slug)
	}
	var display string
	if err = c.QueryRow(ctx, `select display_name from app_v2.accounts where id=$1`, id).Scan(&display); err != nil || display != "Alpha" {
		t.Fatalf("new read: %v %s", err, display)
	}
	if err = c.QueryRow(ctx, `update app_v2.accounts set display_name='Beta' where id=$1 returning slug`, id).Scan(&slug); err != nil || slug != "beta" {
		t.Fatalf("new update returning: %v %s", err, slug)
	}
	if err = c.QueryRow(ctx, `select name from app_v1.accounts where id=$1`, id).Scan(&display); err != nil || display != "Beta" {
		t.Fatalf("old read: %v %s", err, display)
	}
	if _, err = c.Exec(ctx, `delete from app_v1.accounts where id=$1`, id); err != nil {
		t.Fatal(err)
	}
	var n int
	if err = c.QueryRow(ctx, `select count(*) from physical_app.accounts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("delete: %v %d", err, n)
	}
	var canCreate bool
	if err = c.QueryRow(ctx, `select has_schema_privilege('public','app_v1','create')`).Scan(&canCreate); err != nil || canCreate {
		t.Fatalf("public CREATE not revoked: %v", err)
	}
	st2, err := Apply(ctx, cfg, s)
	if err != nil || !st2.Current.Exact {
		t.Fatalf("idempotent: %v", err)
	}
	if _, err = c.Exec(ctx, `create or replace view app_v2.accounts as select created_at,name||'' as display_name,id,slug from physical_app.accounts`); err != nil {
		t.Fatal(err)
	}
	drift, err := Inspect(ctx, cfg, s)
	if err != nil || drift.Current.Exact {
		t.Fatalf("view definition drift not detected: %v", err)
	}
	if _, err = Apply(ctx, cfg, s); err != nil {
		t.Fatalf("marked drift should repair: %v", err)
	}
	if _, err = c.Exec(ctx, `comment on view app_v2.accounts is 'attacker'`); err != nil {
		t.Fatal(err)
	}
	if _, err = Apply(ctx, cfg, s); err == nil {
		t.Fatal("expected drift/collision refusal")
	}
}

func TestLiveAtomicCollisionRollback(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	for _, s := range []string{"atomic_v1", "atomic_v2", "atomic_physical"} {
		_, _ = c.Exec(ctx, "drop schema if exists "+q(s)+" cascade")
	}
	defer func() {
		for _, s := range []string{"atomic_v1", "atomic_v2", "atomic_physical"} {
			_, _ = c.Exec(context.Background(), "drop schema if exists "+q(s)+" cascade")
		}
	}()
	_, err = c.Exec(ctx, `create schema atomic_physical;create table atomic_physical.t(id bigint primary key);create schema atomic_v2`)
	if err != nil {
		t.Fatal(err)
	}
	tv := TableView{Name: "t", PhysicalTable: "t", Columns: []ColumnView{{Name: "id", PhysicalColumn: "id"}}}
	spec, err := New(strings.Repeat("b", 64), "atomic_physical", SchemaVersion{Name: "atomic_v1", Tables: []TableView{tv}}, SchemaVersion{Name: "atomic_v2", Tables: []TableView{tv}})
	if err != nil {
		t.Fatal(err)
	}
	_, err = Apply(ctx, Config{URL: url, Target: "atomic", Environment: "test", LockTimeoutMS: 500}, spec)
	if err == nil {
		t.Fatal("expected collision")
	}
	var exists bool
	if err = c.QueryRow(ctx, `select exists(select 1 from pg_namespace where nspname='atomic_v1')`).Scan(&exists); err != nil || exists {
		t.Fatalf("partial installation survived rollback: %v", err)
	}
}
