package postgres

import (
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"autosql/pkg/schema"

	"github.com/jackc/pgx/v5"
)

func TestInspectURLIntegration(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatalf("connect fixture database: %v", err)
	}
	defer conn.Close(context.Background())
	const fixture = `
drop schema if exists autosql_inspect cascade;
create schema autosql_inspect;
comment on schema autosql_inspect is 'integration schema';
create extension hstore with schema autosql_inspect;
create type autosql_inspect.status as enum ('new','done');
create domain autosql_inspect.positive_int as integer check (value > 0);
create type autosql_inspect.address as (street text, zip integer);
create sequence autosql_inspect.ticket_seq start 10 increment 2;
create table autosql_inspect.teams (
  id integer primary key,
  name text not null unique
);
create table autosql_inspect.users (
  id integer generated always as identity,
  team_id integer references autosql_inspect.teams(id),
  state autosql_inspect.status not null default 'new',
  score autosql_inspect.positive_int,
  email text,
  constraint users_pkey primary key(id),
  constraint users_email_check check (position('@' in email) > 1),
  constraint users_email_unique unique(email)
);
comment on table autosql_inspect.users is 'application users';
comment on column autosql_inspect.users.email is 'login address';
create index users_state_idx on autosql_inspect.users(state) where state = 'new';
create view autosql_inspect.active_users as select id,email from autosql_inspect.users where state='new';
create materialized view autosql_inspect.user_counts as select state,count(*) n from autosql_inspect.users group by state;
create function autosql_inspect.touch_user() returns trigger language plpgsql as $$ begin new.email=lower(new.email); return new; end $$;
create trigger users_touch before insert or update on autosql_inspect.users for each row execute function autosql_inspect.touch_user();
create function autosql_inspect.user_count() returns bigint language sql stable as $$ select count(*) from autosql_inspect.users $$;
create procedure autosql_inspect.noop() language plpgsql as $$ begin null; end $$;
alter table autosql_inspect.users enable row level security;
create policy user_read on autosql_inspect.users for select to public using (true);
`
	if _, err := conn.Exec(ctx, fixture); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = conn.Exec(cleanup, `drop schema if exists autosql_inspect cascade`)
	}()

	opts := Options{Schemas: []string{"autosql_inspect"}}
	first, err := InspectURL(ctx, url, opts)
	if err != nil {
		t.Fatalf("InspectURL: %v", err)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	second, err := InspectURL(ctx, url, opts)
	if err != nil {
		t.Fatalf("second InspectURL: %v", err)
	}
	a, _ := first.MarshalCanonical()
	b, _ := second.MarshalCanonical()
	if string(a) != string(b) {
		t.Fatal("repeated inspection is not byte-for-byte stable")
	}
	want := map[schema.Kind]bool{
		schema.KindSchema: true, schema.KindExtension: true, schema.KindEnum: true, schema.KindDomain: true,
		schema.KindComposite: true, schema.KindSequence: true, schema.KindTable: true,
		schema.KindColumn: true, schema.KindPrimaryKey: true, schema.KindUniqueConstraint: true,
		schema.KindCheckConstraint: true, schema.KindForeignKey: true, schema.KindIndex: true,
		schema.KindView: true, schema.KindMaterializedView: true, schema.KindFunction: true,
		schema.KindProcedure: true, schema.KindTrigger: true, schema.KindPolicy: true,
	}
	explicitDefaultLookingPK := false
	for _, r := range first.Graph.Resources {
		delete(want, r.Kind)
		if r.Kind == schema.KindPrimaryKey && r.Name.Name == "users_pkey" {
			explicitDefaultLookingPK = true
			if r.Annotations["autosql.io/generated-name"] != "" || r.Annotations["autosql.io/name-origin"] != "" {
				t.Fatal("explicit users_pkey was incorrectly trusted as generated")
			}
		}
	}
	if len(want) != 0 {
		t.Fatalf("inspection missing kinds: %#v", want)
	}
	if !explicitDefaultLookingPK {
		t.Fatal("live inspection did not return explicit users_pkey fixture")
	}
	advanced, err := InspectURL(ctx, url, Options{Schemas: []string{"autosql_inspect"}, Advanced: true})
	if err != nil {
		t.Fatalf("advanced InspectURL: %v", err)
	}
	advancedKinds := map[schema.Kind]bool{}
	for _, r := range advanced.Graph.Resources {
		advancedKinds[r.Kind] = true
	}
	if !advancedKinds[schema.KindRole] || !advancedKinds[schema.KindGrant] {
		t.Fatalf("advanced inspection missing roles or grants: %#v", advancedKinds)
	}

	filtered, err := InspectURL(ctx, url, Options{Schemas: []string{"autosql_inspect"}, Include: []string{"table:autosql_inspect.users"}, Exclude: []string{"column:email"}})
	if err != nil {
		t.Fatalf("filtered InspectURL: %v", err)
	}
	if err := filtered.Validate(); err != nil {
		t.Fatalf("filtered document has dangling reference: %v", err)
	}
	for _, r := range filtered.Graph.Resources {
		if r.Kind == schema.KindColumn && strings.EqualFold(r.Name.Name, "email") {
			t.Fatal("excluded column was retained")
		}
	}
}

func TestManagedColumnCastMatrixMatchesTargetPostgres(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	safe := [][2]string{{"smallint", "integer"}, {"smallint", "bigint"}, {"smallint", "numeric"}, {"integer", "bigint"}, {"integer", "numeric"}, {"bigint", "numeric"}, {"real", "double precision"}, {"character varying", "text"}, {"character", "text"}}
	for _, pair := range safe {
		var context string
		err := conn.QueryRow(ctx, `select castcontext::text from pg_cast where castsource=$1::regtype and casttarget=$2::regtype`, pair[0], pair[1]).Scan(&context)
		if err != nil {
			t.Fatalf("cast %s -> %s missing: %v", pair[0], pair[1], err)
		}
		if context != "i" && context != "a" || !safeAssignmentCast(pair[0], pair[1]) {
			t.Fatalf("cast %s -> %s context=%q is inconsistent with capability", pair[0], pair[1], context)
		}
	}
	var unsafeContext string
	if err := conn.QueryRow(ctx, `select coalesce((select castcontext::text from pg_cast where castsource='text'::regtype and casttarget='integer'::regtype),'e')`).Scan(&unsafeContext); err != nil {
		t.Fatal(err)
	}
	if unsafeContext != "e" || safeAssignmentCast("text", "integer") {
		t.Fatalf("text -> integer context=%q must remain explicit-only", unsafeContext)
	}
}
