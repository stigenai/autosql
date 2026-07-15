package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"strings"
	"testing"
	"time"

	"autosql/pkg/schema"

	"github.com/jackc/pgx/v5"
)

func TestInspectURLConcurrentIndexChurn(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	nonce := make([]byte, 6)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	name := "autosql_churn_" + hex.EncodeToString(nonce)
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err = conn.Exec(ctx, "create schema "+pgx.Identifier{name}.Sanitize()+"; create table "+pgx.Identifier{name, "items"}.Sanitize()+" (id bigint primary key, value text)"); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), "drop schema if exists "+pgx.Identifier{name}.Sanitize()+" cascade")

	churnDone := make(chan error, 1)
	go func() {
		churn, connectErr := pgx.Connect(ctx, url)
		if connectErr != nil {
			churnDone <- connectErr
			return
		}
		defer churn.Close(context.Background())
		index := pgx.Identifier{"items_value_idx"}.Sanitize()
		qualifiedIndex := pgx.Identifier{name, "items_value_idx"}.Sanitize()
		table := pgx.Identifier{name, "items"}.Sanitize()
		for i := 0; i < 100; i++ {
			if _, execErr := churn.Exec(ctx, "create index "+index+" on "+table+" (value)"); execErr != nil {
				churnDone <- execErr
				return
			}
			if _, execErr := churn.Exec(ctx, "drop index "+qualifiedIndex); execErr != nil {
				churnDone <- execErr
				return
			}
		}
		churnDone <- nil
	}()
	for i := 0; i < 40; i++ {
		doc, inspectErr := InspectURL(ctx, url, Options{Schemas: []string{name}})
		if inspectErr != nil {
			t.Fatalf("inspection under index churn %d: %v", i, inspectErr)
		}
		if inspectErr = doc.Validate(); inspectErr != nil {
			t.Fatalf("invalid inspection under index churn %d: %v", i, inspectErr)
		}
	}
	if err = <-churnDone; err != nil {
		t.Fatalf("index churn: %v", err)
	}
}

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
create type autosql_inspect."Mood" as enum ('good','bad');
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
  state_history autosql_inspect.status[],
  mood autosql_inspect."Mood",
  moods autosql_inspect."Mood"[],
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
alter default privileges in schema autosql_inspect grant select on tables to public;
`
	if _, err := conn.Exec(ctx, fixture); err != nil {
		t.Fatalf("create fixture: %v", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = conn.Exec(cleanup, `alter default privileges in schema autosql_inspect revoke select on tables from public; drop schema if exists autosql_inspect cascade`)
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
	typeIDs := map[string]string{}
	columnUses := map[string]string{}
	for _, r := range first.Graph.Resources {
		delete(want, r.Kind)
		if r.Kind == schema.KindEnum {
			typeIDs[r.Name.Name] = r.ID
		}
		if r.Kind == schema.KindColumn {
			for _, dep := range r.Dependencies {
				if dep.Type == schema.DependencyUses {
					columnUses[r.Name.Name] = dep.Target
				}
			}
		}
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
	for column, typ := range map[string]string{"state": "status", "state_history": "status", "mood": "Mood", "moods": "Mood"} {
		if columnUses[column] == "" || columnUses[column] != typeIDs[typ] {
			t.Errorf("column %s uses=%q, want canonical %s dependency %q", column, columnUses[column], typ, typeIDs[typ])
		}
	}
	advanced, err := InspectURL(ctx, url, Options{Schemas: []string{"autosql_inspect"}, Advanced: true})
	if err != nil {
		t.Fatalf("advanced InspectURL: %v", err)
	}
	advancedKinds := map[schema.Kind]bool{}
	for _, r := range advanced.Graph.Resources {
		advancedKinds[r.Kind] = true
	}
	if !advancedKinds[schema.KindRole] || !advancedKinds[schema.KindGrant] || !advancedKinds[schema.KindDefaultPrivilege] {
		t.Fatalf("advanced inspection missing roles, grants, or default privileges: %#v", advancedKinds)
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
