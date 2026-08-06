package postgres

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"autosql/pkg/plan"
	"autosql/pkg/schema"
	"autosql/pkg/source"

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
drop schema if exists autosql_fresh_cell cascade;
create schema autosql_inspect;
create schema autosql_fresh_cell;
create table autosql_fresh_cell.widgets (
  id bigint not null,
  label character varying(255) not null default 'widget'
);
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
create function autosql_inspect.lifecycle_state_to_v2(value autosql_inspect.status) returns text language sql immutable as $$ select value::text $$;
create function autosql_inspect."Lifecycle To V2"(value text) returns text language sql immutable as $$ select value $$;
create table autosql_inspect.users (
  id integer generated always as identity,
  team_id integer references autosql_inspect.teams(id),
  state autosql_inspect.status not null default 'new',
  state_v2 text generated always as (autosql_inspect.lifecycle_state_to_v2(state)) stored,
  "Quoted State" text,
  "Quoted V2" text generated always as (autosql_inspect."Lifecycle To V2"("Quoted State")) stored,
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
	// hstore carries its own extension comment, so 244 explicit column comments
	// plus the four inspected fixture comments produce the reported 248 objects.
	columns := make([]string, 244)
	var comments strings.Builder
	for i := range columns {
		name := fmt.Sprintf("comment_%03d", i+1)
		columns[i] = pgx.Identifier{name}.Sanitize() + " text"
		comments.WriteString("comment on column autosql_inspect.comment_inventory.")
		comments.WriteString(pgx.Identifier{name}.Sanitize())
		comments.WriteString(" is 'inventory';\n")
	}
	if _, err := conn.Exec(ctx, "create table autosql_inspect.comment_inventory ("+strings.Join(columns, ",")+");\n"+comments.String()); err != nil {
		t.Fatalf("create complete comment inventory: %v", err)
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_, _ = conn.Exec(cleanup, `alter default privileges in schema autosql_inspect revoke select on tables from public; drop schema if exists autosql_inspect cascade; drop schema if exists autosql_fresh_cell cascade`)
	}()

	opts := Options{Schemas: []string{"autosql_inspect"}}
	first, err := InspectURL(ctx, url, opts)
	if err != nil {
		t.Fatalf("InspectURL: %v", err)
	}
	if err := first.Validate(); err != nil {
		t.Fatalf("Validate: %v", err)
	}
	commentCount := 0
	for _, resource := range first.Graph.Resources {
		if resource.Annotations["comment"] != "" {
			commentCount++
		}
	}
	if commentCount != 248 {
		t.Fatalf("complete fixture has %d comments, want 248", commentCount)
	}
	resourceByKey := map[string]schema.Resource{}
	for _, resource := range first.Graph.Resources {
		resourceByKey[string(resource.Kind)+":"+resource.Name.Name] = resource
	}
	for generatedName, prerequisites := range map[string][]string{
		"state_v2":  {resourceByKey["column:state"].ID, resourceByKey["function:lifecycle_state_to_v2(value autosql_inspect.status)"].ID},
		"Quoted V2": {resourceByKey["column:Quoted State"].ID, resourceByKey["function:Lifecycle To V2(value text)"].ID},
	} {
		generated := resourceByKey["column:"+generatedName]
		actual := map[string]bool{}
		for _, dependency := range generated.Dependencies {
			if dependency.Type == schema.DependencyReferences {
				actual[dependency.Target] = true
			}
		}
		for _, prerequisite := range prerequisites {
			if prerequisite == "" || !actual[prerequisite] {
				t.Errorf("generated column %q missing exact dependency %q: %+v", generatedName, prerequisite, generated.Dependencies)
			}
		}
		if len(actual) != len(prerequisites) {
			t.Errorf("generated column %q has non-exact reference dependencies: %+v", generatedName, generated.Dependencies)
		}
	}
	for _, r := range first.Graph.Resources {
		if r.Kind != schema.KindColumn {
			continue
		}
		var spec map[string]any
		if err := json.Unmarshal(r.Spec, &spec); err != nil {
			t.Fatalf("decode inspected column %s: %v", r.Name.String(), err)
		}
		if _, ok := spec["position"]; ok {
			t.Fatalf("inspected column %s emitted legacy position: %#v", r.Name.String(), spec)
		}
		if ordinal, ok := spec["ordinal"].(float64); !ok || ordinal < 1 {
			t.Fatalf("inspected column %s missing canonical ordinal: %#v", r.Name.String(), spec)
		}
	}
	// The complete fixture includes comments, identity columns, types, views,
	// policies, and complex inspected semantics. An unchanged round-trip must
	// remain a valid no-op even when bounded fresh-provisioning grammar reports
	// a diagnostic; every advertised PostgreSQL kind is managed, so none may be
	// misclassified as an external prerequisite.
	hcl, err := source.FormatHCL(first)
	if err != nil {
		t.Fatalf("FormatHCL inspected schema: %v", err)
	}
	reloaded, err := source.LoadContext(ctx, source.Input{URI: "inspected.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatalf("reload inspected HCL: %v", err)
	}
	current, err := New().Normalize(ctx, first)
	if err != nil {
		t.Fatalf("normalize inspected schema: %v", err)
	}
	desired, err := New().Normalize(ctx, reloaded)
	if err != nil {
		t.Fatalf("normalize reloaded HCL: %v", err)
	}
	report, err := PreflightProvisioning(ctx, desired, nil)
	if err != nil {
		t.Fatalf("preflight complete inspected cell: %v", err)
	}
	for _, diagnostic := range report.Diagnostics {
		if diagnostic.External || diagnostic.Class == "external_prerequisite" {
			t.Fatalf("managed PostgreSQL capability reported as external: %+v", diagnostic)
		}
		if diagnostic.Class == "unsupported_semantic" && diagnostic.Field == "generated" {
			t.Fatalf("complete fixture retained generated blocker: %+v", diagnostic)
		}
	}
	diff, err := schema.Diff(current, desired, schema.DiffOptions{})
	if err != nil {
		t.Fatalf("diff inspected HCL: %v", err)
	}
	converged, err := plan.Build(ctx, New(), current, desired, plan.Options{})
	if err != nil {
		encoded, _ := diff.MarshalCanonical()
		t.Fatalf("plan inspected HCL convergence: %v\n%s", err, encoded)
	}
	if len(converged.Changes.Changes) != 0 || len(converged.Steps) != 0 {
		t.Fatalf("inspected HCL did not converge: changes=%d steps=%d", len(converged.Changes.Changes), len(converged.Steps))
	}
	fresh, err := InspectURL(ctx, url, Options{Schemas: []string{"autosql_fresh_cell"}})
	if err != nil {
		t.Fatalf("inspect fresh-cell fixture: %v", err)
	}
	freshHCL, err := source.FormatHCL(fresh)
	if err != nil {
		t.Fatalf("format fresh-cell HCL: %v", err)
	}
	fresh, err = source.LoadContext(ctx, source.Input{URI: "fresh-cell.hcl", Format: source.FormatHCLSource, Data: freshHCL})
	if err != nil {
		t.Fatalf("reload fresh-cell HCL: %v", err)
	}
	fresh, err = New().Normalize(ctx, fresh)
	if err != nil {
		t.Fatalf("normalize fresh-cell fixture: %v", err)
	}
	empty := schema.Document{Version: schema.SchemaVersion, Annotations: map[string]string{"dialect": "postgresql"}}
	freshPlan, err := plan.Build(ctx, New(), empty, fresh, plan.Options{})
	if err != nil {
		t.Fatalf("plan fresh-cell parameterized types: %v", err)
	}
	if len(freshPlan.Steps) == 0 {
		t.Fatal("fresh-cell parameterized type plan has no steps")
	}
	var freshSQL strings.Builder
	for _, step := range freshPlan.Steps {
		freshSQL.WriteString(step.SQL)
		freshSQL.WriteByte('\n')
	}
	if !strings.Contains(freshSQL.String(), "character varying(255)") {
		t.Fatalf("fresh-cell plan dropped inspected type modifier:\n%s", freshSQL.String())
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

func TestDefaultExpressionMatrixAdoptProvisionAndIncrement(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	const namespace = "autosql_default_matrix"
	const fixture = `
drop schema if exists autosql_default_matrix cascade;
create schema autosql_default_matrix;
create type autosql_default_matrix.job_status as enum ('pending','done');
create domain autosql_default_matrix.positive_int as integer check (value > 0);
create sequence autosql_default_matrix.widget_id_seq start with 10 increment by 2;
create table autosql_default_matrix.widgets (
  seq_id bigint default nextval('autosql_default_matrix.widget_id_seq'::regclass),
  serial_id serial,
  price numeric(10,2) default 0.00,
  metadata jsonb default '{}'::jsonb,
  items jsonb default '[]'::jsonb,
  external_id uuid default '550e8400-e29b-41d4-a716-446655440000'::uuid,
  client_network cidr default '10.0.0.0/8'::cidr,
  client_address inet default '192.0.2.1/24'::inet,
  client_mac macaddr default '08:00:2b:01:02:03'::macaddr,
  generated_id uuid default gen_random_uuid(),
  generated_text_id text default (gen_random_uuid())::text,
  state autosql_default_matrix.job_status default 'pending'::autosql_default_matrix.job_status,
  positive autosql_default_matrix.positive_int default 5,
  text_state text default 'pending'::text,
  empty_tags text[] default '{}'::text[],
  tags text[] default array['a'::text,'b'::text],
  numbers integer[] default array[1,2],
  switches boolean[] default array[true,false],
  business_date date default current_date,
  local_clock time(3) default localtime(3),
  zoned_clock time(2) with time zone default current_time(2),
  created_at timestamptz default now(),
  local_stamp timestamp(2) default localtimestamp(2),
  utc_stamp timestamp default timezone('utc'::text,now()),
  delay interval default '00:05:00'::interval,
  code character(4) default 'x',
  flags bit(4) default '1010',
  arithmetic_add integer default (1 + 2),
  arithmetic_sub integer default (10 - (2 - 1)),
  arithmetic_mul integer default (2 * 3),
  arithmetic_div integer default (10 / 2),
  arithmetic_mod integer default (10 % 3),
  arithmetic_unary integer default -(1 + 2),
  dbos_updated_at bigint default (extract(epoch from now()) * 1000)::bigint,
  numeric_mod numeric default (5.5 % 2),
  real_add real default (1::real + 2::real),
  bigint_promoted bigint default (2147483647::bigint + 1)
);`
	if _, err = conn.Exec(ctx, fixture); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop schema if exists autosql_default_matrix cascade`)

	inspected, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	expectedDefaults := map[string]string{
		"seq_id":            "nextval('autosql_default_matrix.widget_id_seq'::regclass)",
		"serial_id":         "nextval('autosql_default_matrix.widgets_serial_id_seq'::regclass)",
		"price":             "0.00",
		"metadata":          "'{}'::jsonb",
		"items":             "'[]'::jsonb",
		"external_id":       "'550e8400-e29b-41d4-a716-446655440000'::uuid",
		"client_network":    "'10.0.0.0/8'::cidr",
		"client_address":    "'192.0.2.1/24'::inet",
		"client_mac":        "'08:00:2b:01:02:03'::macaddr",
		"generated_id":      "gen_random_uuid()",
		"generated_text_id": "(gen_random_uuid())::text",
		"state":             "'pending'::autosql_default_matrix.job_status",
		"positive":          "5",
		"text_state":        "'pending'::text",
		"empty_tags":        "'{}'::text[]",
		"tags":              "ARRAY['a'::text, 'b'::text]",
		"numbers":           "ARRAY[1, 2]",
		"switches":          "ARRAY[true, false]",
		"business_date":     "CURRENT_DATE",
		"local_clock":       "LOCALTIME(3)",
		"zoned_clock":       "CURRENT_TIME(2)",
		"created_at":        "now()",
		"local_stamp":       "LOCALTIMESTAMP(2)",
		"utc_stamp":         "timezone('utc'::text, now())",
		"delay":             "'00:05:00'::interval",
		"code":              "'x'::bpchar",
		"flags":             "'1010'::\"bit\"",
	}
	sequenceIDs := map[string]string{}
	var enumID, domainID string
	for _, resource := range inspected.Graph.Resources {
		switch resource.Kind {
		case schema.KindSequence:
			sequenceIDs[resource.Name.Name] = resource.ID
		case schema.KindEnum:
			enumID = resource.ID
		case schema.KindDomain:
			domainID = resource.ID
		}
	}
	seen := map[string]bool{}
	for _, resource := range inspected.Graph.Resources {
		if resource.Kind != schema.KindColumn {
			continue
		}
		columnSpec := spec(resource)
		if expected, ok := expectedDefaults[resource.Name.Name]; ok {
			seen[resource.Name.Name] = true
			if got := stringValue(columnSpec, "default"); got != expected {
				t.Errorf("inspected default %s=%q want %q", resource.Name.Name, got, expected)
			}
		}
		dependencies := map[schema.DependencyType][]string{}
		for _, dependency := range resource.Dependencies {
			dependencies[dependency.Type] = append(dependencies[dependency.Type], dependency.Target)
		}
		switch resource.Name.Name {
		case "seq_id":
			if !slices.Contains(dependencies[schema.DependencyReferences], sequenceIDs["widget_id_seq"]) {
				t.Fatalf("sequence default dependency=%v want %s", dependencies, sequenceIDs["widget_id_seq"])
			}
		case "serial_id":
			if !slices.Contains(dependencies[schema.DependencyReferences], sequenceIDs["widgets_serial_id_seq"]) {
				t.Fatalf("serial default dependency=%v want %s", dependencies, sequenceIDs["widgets_serial_id_seq"])
			}
		case "state":
			if !slices.Contains(dependencies[schema.DependencyUses], enumID) {
				t.Fatalf("enum default dependency=%v want %s", dependencies, enumID)
			}
		case "positive":
			if !slices.Contains(dependencies[schema.DependencyUses], domainID) {
				t.Fatalf("domain default dependency=%v want %s", dependencies, domainID)
			}
		}
	}
	if len(seen) != len(expectedDefaults) {
		t.Fatalf("explicit default assertions seen=%v want=%v", seen, expectedDefaults)
	}

	hcl, err := source.FormatHCL(inspected)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := source.LoadContext(ctx, source.Input{URI: "default-matrix.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	current, err := New().Normalize(ctx, inspected)
	if err != nil {
		t.Fatal(err)
	}
	operatorDefaults := map[string]string{
		"arithmetic_add":   "(1 OPERATOR(pg_catalog.+) 2)",
		"arithmetic_sub":   "(10 OPERATOR(pg_catalog.-) (2 OPERATOR(pg_catalog.-) 1))",
		"arithmetic_mul":   "(2 OPERATOR(pg_catalog.*) 3)",
		"arithmetic_div":   "(10 OPERATOR(pg_catalog./) 2)",
		"arithmetic_mod":   "(10 OPERATOR(pg_catalog.%) 3)",
		"arithmetic_unary": "(OPERATOR(pg_catalog.-) (1 OPERATOR(pg_catalog.+) 2))",
		"dbos_updated_at":  "(extract(epoch from CURRENT_TIMESTAMP) OPERATOR(pg_catalog.*) 1000::numeric)::bigint",
		"numeric_mod":      "(5.5 OPERATOR(pg_catalog.%) 2::numeric)",
		"real_add":         "(1::real OPERATOR(pg_catalog.+) 2::real)",
		"bigint_promoted":  "(2147483647::bigint OPERATOR(pg_catalog.+) 1)",
	}
	for _, resource := range current.Graph.Resources {
		if want, ok := operatorDefaults[resource.Name.Name]; ok {
			if got := stringValue(spec(resource), "default"); got != want {
				t.Errorf("normalized operator default %s=%q want %q", resource.Name.Name, got, want)
			}
			delete(operatorDefaults, resource.Name.Name)
		}
	}
	if len(operatorDefaults) != 0 {
		t.Fatalf("normalized operator defaults were not inspected: %v", operatorDefaults)
	}
	desired, err := New().Normalize(ctx, reloaded)
	if err != nil {
		t.Fatal(err)
	}
	adopted, err := plan.Build(ctx, New(), current, desired, plan.Options{})
	if err != nil || len(adopted.Steps) != 0 {
		t.Fatalf("adopt plan=%+v err=%v", adopted, err)
	}

	if _, err = conn.Exec(ctx, `drop schema autosql_default_matrix cascade`); err != nil {
		t.Fatal(err)
	}
	empty, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	empty, err = New().Normalize(ctx, empty)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := plan.Build(ctx, New(), empty, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.Steps) == 0 {
		t.Fatal("fresh default matrix plan was empty")
	}
	sequenceStep, sequenceColumnStep := -1, -1
	for index, step := range fresh.Steps {
		if strings.Contains(step.SQL, `CREATE SEQUENCE "autosql_default_matrix"."widget_id_seq"`) {
			sequenceStep = index
		}
		if strings.Contains(step.SQL, `ADD COLUMN "seq_id"`) {
			sequenceColumnStep = index
		}
	}
	if sequenceStep < 0 || sequenceColumnStep < 0 || sequenceStep >= sequenceColumnStep {
		t.Fatalf("sequence dependency order is not provable: sequence=%d column=%d plan=%+v", sequenceStep, sequenceColumnStep, fresh.Steps)
	}
	applyDefaultMatrixPlan(t, ctx, conn, fresh)
	var add, subtract, multiply, divide, modulo, unary int
	var updatedAt int64
	var numericModulo string
	var realAdd float32
	var bigintPromoted int64
	if err = conn.QueryRow(ctx, `insert into autosql_default_matrix.widgets default values returning arithmetic_add, arithmetic_sub, arithmetic_mul, arithmetic_div, arithmetic_mod, arithmetic_unary, dbos_updated_at, numeric_mod::text, real_add, bigint_promoted`).Scan(&add, &subtract, &multiply, &divide, &modulo, &unary, &updatedAt, &numericModulo, &realAdd, &bigintPromoted); err != nil {
		t.Fatalf("consume operator defaults: %v", err)
	}
	if add != 3 || subtract != 9 || multiply != 6 || divide != 5 || modulo != 1 || unary != -3 || updatedAt <= 0 || numericModulo != "1.5" || realAdd != 3 || bigintPromoted != 2147483648 {
		t.Fatalf("operator defaults add=%d subtract=%d multiply=%d divide=%d modulo=%d unary=%d updated_at=%d numeric_mod=%s real_add=%v bigint_promoted=%d", add, subtract, multiply, divide, modulo, unary, updatedAt, numericModulo, realAdd, bigintPromoted)
	}
	actual, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentsEqual(t, actual, desired)

	incremental := desired
	incremental.Graph.Resources = append([]schema.Resource(nil), desired.Graph.Resources...)
	var table schema.Resource
	for _, resource := range incremental.Graph.Resources {
		if resource.Kind == schema.KindTable && resource.Name.Name == "widgets" {
			table = resource
		}
	}
	maxOrdinal := 0
	for _, resource := range incremental.Graph.Resources {
		if resource.Kind == schema.KindColumn && resource.Name.Parent == table.ID {
			maxOrdinal = max(maxOrdinal, numberAsInt(spec(resource), "ordinal"))
		}
	}
	added := schema.Resource{Kind: schema.KindColumn, Name: schema.Name{Schema: namespace, Name: "unrelated", Parent: table.ID}, Dependencies: []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(fmt.Sprintf(`{"type":"text","default":"'safe'","not_null":false,"ordinal":%d}`, maxOrdinal+1))}
	added.ID = schema.StableID(added.Kind, added.Name)
	incremental.Graph.Resources = append(incremental.Graph.Resources, added)
	incremental, err = New().Normalize(ctx, incremental)
	if err != nil {
		t.Fatal(err)
	}
	increment, err := plan.Build(ctx, New(), actual, incremental, plan.Options{})
	if err != nil || len(increment.Steps) == 0 {
		t.Fatalf("incremental plan=%+v err=%v", increment, err)
	}
	applyDefaultMatrixPlan(t, ctx, conn, increment)
	final, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	final, err = New().Normalize(ctx, final)
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentsEqual(t, final, incremental)
	noop, err := plan.Build(ctx, New(), final, incremental, plan.Options{})
	if err != nil || len(noop.Steps) != 0 {
		t.Fatalf("final no-op plan=%+v err=%v", noop, err)
	}
}

func TestCanonicalOperatorDefaultsIgnoreSearchPathShadowing(t *testing.T) {
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
	const cleanup = `
reset search_path;
drop schema if exists autosql_operator_shadow cascade;
drop operator if exists public.+(integer,integer);
drop operator if exists public.-(integer,integer);
drop operator if exists public.*(integer,integer);
drop operator if exists public./(integer,integer);
drop operator if exists public.%(integer,integer);
drop operator if exists public.-(none,integer);
drop function if exists public.autosql_malicious_binary(integer,integer);
drop function if exists public.autosql_malicious_unary(integer);`
	_, _ = conn.Exec(ctx, cleanup)
	defer conn.Exec(context.Background(), cleanup)
	if _, err = conn.Exec(ctx, `
create function public.autosql_malicious_binary(integer,integer) returns integer language sql immutable as 'select 999';
create function public.autosql_malicious_unary(integer) returns integer language sql immutable as 'select 999';
create operator public.+ (function=public.autosql_malicious_binary,leftarg=integer,rightarg=integer);
create operator public.- (function=public.autosql_malicious_binary,leftarg=integer,rightarg=integer);
create operator public.* (function=public.autosql_malicious_binary,leftarg=integer,rightarg=integer);
create operator public./ (function=public.autosql_malicious_binary,leftarg=integer,rightarg=integer);
create operator public.% (function=public.autosql_malicious_binary,leftarg=integer,rightarg=integer);
create operator public.- (function=public.autosql_malicious_unary,rightarg=integer);
set search_path=public,pg_catalog;
create schema autosql_operator_shadow;`); err != nil {
		t.Fatal(err)
	}
	shadowed := make([]int, 6)
	if err = conn.QueryRow(ctx, `select 1 + 2, 4 - 2, 2 * 3, 6 / 2, 7 % 4, -(1 + 2)`).Scan(&shadowed[0], &shadowed[1], &shadowed[2], &shadowed[3], &shadowed[4], &shadowed[5]); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(shadowed, []int{999, 999, 999, 999, 999, 999}) {
		t.Fatalf("attacker operators were not active: %v", shadowed)
	}
	canonical := []string{
		postgresDefault("1 + 2"), postgresDefault("4 - 2"), postgresDefault("2 * 3"),
		postgresDefault("6 / 2"), postgresDefault("7 % 4"), postgresDefault("-(1 + 2)"),
	}
	create := fmt.Sprintf(`create table autosql_operator_shadow.probe(add_value integer default %s, sub_value integer default %s, mul_value integer default %s, div_value integer default %s, mod_value integer default %s, unary_value integer default %s)`, canonical[0], canonical[1], canonical[2], canonical[3], canonical[4], canonical[5])
	if _, err = conn.Exec(ctx, create); err != nil {
		t.Fatal(err)
	}
	actual := make([]int, 6)
	if err = conn.QueryRow(ctx, `insert into autosql_operator_shadow.probe default values returning add_value, sub_value, mul_value, div_value, mod_value, unary_value`).Scan(&actual[0], &actual[1], &actual[2], &actual[3], &actual[4], &actual[5]); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(actual, []int{3, 2, 6, 3, 3, -3}) {
		t.Fatalf("canonical defaults invoked shadow operators: %v", actual)
	}
	for _, rejected := range []struct{ typ, value string }{
		{"integer", "1 / (1 / 2)"},
		{"integer", "1 / (0.4::integer)"},
		{"integer", "1 / (0.4::numeric(1,0))"},
		{"real", "1::real % 1::real"},
		{"smallint", "32767 + 1"},
		{"integer", "1 OPERATOR(public.+) 2"},
		{"integer", "1 + public.secret"},
		{"real", "1::real / ((16777216::real + 1::real) - 16777216::real)"},
		{"integer", "(-2147483648)::integer % (-1)::integer"},
		{"bigint", "(-9223372036854775808)::bigint % (-1)::bigint"},
		{"integer", "(-2147483648::integer) % (-1::integer)"},
		{"bigint", "(-9223372036854775808::bigint) % (-1::bigint)"},
		{"smallint", "extract(epoch from now()) + 0"},
	} {
		statements, renderErr := renderDocumentWithDefault(rejected.typ, rejected.value)
		if renderErr == nil || len(statements) != 0 {
			t.Fatalf("unsafe default type=%s value=%q rendered=%v err=%v", rejected.typ, rejected.value, statements, renderErr)
		}
	}
	if _, err = conn.Exec(ctx, `create table autosql_operator_shadow.float_runtime_probe(value real default (1::real / ((16777216::real + 1::real) - 16777216::real)))`); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `insert into autosql_operator_shadow.float_runtime_probe default values`); err == nil || !strings.Contains(err.Error(), "division by zero") {
		t.Fatalf("PostgreSQL float4 rounding proof err=%v", err)
	}
	if _, err = conn.Exec(ctx, `create table autosql_operator_shadow.dynamic_runtime_probe(value smallint default (extract(epoch from now()) + 0))`); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `insert into autosql_operator_shadow.dynamic_runtime_probe default values`); err == nil || !strings.Contains(err.Error(), "smallint out of range") {
		t.Fatalf("PostgreSQL dynamic destination proof err=%v", err)
	}
}

func applyDefaultMatrixPlan(t *testing.T, ctx context.Context, conn *pgx.Conn, migration plan.Plan) {
	t.Helper()
	transaction, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range migration.Steps {
		if step.Kind == plan.StepTopology {
			continue
		}
		if _, err = transaction.Exec(ctx, step.SQL); err != nil {
			_ = transaction.Rollback(ctx)
			t.Fatalf("apply %s: %v", step.SQL, err)
		}
	}
	if err = transaction.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestMaterializedViewIndexesAdoptProvisionAndConverge(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	const namespace = "autosql_matview_indexes"
	if _, err = conn.Exec(ctx, `drop schema if exists autosql_matview_indexes cascade;
create schema autosql_matview_indexes;
create type autosql_matview_indexes.provider as enum ('aws', 'gcp');
create table autosql_matview_indexes.block_health(block_number bigint, provider autosql_matview_indexes.provider);
insert into autosql_matview_indexes.block_health values (1, 'aws'), (1, 'gcp'), (2, 'aws');
create materialized view autosql_matview_indexes.block_health_summary as
select block_number, provider, count(*)::bigint as sample_count
from autosql_matview_indexes.block_health
group by block_number, provider`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop schema if exists autosql_matview_indexes cascade`)
	for number := 1; number <= 8; number++ {
		name := fmt.Sprintf("block_health_summary_idx_%d", number)
		if _, err = conn.Exec(ctx, "create index "+pgx.Identifier{name}.Sanitize()+" on autosql_matview_indexes.block_health_summary (block_number)"); err != nil {
			t.Fatal(err)
		}
	}
	desired, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	resourcesByID := map[string]schema.Resource{}
	for _, resource := range desired.Graph.Resources {
		resourcesByID[resource.ID] = resource
	}
	indexCount := 0
	complexDependencyProven := false
	for _, resource := range desired.Graph.Resources {
		if resource.Kind == schema.KindMaterializedView && resource.Name.Name == "block_health_summary" {
			for _, dependency := range resource.Dependencies {
				complexDependencyProven = complexDependencyProven || resourcesByID[dependency.Target].Kind == schema.KindTable
			}
		}
		if resource.Kind == schema.KindIndex {
			indexCount++
			parent, ok := resourcesByID[resource.Name.Parent]
			if !ok || parent.Kind != schema.KindMaterializedView {
				t.Fatalf("index parent=%+v exists=%t", parent, ok)
			}
		}
	}
	if indexCount != 8 || !complexDependencyProven {
		t.Fatalf("inspected materialized-view indexes=%d dependency_proven=%t", indexCount, complexDependencyProven)
	}
	if _, err = conn.Exec(ctx, `drop schema autosql_matview_indexes cascade`); err != nil {
		t.Fatal(err)
	}
	empty, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	empty, err = New().Normalize(ctx, empty)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := plan.Build(ctx, New(), empty, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyDefaultMatrixPlan(t, ctx, conn, migration)
	actual, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentsEqual(t, actual, desired)
	noop, err := plan.Build(ctx, New(), actual, desired, plan.Options{})
	if err != nil || len(noop.Steps) != 0 {
		t.Fatalf("materialized-view index no-op plan=%+v err=%v", noop, err)
	}
}

func TestNetworkInetOperatorClassProvisionAndConverge(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	const namespace = "autosql_network_opclass"
	if _, err = conn.Exec(ctx, `drop schema if exists autosql_network_opclass cascade;
create schema autosql_network_opclass;
create table autosql_network_opclass.ipam_allocations(id bigint, allocation cidr);
create index idx_ipam_allocations_cidr_btree on autosql_network_opclass.ipam_allocations using btree (allocation inet_ops);
create index idx_ipam_allocations_cidr_hash on autosql_network_opclass.ipam_allocations using hash (allocation inet_ops);
create index idx_ipam_allocations_cidr_gist on autosql_network_opclass.ipam_allocations using gist (allocation inet_ops);
create index idx_ipam_allocations_cidr_spgist on autosql_network_opclass.ipam_allocations using spgist (allocation inet_ops)`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop schema if exists autosql_network_opclass cascade`)
	desired, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `drop schema autosql_network_opclass cascade`); err != nil {
		t.Fatal(err)
	}
	empty, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	empty, err = New().Normalize(ctx, empty)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := plan.Build(ctx, New(), empty, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyDefaultMatrixPlan(t, ctx, conn, migration)
	actual, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentsEqual(t, actual, desired)
	noop, err := plan.Build(ctx, New(), actual, desired, plan.Options{})
	if err != nil || len(noop.Steps) != 0 {
		t.Fatalf("inet_ops no-op plan=%+v err=%v", noop, err)
	}
}

func TestUnqualifiedTriggerTargetProvisionAndConverge(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	const namespace = "autosql_unqualified_trigger"
	if _, err = conn.Exec(ctx, `drop schema if exists autosql_unqualified_trigger cascade;
create schema autosql_unqualified_trigger;
create table autosql_unqualified_trigger.tenant_assignments(id bigint);
create function autosql_unqualified_trigger.capacity_check() returns trigger language plpgsql as $$ begin return new; end $$;
set search_path to autosql_unqualified_trigger, public;
create trigger tenant_assignments_capacity_trigger before insert on tenant_assignments for each row execute function autosql_unqualified_trigger.capacity_check()`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop schema if exists autosql_unqualified_trigger cascade`)
	desired, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for index := range desired.Graph.Resources {
		resource := &desired.Graph.Resources[index]
		if resource.Kind != schema.KindTrigger || resource.Name.Name != "tenant_assignments_capacity_trigger" {
			continue
		}
		values := specMap(resource.Spec)
		definition := stringValue(values, "definition")
		values["definition"] = strings.Replace(definition, namespace+".tenant_assignments", "tenant_assignments", 1)
		values["definition"] = strings.Replace(stringValue(values, "definition"), namespace+".capacity_check", "capacity_check", 1)
		resource.Spec, err = json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		found = true
	}
	if !found {
		t.Fatal("inspected trigger is missing")
	}
	desired, err = New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range desired.Graph.Resources {
		if resource.Kind == schema.KindTrigger && resource.Name.Name == "tenant_assignments_capacity_trigger" {
			definition := stringValue(spec(resource), "definition")
			if !strings.Contains(definition, " ON "+namespace+".tenant_assignments ") || !strings.Contains(definition, "EXECUTE FUNCTION "+namespace+".capacity_check()") {
				t.Fatalf("normalized trigger is not fully schema-bound: %s", definition)
			}
		}
	}
	if _, err = conn.Exec(ctx, `drop trigger tenant_assignments_capacity_trigger on autosql_unqualified_trigger.tenant_assignments`); err != nil {
		t.Fatal(err)
	}
	current, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	current, err = New().Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := plan.Build(ctx, New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyDefaultMatrixPlan(t, ctx, conn, migration)
	actual, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentsEqual(t, actual, desired)
	noop, err := plan.Build(ctx, New(), actual, desired, plan.Options{})
	if err != nil || len(noop.Steps) != 0 {
		t.Fatalf("unqualified trigger no-op plan=%+v err=%v", noop, err)
	}
}

func TestUnqualifiedMaterializedViewProvisionAndConverge(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	const namespace = "autosql_unqualified_view"
	if _, err = conn.Exec(ctx, `drop schema if exists autosql_unqualified_view cascade;
create schema autosql_unqualified_view;
create type autosql_unqualified_view.status as enum ('active');
create table autosql_unqualified_view.blocks(status autosql_unqualified_view.status);
insert into autosql_unqualified_view.blocks values ('active');
set search_path to autosql_unqualified_view, public;
create materialized view autosql_unqualified_view.block_health_summary as select status::status as status from blocks`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop schema if exists autosql_unqualified_view cascade`)
	desired, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for index := range desired.Graph.Resources {
		resource := &desired.Graph.Resources[index]
		if resource.Kind != schema.KindMaterializedView || resource.Name.Name != "block_health_summary" {
			continue
		}
		values := specMap(resource.Spec)
		definition := stringValue(values, "definition")
		definition = strings.ReplaceAll(definition, namespace+".blocks", "blocks")
		definition = strings.ReplaceAll(definition, namespace+".status", "status")
		values["definition"] = definition
		resource.Spec, err = json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		filtered := resource.Dependencies[:0]
		for _, dependency := range resource.Dependencies {
			if dependency.Type != schema.DependencyUses {
				filtered = append(filtered, dependency)
			}
		}
		resource.Dependencies = filtered
		found = true
	}
	if !found {
		t.Fatal("inspected materialized view is missing")
	}
	desired, err = New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `reset search_path; drop materialized view autosql_unqualified_view.block_health_summary`); err != nil {
		t.Fatal(err)
	}
	current, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	current, err = New().Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := plan.Build(ctx, New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyDefaultMatrixPlan(t, ctx, conn, migration)
	actual, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentsEqual(t, actual, desired)
	noop, err := plan.Build(ctx, New(), actual, desired, plan.Options{})
	if err != nil || len(noop.Steps) != 0 {
		t.Fatalf("unqualified materialized view no-op plan=%+v err=%v", noop, err)
	}
}

func TestRoutineCustomTypeAndRuntimeSearchPathProvisionAndConverge(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	const namespace = "autosql_routine_binding"
	if _, err = conn.Exec(ctx, `drop schema if exists autosql_routine_binding cascade;
create schema autosql_routine_binding;
create type autosql_routine_binding.state as enum ('active');
create table autosql_routine_binding.state_values(value autosql_routine_binding.state);
set search_path to autosql_routine_binding, public;
create function autosql_routine_binding.echo_state(s state) returns state
language plpgsql as $function$
begin
  -- runtime lookup intentionally remains bare
  return (select value::state from state_values where value = s limit 1);
end;
$function$`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop schema if exists autosql_routine_binding cascade`)
	desired, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for index := range desired.Graph.Resources {
		resource := &desired.Graph.Resources[index]
		if resource.Kind != schema.KindFunction || stringValue(spec(*resource), "name") != "echo_state" {
			continue
		}
		values := specMap(resource.Spec)
		values["definition"] = strings.ReplaceAll(stringValue(values, "definition"), namespace+".state", "state")
		resource.Spec, err = json.Marshal(values)
		if err != nil {
			t.Fatal(err)
		}
		found = true
	}
	if !found {
		t.Fatal("inspected routine is missing")
	}
	desired, err = New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	hcl, err := source.FormatHCL(desired)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadContext(ctx, source.Input{URI: "schema-bound-routine.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	options := map[string]string{}
	for _, resource := range desired.Graph.Resources {
		if resource.Kind == schema.KindFunction {
			options["reviewed_routine_digests"] = stringValue(spec(resource), "body_digest")
		}
	}
	if _, err = conn.Exec(ctx, `reset search_path; drop schema autosql_routine_binding cascade`); err != nil {
		t.Fatal(err)
	}
	current, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	current, err = New().Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := plan.Build(ctx, New(), current, desired, plan.Options{Render: options})
	if err != nil {
		t.Fatal(err)
	}
	applyDefaultMatrixPlan(t, ctx, conn, migration)
	if _, err = conn.Exec(ctx, `insert into autosql_routine_binding.state_values values ('active')`); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `set search_path to pg_catalog, public`); err != nil {
		t.Fatal(err)
	}
	var value string
	if err = conn.QueryRow(ctx, `select autosql_routine_binding.echo_state('active'::autosql_routine_binding.state)::text`).Scan(&value); err != nil {
		t.Fatal(err)
	}
	if value != "active" {
		t.Fatalf("runtime routine result=%q", value)
	}
	actual, err := InspectURL(ctx, url, Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	assertDocumentsEqual(t, actual, desired)
	noop, err := plan.Build(ctx, New(), actual, desired, plan.Options{Render: options})
	if err != nil || len(noop.Steps) != 0 {
		t.Fatalf("schema-bound routine no-op plan=%+v err=%v", noop, err)
	}
}

func assertDocumentsEqual(t *testing.T, actual, desired schema.Document) {
	t.Helper()
	actualFingerprint, actualErr := schema.SemanticFingerprint(actual)
	desiredFingerprint, desiredErr := schema.SemanticFingerprint(desired)
	if actualErr != nil || desiredErr != nil || actualFingerprint != desiredFingerprint {
		actualJSON, _ := actual.MarshalCanonical()
		desiredJSON, _ := desired.MarshalCanonical()
		t.Fatalf("documents differ actual_err=%v desired_err=%v\nactual=%s\ndesired=%s", actualErr, desiredErr, actualJSON, desiredJSON)
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
