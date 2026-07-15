package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"autosql/pkg/plan"
	"autosql/pkg/plugin"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/source"

	"github.com/jackc/pgx/v5"
)

func TestSQLSourcePlanApplyReinspectConverges(t *testing.T) {
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
	if _, err = conn.Exec(ctx, `drop schema if exists autosql_plan cascade; create schema autosql_plan; create table autosql_plan.widgets(z bigint not null, a text);`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop schema if exists autosql_plan cascade`)
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_plan"}})
	if err != nil {
		t.Fatal(err)
	}
	current, err = postgres.New().Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	fromSQL, err := source.ParseSQL("desired.sql", `CREATE TABLE autosql_plan.widgets(z bigint NOT NULL, a text); CREATE OR REPLACE VIEW autosql_plan.widget_view AS SELECT * FROM autosql_plan.widgets; CREATE VIEW autosql_plan.literal_view AS SELECT 'x' AS label; CREATE MATERIALIZED VIEW autosql_plan.widget_mv AS SELECT * FROM autosql_plan.widgets; CREATE MATERIALIZED VIEW autosql_plan.literal_mv AS SELECT 1 AS answer;`)
	if err != nil {
		t.Fatal(err)
	}
	fromSQL, err = postgres.New().Normalize(ctx, fromSQL)
	if err != nil {
		t.Fatal(err)
	}
	desired := current
	desired.Graph.Resources = append([]schema.Resource(nil), current.Graph.Resources...)
	for _, r := range fromSQL.Graph.Resources {
		replaced := false
		for idx, old := range desired.Graph.Resources {
			if old.ID == r.ID {
				desired.Graph.Resources[idx] = r
				replaced = true
				break
			}
		}
		if !replaced {
			desired.Graph.Resources = append(desired.Graph.Resources, r)
		}
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	if err := desired.Validate(); err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := p.MarshalCanonical()
	again, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, _ := again.MarshalCanonical()
	if string(first) != string(second) {
		t.Fatal("repeated plan bytes differ")
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range p.Steps {
		if step.Kind == plan.StepTopology {
			continue
		}
		if step.Transaction != plan.TransactionRequired {
			_ = tx.Rollback(ctx)
			t.Fatalf("unexpected phase: %+v", step)
		}
		if _, err = tx.Exec(ctx, step.SQL); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("apply %s: %v", step.SQL, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	actual, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_plan"}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = postgres.New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	wantFP, _ := schema.SemanticFingerprint(desired)
	gotFP, _ := schema.SemanticFingerprint(actual)
	if gotFP != wantFP {
		gotJSON, _ := actual.MarshalCanonical()
		wantJSON, _ := desired.MarshalCanonical()
		t.Fatalf("fingerprint mismatch got=%s want=%s\nactual=%s\ndesired=%s\nplan=%s", gotFP, wantFP, gotJSON, wantJSON, first)
	}
	empty, err := plan.Build(ctx, postgres.New(), actual, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(empty.Changes.Changes) != 0 || len(empty.Steps) != 0 {
		t.Fatalf("second plan not empty: %+v", empty)
	}
	for name, mutate := range map[string]func(*schema.Resource, schema.Document){
		"missing actual reference": func(view *schema.Resource, _ schema.Document) {
			filtered := view.Dependencies[:0]
			for _, dep := range view.Dependencies {
				if dep.Type != schema.DependencyReferences {
					filtered = append(filtered, dep)
				}
			}
			view.Dependencies = filtered
		},
		"extra unrelated reference": func(view *schema.Resource, doc schema.Document) {
			for _, candidate := range doc.Graph.Resources {
				if candidate.Kind == schema.KindView && candidate.Name.Name == "literal_view" {
					view.Dependencies = append(view.Dependencies, schema.Dependency{Target: candidate.ID, Type: schema.DependencyReferences})
				}
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			bad := actual
			bad.Graph.Resources = append([]schema.Resource(nil), actual.Graph.Resources...)
			for i := range bad.Graph.Resources {
				if bad.Graph.Resources[i].Kind == schema.KindView && bad.Graph.Resources[i].Name.Name == "widget_view" {
					bad.Graph.Resources[i].Dependencies = append([]schema.Dependency(nil), bad.Graph.Resources[i].Dependencies...)
					mutate(&bad.Graph.Resources[i], bad)
				}
			}
			failed, buildErr := plan.Build(ctx, postgres.New(), actual, bad, plan.Options{})
			if buildErr == nil || len(failed.Steps) != 0 {
				t.Fatalf("dependency mismatch planned: %+v err=%v", failed, buildErr)
			}
		})
	}
	badQuery := actual
	badQuery.Graph.Resources = append([]schema.Resource(nil), actual.Graph.Resources...)
	for i := range badQuery.Graph.Resources {
		if badQuery.Graph.Resources[i].Kind == schema.KindView && badQuery.Graph.Resources[i].Name.Name == "widget_view" {
			badQuery.Graph.Resources[i].Spec = json.RawMessage(`{"definition":"TABLE autosql_plan.widgets"}`)
		}
	}
	if failed, buildErr := plan.Build(ctx, postgres.New(), actual, badQuery, plan.Options{}); buildErr == nil || len(failed.Steps) != 0 {
		t.Fatalf("TABLE query expression planned: %+v err=%v", failed, buildErr)
	}
	regclassQuery := actual
	regclassQuery.Graph.Resources = append([]schema.Resource(nil), actual.Graph.Resources...)
	for i := range regclassQuery.Graph.Resources {
		if regclassQuery.Graph.Resources[i].Kind == schema.KindView && regclassQuery.Graph.Resources[i].Name.Name == "widget_view" {
			regclassQuery.Graph.Resources[i].Spec = json.RawMessage(`{"definition":"SELECT 'autosql_plan.widgets'::regclass AS label"}`)
		}
	}
	if failed, buildErr := plan.Build(ctx, postgres.New(), actual, regclassQuery, plan.Options{}); buildErr == nil || len(failed.Steps) != 0 {
		t.Fatalf("regclass dependency planned: %+v err=%v", failed, buildErr)
	}
	for name, definition := range map[string]string{"foreign qualifier": "SELECT other.z FROM autosql_plan.widgets", "mixed wildcard": "SELECT *, z FROM autosql_plan.widgets"} {
		t.Run(name, func(t *testing.T) {
			bad := actual
			bad.Graph.Resources = append([]schema.Resource(nil), actual.Graph.Resources...)
			for i := range bad.Graph.Resources {
				if bad.Graph.Resources[i].Kind == schema.KindView && bad.Graph.Resources[i].Name.Name == "widget_view" {
					bad.Graph.Resources[i].Spec = json.RawMessage(fmt.Sprintf(`{"definition":%q}`, definition))
				}
			}
			if failed, buildErr := plan.Build(ctx, postgres.New(), actual, bad, plan.Options{}); buildErr == nil || len(failed.Steps) != 0 {
				t.Fatalf("invalid projection planned: %+v err=%v", failed, buildErr)
			}
		})
	}
	raw, _ := json.Marshal(actual)
	var sameShape schema.Document
	_ = json.Unmarshal(raw, &sameShape)
	for idx := range sameShape.Graph.Resources {
		r := &sameShape.Graph.Resources[idx]
		if r.Kind == schema.KindView && r.Name.Name == "literal_view" {
			r.Spec = json.RawMessage(`{"definition":"SELECT 'y'::text AS label"}`)
		}
	}
	sameShape.Normalize()
	alter, err := plan.Build(ctx, postgres.New(), actual, sameShape, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, alter)
	reinspected, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_plan"}})
	if err != nil {
		t.Fatal(err)
	}
	reinspected, err = postgres.New().Normalize(ctx, reinspected)
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, reinspected, sameShape)
	noop, err := plan.Build(ctx, postgres.New(), reinspected, sameShape, plan.Options{})
	if err != nil || len(noop.Steps) != 0 {
		t.Fatalf("same-shape second plan=%+v err=%v", noop, err)
	}
}

func TestManagedCommentLifecycleCreateApplyReinspect(t *testing.T) {
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
	defer func() { _ = conn.Close(context.Background()) }()
	const namespace = "autosql_comments"
	defer func() { _, _ = conn.Exec(context.Background(), `drop schema if exists autosql_comments cascade`) }()
	const fixture = `
drop schema if exists autosql_comments cascade;
create schema autosql_comments;
create type autosql_comments.state as enum ('new','done');
create domain autosql_comments.positive_int as integer check (value > 0);
create sequence autosql_comments.widget_seq start with 10 increment by 2;
create table autosql_comments.widgets (
  id bigint not null,
  state autosql_comments.state not null default 'new',
  score autosql_comments.positive_int
);
create view autosql_comments.widget_view as select id from autosql_comments.widgets;
create materialized view autosql_comments.widget_mv as select 1 as value;
`
	if _, err = conn.Exec(ctx, fixture); err != nil {
		t.Fatal(err)
	}
	desired, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	for index := range desired.Graph.Resources {
		resource := &desired.Graph.Resources[index]
		if postgres.New().Info().Capability(resource.Kind).Mode == "managed" {
			resource.Annotations = map[string]string{"comment": "quote ' snowman ☃\nline; DROP SCHEMA autosql_comments; --"}
		}
	}
	desired.Normalize()
	if _, err = conn.Exec(ctx, `drop schema autosql_comments cascade`); err != nil {
		t.Fatal(err)
	}
	empty := schema.Document{Version: schema.SchemaVersion, Annotations: map[string]string{"dialect": "postgresql"}}
	fresh, err := plan.Build(ctx, postgres.New(), empty, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, fresh)
	actual, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = postgres.New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, actual, desired)

	updated := cloneSchemaDocument(t, desired)
	for index := range updated.Graph.Resources {
		resource := &updated.Graph.Resources[index]
		if resource.Kind == schema.KindSchema {
			resource.Annotations = nil
		}
		if resource.Kind == schema.KindTable {
			resource.Annotations = map[string]string{"comment": "changed ' safely"}
		}
	}
	updated.Normalize()
	commentOnly, err := plan.Build(ctx, postgres.New(), actual, updated, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, commentOnly)
	actual, err = postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = postgres.New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, actual, updated)

	added := cloneSchemaDocument(t, updated)
	for index := range added.Graph.Resources {
		if added.Graph.Resources[index].Kind == schema.KindSchema {
			added.Graph.Resources[index].Annotations = map[string]string{"comment": "added again"}
		}
	}
	added.Normalize()
	addPlan, err := plan.Build(ctx, postgres.New(), actual, added, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, addPlan)
	finalState, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	finalState, err = postgres.New().Normalize(ctx, finalState)
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, finalState, added)
	noop, err := plan.Build(ctx, postgres.New(), finalState, added, plan.Options{})
	if err != nil || len(noop.Changes.Changes) != 0 || len(noop.Steps) != 0 {
		t.Fatalf("comment lifecycle did not converge: changes=%d steps=%d err=%v", len(noop.Changes.Changes), len(noop.Steps), err)
	}
}

func cloneSchemaDocument(t *testing.T, input schema.Document) schema.Document {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var output schema.Document
	if err = json.Unmarshal(raw, &output); err != nil {
		t.Fatal(err)
	}
	return output
}

func TestIdentityColumnsCreateApplyReinspect(t *testing.T) {
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
	defer func() { _ = conn.Close(context.Background()) }()
	const namespace = "autosql_identity"
	defer func() { _, _ = conn.Exec(context.Background(), `drop schema if exists autosql_identity cascade`) }()
	if _, err = conn.Exec(ctx, `drop schema if exists autosql_identity cascade; create schema autosql_identity; create table autosql_identity.widgets(always_id bigint generated always as identity, default_id bigint generated by default as identity, label text);`); err != nil {
		t.Fatal(err)
	}
	desired, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range desired.Graph.Resources {
		if resource.Kind == schema.KindSequence {
			t.Fatalf("identity-owned sequence leaked into managed graph: %s", resource.Name.String())
		}
	}
	hcl, err := source.FormatHCL(desired)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadContext(ctx, source.Input{URI: "identity.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `drop schema autosql_identity cascade`); err != nil {
		t.Fatal(err)
	}
	empty := schema.Document{Version: schema.SchemaVersion, Annotations: map[string]string{"dialect": "postgresql"}}
	fresh, err := plan.Build(ctx, postgres.New(), empty, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, fresh)
	actual, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = postgres.New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, actual, desired)
	noop, err := plan.Build(ctx, postgres.New(), actual, desired, plan.Options{})
	if err != nil || len(noop.Steps) != 0 {
		t.Fatalf("identity lifecycle did not converge: steps=%d err=%v", len(noop.Steps), err)
	}
}

func TestGeneratedRoutinePrerequisiteVerification(t *testing.T) {
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
	defer func() { _ = conn.Close(context.Background()) }()
	defer func() { _, _ = conn.Exec(context.Background(), `drop schema if exists autosql_routine_prereq cascade`) }()
	const fixture = `
drop schema if exists autosql_routine_prereq cascade;
create schema autosql_routine_prereq;
create function autosql_routine_prereq.to_v2(value text) returns text language sql immutable return value;
create table autosql_routine_prereq.jobs(
  state text,
  state_v2 text generated always as (autosql_routine_prereq.to_v2(state)) stored
);`
	if _, err = conn.Exec(ctx, fixture); err != nil {
		t.Fatal(err)
	}
	desired, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_routine_prereq"}})
	if err != nil {
		t.Fatal(err)
	}
	report, err := postgres.VerifyGeneratedRoutinePrerequisites(ctx, url, desired)
	if err != nil || !report.Satisfied || len(report.Prerequisites) != 1 || report.Prerequisites[0].Status != "satisfied" {
		t.Fatalf("satisfied report=%+v err=%v", report, err)
	}
	if _, err = conn.Exec(ctx, `create or replace function autosql_routine_prereq.to_v2(value text) returns text language sql immutable return upper(value)`); err != nil {
		t.Fatal(err)
	}
	report, err = postgres.VerifyGeneratedRoutinePrerequisites(ctx, url, desired)
	if err != nil || report.Satisfied || len(report.Prerequisites) != 1 || report.Prerequisites[0].Status != "version_mismatch" {
		t.Fatalf("mismatch report=%+v err=%v", report, err)
	}
	if _, err = conn.Exec(ctx, `drop table autosql_routine_prereq.jobs; drop function autosql_routine_prereq.to_v2(text)`); err != nil {
		t.Fatal(err)
	}
	report, err = postgres.VerifyGeneratedRoutinePrerequisites(ctx, url, desired)
	if err != nil || report.Satisfied || len(report.Prerequisites) != 1 || report.Prerequisites[0].Status != "missing" {
		t.Fatalf("missing report=%+v err=%v", report, err)
	}
}

func TestStoredGeneratedColumnCreateApplyReinspect(t *testing.T) {
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
	defer func() { _ = conn.Close(context.Background()) }()
	const namespace = "autosql_generated"
	defer func() { _, _ = conn.Exec(context.Background(), `drop schema if exists autosql_generated cascade`) }()
	if _, err = conn.Exec(ctx, `drop schema if exists autosql_generated cascade; create schema autosql_generated; create function autosql_generated.lifecycle_state_to_v2(value text) returns text language sql immutable return upper(value);`); err != nil {
		t.Fatal(err)
	}
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	current, err = postgres.New().Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	desired := cloneSchemaDocument(t, current)
	var ns, routine schema.Resource
	for _, resource := range desired.Graph.Resources {
		switch resource.Kind {
		case schema.KindSchema:
			ns = resource
		case schema.KindFunction:
			routine = resource
		}
	}
	table := schema.Resource{Kind: schema.KindTable, Name: schema.Name{Schema: namespace, Name: "jobs", Parent: ns.ID}, Dependencies: []schema.Dependency{{Target: ns.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(`{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`)}
	table.ID = schema.StableID(table.Kind, table.Name)
	state := schema.Resource{Kind: schema.KindColumn, Name: schema.Name{Schema: namespace, Name: "state", Parent: table.ID}, Dependencies: []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(`{"type":"text","not_null":false,"ordinal":1}`)}
	state.ID = schema.StableID(state.Kind, state.Name)
	generated := schema.Resource{Kind: schema.KindColumn, Name: schema.Name{Schema: namespace, Name: "state_v2", Parent: table.ID}, Dependencies: []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}, {Target: state.ID, Type: schema.DependencyReferences}, {Target: routine.ID, Type: schema.DependencyReferences}}, Spec: json.RawMessage(`{"type":"text","not_null":false,"ordinal":2,"default":"autosql_generated.lifecycle_state_to_v2(state)","generated":"s"}`)}
	generated.ID = schema.StableID(generated.Kind, generated.Name)
	desired.Graph.Resources = append(desired.Graph.Resources, table, state, generated)
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	hcl, err := source.FormatHCL(desired)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadContext(ctx, source.Input{URI: "generated.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	prerequisites, err := postgres.VerifyGeneratedRoutinePrerequisites(ctx, url, desired)
	if err != nil || !prerequisites.Satisfied {
		t.Fatalf("prerequisites=%+v err=%v", prerequisites, err)
	}
	fresh, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, fresh)
	actual, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = postgres.New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, actual, desired)
	noop, err := plan.Build(ctx, postgres.New(), actual, desired, plan.Options{})
	if err != nil || len(noop.Steps) != 0 {
		t.Fatalf("stored generated lifecycle did not converge: steps=%d err=%v", len(noop.Steps), err)
	}
}

func TestCompleteCellProvisioningParity(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = conn.Close(context.Background()) }()
	const namespace = "autosql_complete_cell"
	defer func() { _, _ = conn.Exec(context.Background(), `drop schema if exists autosql_complete_cell cascade`) }()
	const prerequisite = `
create schema autosql_complete_cell;
create function autosql_complete_cell.lifecycle_state_to_v2(value text)
returns text language sql immutable security invoker
as $$ select upper(value) $$;`
	const fixture = `
drop schema if exists autosql_complete_cell cascade;
` + prerequisite + `
comment on schema autosql_complete_cell is 'complete provisioning fixture';
create type autosql_complete_cell.job_status as enum ('pending', 'running', 'done');
comment on type autosql_complete_cell.job_status is 'job lifecycle';
create domain autosql_complete_cell.positive_int as integer default 1 check (value > 0);
comment on domain autosql_complete_cell.positive_int is 'strictly positive';
create sequence autosql_complete_cell.ticket_seq start 100 increment 5 cache 3;
comment on sequence autosql_complete_cell.ticket_seq is 'public ticket numbers';
create table autosql_complete_cell.jobs (
  always_id bigint generated always as identity,
  default_id bigint generated by default as identity,
  state text not null default 'pending'::text,
  state_v2 text generated always as (autosql_complete_cell.lifecycle_state_to_v2(state)) stored,
  title character varying(255) not null default 'untitled',
  payload jsonb not null default '{}'::jsonb,
  tags text[] not null default '{}'::text[],
  created_at timestamptz not null default now(),
  status autosql_complete_cell.job_status not null default 'pending'::autosql_complete_cell.job_status,
  attempts autosql_complete_cell.positive_int not null default 1,
  ticket bigint not null default nextval('autosql_complete_cell.ticket_seq'::regclass),
  constraint jobs_pkey primary key (always_id),
  constraint jobs_attempts_check check (attempts > 0)
);
comment on table autosql_complete_cell.jobs is 'jobs managed from HCL';
comment on column autosql_complete_cell.jobs.state_v2 is 'normalized lifecycle state';
comment on column autosql_complete_cell.jobs.payload is 'structured job payload';
create index jobs_status_idx on autosql_complete_cell.jobs(status);`
	if _, err = conn.Exec(ctx, fixture); err != nil {
		t.Fatal(err)
	}

	inspected, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	formatted, err := source.FormatHCL(inspected)
	if err != nil {
		t.Fatal(err)
	}
	desired, err := source.LoadContext(ctx, source.Input{URI: "complete-cell.hcl", Format: source.FormatHCLSource, Data: formatted})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	assertCompleteCellFeatures(t, desired)

	adopt, err := plan.Build(ctx, postgres.New(), desired, desired, plan.Options{})
	if err != nil || len(adopt.Changes.Changes) != 0 || len(adopt.Steps) != 0 {
		t.Fatalf("adoption was not a no-op: changes=%d steps=%d err=%v", len(adopt.Changes.Changes), len(adopt.Steps), err)
	}
	fullReport, err := postgres.PreflightProvisioning(ctx, desired, nil)
	if err != nil {
		t.Fatal(err)
	}
	assertCompleteCellExternalInventory(t, fullReport)

	managed := managedCellProjection(t, desired)
	if _, err = conn.Exec(ctx, `drop schema autosql_complete_cell cascade; `+prerequisite); err != nil {
		t.Fatal(err)
	}
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	current, err = postgres.New().Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	prerequisites, err := postgres.VerifyGeneratedRoutinePrerequisites(ctx, url, managed)
	if err != nil || !prerequisites.Satisfied || len(prerequisites.Prerequisites) != 1 {
		t.Fatalf("generated routine prerequisites=%+v err=%v", prerequisites, err)
	}
	fresh, err := plan.Build(ctx, postgres.New(), current, managed, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(fresh.Steps) == 0 {
		t.Fatal("fresh managed projection produced no steps")
	}
	applyTestPlan(t, ctx, conn, fresh)
	actual, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = postgres.New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, actual, managed)
	noop, err := plan.Build(ctx, postgres.New(), actual, managed, plan.Options{})
	if err != nil || len(noop.Changes.Changes) != 0 || len(noop.Steps) != 0 {
		t.Fatalf("fresh provisioning did not converge: changes=%d steps=%d err=%v", len(noop.Changes.Changes), len(noop.Steps), err)
	}

	added := cloneSchemaDocument(t, managed)
	var table schema.Resource
	for _, resource := range added.Graph.Resources {
		if resource.Kind == schema.KindTable && resource.Name.Name == "jobs" {
			table = resource
		}
	}
	column := schema.Resource{
		Kind:         schema.KindColumn,
		Name:         schema.Name{Schema: namespace, Name: "trace_id", Parent: table.ID},
		Dependencies: []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}},
		Spec:         json.RawMessage(`{"type":"text","not_null":false,"ordinal":12}`),
	}
	column.ID = schema.StableID(column.Kind, column.Name)
	added.Graph.Resources = append(added.Graph.Resources, column)
	added, err = postgres.New().Normalize(ctx, added)
	if err != nil {
		t.Fatal(err)
	}
	incremental, err := plan.Build(ctx, postgres.New(), actual, added, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, incremental)
	finalState, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	finalState, err = postgres.New().Normalize(ctx, finalState)
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, finalState, added)
	finalNoop, err := plan.Build(ctx, postgres.New(), finalState, added, plan.Options{})
	if err != nil || len(finalNoop.Changes.Changes) != 0 || len(finalNoop.Steps) != 0 {
		t.Fatalf("incremental change did not converge: changes=%d steps=%d err=%v", len(finalNoop.Changes.Changes), len(finalNoop.Steps), err)
	}
}

func managedCellProjection(t *testing.T, input schema.Document) schema.Document {
	t.Helper()
	out := cloneSchemaDocument(t, input)
	keep := map[string]bool{}
	for _, resource := range out.Graph.Resources {
		if postgres.New().Info().Capability(resource.Kind).Mode == plugin.Managed || resource.Kind == schema.KindFunction {
			keep[resource.ID] = true
		}
	}
	resources := out.Graph.Resources[:0]
	for _, resource := range out.Graph.Resources {
		if !keep[resource.ID] {
			continue
		}
		dependencies := resource.Dependencies[:0]
		for _, dependency := range resource.Dependencies {
			if keep[dependency.Target] {
				dependencies = append(dependencies, dependency)
			}
		}
		resource.Dependencies = dependencies
		resources = append(resources, resource)
	}
	out.Graph.Resources = resources
	var err error
	out, err = postgres.New().Normalize(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	if err = out.Validate(); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertCompleteCellFeatures(t *testing.T, doc schema.Document) {
	t.Helper()
	comments := 0
	identities := map[string]bool{}
	found := map[schema.Kind]bool{}
	for _, resource := range doc.Graph.Resources {
		found[resource.Kind] = true
		if resource.Annotations["comment"] != "" {
			comments++
		}
		if resource.Kind != schema.KindColumn {
			continue
		}
		var values map[string]any
		if err := json.Unmarshal(resource.Spec, &values); err != nil {
			t.Fatal(err)
		}
		if identity, _ := values["identity"].(string); identity != "" {
			identities[identity] = true
		}
		if resource.Name.Name == "state_v2" {
			if values["generated"] != "s" || values["default"] != "autosql_complete_cell.lifecycle_state_to_v2(state)" {
				t.Fatalf("stored generated spec=%s", resource.Spec)
			}
			references := 0
			for _, dependency := range resource.Dependencies {
				if dependency.Type == schema.DependencyReferences {
					references++
				}
			}
			if references != 2 {
				t.Fatalf("stored generated references=%d dependencies=%+v", references, resource.Dependencies)
			}
		}
	}
	if comments < 7 || !identities["a"] || !identities["d"] || !found[schema.KindEnum] || !found[schema.KindDomain] || !found[schema.KindSequence] {
		t.Fatalf("incomplete fixture: comments=%d identities=%v kinds=%v", comments, identities, found)
	}
}

func assertCompleteCellExternalInventory(t *testing.T, report postgres.ProvisioningReport) {
	t.Helper()
	if report.Supported {
		t.Fatal("full inspected cell unexpectedly reported as wholly managed")
	}
	kinds := map[schema.Kind]bool{}
	external := false
	for _, diagnostic := range report.Diagnostics {
		kinds[diagnostic.Kind] = true
		external = external || diagnostic.External
	}
	if !external || !kinds[schema.KindFunction] || !kinds[schema.KindIndex] || (!kinds[schema.KindPrimaryKey] && !kinds[schema.KindCheckConstraint]) {
		t.Fatalf("incomplete aggregate external/read-only inventory: %+v", report.Diagnostics)
	}
}

func TestSchemaAndTableRenameTopologyConverges(t *testing.T) {
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
	_, err = conn.Exec(ctx, `drop schema if exists autosql_rename_old cascade; drop schema if exists autosql_rename_new cascade; create schema autosql_rename_old; create table autosql_rename_old.widgets(id bigint);`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop schema if exists autosql_rename_old cascade; drop schema if exists autosql_rename_new cascade`)
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_rename_old"}})
	if err != nil {
		t.Fatal(err)
	}
	current, err = postgres.New().Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	desired, oldSchema, newSchema, _, _ := renameFixture(current, "autosql_rename_new", "widgets")
	p, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{Diff: schema.DiffOptions{RenameHints: []schema.RenameHint{{From: oldSchema, To: newSchema}}}})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, p)
	for _, step := range p.Steps {
		if step.Kind == plan.StepTopology && step.SQL != "" {
			t.Fatal("topology step has SQL")
		}
		if strings.Contains(step.SQL, "RENAME TO \"widgets\"") {
			t.Fatalf("invalid same-name descendant SQL: %s", step.SQL)
		}
	}
	afterSchema, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_rename_new"}})
	if err != nil {
		t.Fatal(err)
	}
	afterSchema, _ = postgres.New().Normalize(ctx, afterSchema)
	assertFingerprint(t, afterSchema, desired)
	desiredTable, _, _, oldTable, newTable := renameFixture(afterSchema, "autosql_rename_new", "widgets_new")
	p, err = plan.Build(ctx, postgres.New(), afterSchema, desiredTable, plan.Options{Diff: schema.DiffOptions{RenameHints: []schema.RenameHint{{From: oldTable, To: newTable}}}})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, p)
	for _, step := range p.Steps {
		if step.Kind == plan.StepTopology && step.SQL != "" {
			t.Fatal("topology step has SQL")
		}
		if strings.Contains(step.SQL, "RENAME COLUMN \"id\" TO \"id\"") {
			t.Fatalf("invalid same-name child SQL: %s", step.SQL)
		}
	}
	final, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_rename_new"}})
	if err != nil {
		t.Fatal(err)
	}
	final, _ = postgres.New().Normalize(ctx, final)
	assertFingerprint(t, final, desiredTable)
}

func TestMaterializedViewRebuildRejectsUnmanagedDependents(t *testing.T) {
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
	_, err = conn.Exec(ctx, `
drop schema if exists autosql_dependents cascade;
create schema autosql_dependents;
create table autosql_dependents.widgets(id bigint not null);
create materialized view autosql_dependents.widget_mv as select id from autosql_dependents.widgets;
create index widget_mv_id_idx on autosql_dependents.widget_mv(id);
create view autosql_dependents.widget_view as select id from autosql_dependents.widget_mv;
`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop schema if exists autosql_dependents cascade`)
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_dependents"}})
	if err != nil {
		t.Fatal(err)
	}
	current, err = postgres.New().Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	desired := current
	desired.Graph.Resources = append([]schema.Resource(nil), current.Graph.Resources...)
	for i := range desired.Graph.Resources {
		r := &desired.Graph.Resources[i]
		if r.Kind == schema.KindMaterializedView && r.Name.Name == "widget_mv" {
			r.Spec = json.RawMessage(`{"definition":"SELECT id FROM autosql_dependents.widgets WHERE id >= 0"}`)
		}
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{Render: map[string]string{"allow_rebuild": "true"}})
	if err == nil {
		t.Fatalf("unmanaged index and dependent view should block rebuild: %+v", p)
	}
	if len(p.Steps) != 0 {
		t.Fatalf("failed rebuild returned executable steps: %+v", p.Steps)
	}
}

func TestNativeDocumentCreateReinspectConverges(t *testing.T) {
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
	_, err = conn.Exec(ctx, `drop schema if exists autosql_native cascade`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop schema if exists autosql_native cascade`)
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_native"}})
	if err != nil {
		t.Fatal(err)
	}
	current, err = postgres.New().Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	ns := schema.Resource{Kind: schema.KindSchema, Name: schema.Name{Name: "autosql_native"}, Spec: json.RawMessage(`{}`)}
	ns.ID = schema.StableID(ns.Kind, ns.Name)
	table := schema.Resource{Kind: schema.KindTable, Name: schema.Name{Schema: "autosql_native", Name: "widgets", Parent: ns.ID}, Dependencies: []schema.Dependency{{Target: ns.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(`{}`)}
	table.ID = schema.StableID(table.Kind, table.Name)
	z := schema.Resource{Kind: schema.KindColumn, Name: schema.Name{Schema: "autosql_native", Name: "z", Parent: table.ID}, Dependencies: []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(`{"type":"smallint","not_null":false,"ordinal":1}`)}
	z.ID = schema.StableID(z.Kind, z.Name)
	a := schema.Resource{Kind: schema.KindColumn, Name: schema.Name{Schema: "autosql_native", Name: "a", Parent: table.ID}, Dependencies: []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(`{"type":"text","not_null":false,"ordinal":2}`)}
	a.ID = schema.StableID(a.Kind, a.Name)
	defaults := []schema.Resource{}
	for index, fixture := range []struct{ name, typ, value string }{
		{"count", "integer", "-2147483648"},
		{"small", "smallint", "-32768"},
		{"large", "bigint", "9223372036854775807"},
		{"label", "text", "'x'"},
		{"enabled", "boolean", "true"},
		{"price", "numeric(10,2)", "0.00"},
		{"metadata", "jsonb", "'{}'::jsonb"},
		{"items", "jsonb", "'[]'::jsonb"},
		{"external_id", "uuid", "'550e8400-e29b-41d4-a716-446655440000'::uuid"},
		{"generated_id", "uuid", "gen_random_uuid()"},
		{"created_at", "timestamptz", "CURRENT_TIMESTAMP"},
		{"now_at", "timestamptz", "now()"},
		{"transaction_at", "timestamptz", "transaction_timestamp()"},
		{"observed_at", "timestamp(3)", "CURRENT_TIMESTAMP(3)"},
		{"business_date", "date", "CURRENT_DATE"},
		{"local_clock", "time(3)", "LOCALTIME(3)"},
		{"zoned_clock", "timetz(2)", "CURRENT_TIME(2)"},
		{"local_stamp", "timestamp(2)", "LOCALTIMESTAMP(2)"},
		{"utc_stamp", "timestamp", "timezone('utc'::text, now())"},
		{"delay", "interval", "'00:05:00'::interval"},
		{"day_delay", "interval day to second(3)", "'1 day 00:05:00'::interval"},
		{"code", "character(4)", "'x'::character(1)"},
		{"flags", "bit(4)", "'1010'::\"bit\""},
		{"variable_flags", "bit varying(8)", "'101'::\"bit\""},
		{"empty_tags", "text[]", "'{}'::text[]"},
		{"tags", "text[]", "ARRAY['a'::text, 'b'::text]"},
		{"numbers", "integer[]", "ARRAY[1, 2]"},
		{"switches", "boolean[]", "ARRAY[true, false]"},
	} {
		column := schema.Resource{Kind: schema.KindColumn, Name: schema.Name{Schema: "autosql_native", Name: fixture.name, Parent: table.ID}, Dependencies: []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(fmt.Sprintf(`{"type":%q,"default":%q,"not_null":false,"ordinal":%d}`, fixture.typ, fixture.value, index+3))}
		column.ID = schema.StableID(column.Kind, column.Name)
		defaults = append(defaults, column)
	}
	desiredResources := []schema.Resource{ns, table, z, a}
	desiredResources = append(desiredResources, defaults...)
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: desiredResources}}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var createSQL strings.Builder
	for _, step := range p.Steps {
		createSQL.WriteString(step.SQL)
	}
	if strings.Index(createSQL.String(), `"z" smallint`) > strings.Index(createSQL.String(), `"a" text`) {
		t.Fatalf("column creates are not in desired ordinal order: %s", createSQL.String())
	}
	applyTestPlan(t, ctx, conn, p)
	actual, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_native"}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = postgres.New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, actual, desired)
	noop, err := plan.Build(ctx, postgres.New(), actual, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(noop.Changes.Changes) != 0 || len(noop.Steps) != 0 {
		t.Fatalf("native document second plan not empty: %+v", noop)
	}
	for name, value := range map[string]string{"leading zero": "01", "out of range": "2147483648"} {
		t.Run(name, func(t *testing.T) {
			bad := actual
			bad.Graph.Resources = append([]schema.Resource(nil), actual.Graph.Resources...)
			column := schema.Resource{Kind: schema.KindColumn, Name: schema.Name{Schema: "autosql_native", Name: "bad_default", Parent: table.ID}, Dependencies: []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(fmt.Sprintf(`{"type":"integer","default":%q,"not_null":false,"ordinal":%d}`, value, len(defaults)+3))}
			column.ID = schema.StableID(column.Kind, column.Name)
			bad.Graph.Resources = append(bad.Graph.Resources, column)
			bad, err = postgres.New().Normalize(ctx, bad)
			if err != nil {
				t.Fatal(err)
			}
			failed, buildErr := plan.Build(ctx, postgres.New(), actual, bad, plan.Options{})
			if buildErr == nil || len(failed.Steps) != 0 {
				t.Fatalf("planned=%+v err=%v", failed, buildErr)
			}
		})
	}
	safe := desired
	safe.Graph.Resources = append([]schema.Resource(nil), desired.Graph.Resources...)
	for i := range safe.Graph.Resources {
		if safe.Graph.Resources[i].ID == z.ID {
			safe.Graph.Resources[i].Spec = json.RawMessage(`{"type":"integer","not_null":false,"ordinal":1}`)
		}
	}
	safe, err = postgres.New().Normalize(ctx, safe)
	if err != nil {
		t.Fatal(err)
	}
	safePlan, err := plan.Build(ctx, postgres.New(), actual, safe, plan.Options{})
	if err != nil {
		t.Fatalf("safe smallint to integer cast: %v", err)
	}
	applyTestPlan(t, ctx, conn, safePlan)
	actual, err = postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_native"}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = postgres.New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, actual, safe)
	unsafe := safe
	unsafe.Graph.Resources = append([]schema.Resource(nil), safe.Graph.Resources...)
	for i := range unsafe.Graph.Resources {
		if unsafe.Graph.Resources[i].ID == a.ID {
			unsafe.Graph.Resources[i].Spec = json.RawMessage(`{"type":"integer","not_null":false,"ordinal":2}`)
		}
	}
	unsafe, err = postgres.New().Normalize(ctx, unsafe)
	if err != nil {
		t.Fatal(err)
	}
	if failed, buildErr := plan.Build(ctx, postgres.New(), actual, unsafe, plan.Options{}); buildErr == nil || len(failed.Steps) != 0 {
		t.Fatalf("unsafe text to integer cast planned: %+v err=%v", failed, buildErr)
	}
	dropped := safe
	dropped.Graph.Resources = nil
	for _, r := range safe.Graph.Resources {
		if r.ID != z.ID {
			dropped.Graph.Resources = append(dropped.Graph.Resources, r)
		}
	}
	dropped, err = postgres.New().Normalize(ctx, dropped)
	if err != nil {
		t.Fatal(err)
	}
	dropPlan, err := plan.Build(ctx, postgres.New(), actual, dropped, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, dropPlan)
	actual, err = postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_native"}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = postgres.New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, actual, dropped)
	secondDrop, err := plan.Build(ctx, postgres.New(), actual, dropped, plan.Options{})
	if err != nil || len(secondDrop.Steps) != 0 {
		t.Fatalf("nonfinal drop second plan=%+v err=%v", secondDrop, err)
	}
}

func TestUDTArrayColumnApplyReinspectConverges(t *testing.T) {
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
	_, err = conn.Exec(ctx, `drop schema if exists "AutoSQL_UDT" cascade; create schema "AutoSQL_UDT"; create type "AutoSQL_UDT".status as enum ('new'); create type "AutoSQL_UDT"."Mood" as enum ('good'); create table "AutoSQL_UDT".widgets(id bigint);`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop schema if exists "AutoSQL_UDT" cascade`)
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"AutoSQL_UDT"}})
	if err != nil {
		t.Fatal(err)
	}
	current, err = postgres.New().Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	desired := current
	desired.Graph.Resources = append([]schema.Resource(nil), current.Graph.Resources...)
	var table schema.Resource
	types := map[string]schema.Resource{}
	for _, r := range current.Graph.Resources {
		if r.Kind == schema.KindTable {
			table = r
		}
		if r.Kind == schema.KindEnum {
			types[r.Name.Name] = r
		}
	}
	for index, fixture := range []struct{ name, typ, target string }{{"statuses", `status[][]`, "status"}, {"moods", `"AutoSQL_UDT"."Mood"[][]`, "Mood"}, {"matrix", "integer[][]", ""}} {
		deps := []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}
		if fixture.target != "" {
			deps = append(deps, schema.Dependency{Target: types[fixture.target].ID, Type: schema.DependencyUses})
		}
		column := schema.Resource{Kind: schema.KindColumn, Name: schema.Name{Schema: "AutoSQL_UDT", Name: fixture.name, Parent: table.ID}, Dependencies: deps, Spec: json.RawMessage(fmt.Sprintf(`{"type":%q,"not_null":false,"ordinal":%d}`, fixture.typ, index+2))}
		column.ID = schema.StableID(column.Kind, column.Name)
		desired.Graph.Resources = append(desired.Graph.Resources, column)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, p)
	actual, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"AutoSQL_UDT"}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = postgres.New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, actual, desired)
	noop, err := plan.Build(ctx, postgres.New(), actual, desired, plan.Options{})
	if err != nil || len(noop.Steps) != 0 {
		t.Fatalf("UDT array second plan=%+v err=%v", noop, err)
	}
}

func TestNonfinalColumnRenamePreservesOrdinal(t *testing.T) {
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
	_, err = conn.Exec(ctx, `drop schema if exists autosql_colrename cascade; create schema autosql_colrename; create table autosql_colrename.widgets(a text,b text);`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop schema if exists autosql_colrename cascade`)
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_colrename"}})
	if err != nil {
		t.Fatal(err)
	}
	current, err = postgres.New().Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	desired := current
	desired.Graph.Resources = nil
	var oldID, newID string
	for _, original := range current.Graph.Resources {
		r := original
		if r.Kind == schema.KindColumn && r.Name.Name == "a" {
			continue
		}
		if r.Kind == schema.KindColumn && r.Name.Name == "b" {
			oldID = r.ID
			r.Name.Name = "x"
			r.ID = schema.StableID(r.Kind, r.Name)
			newID = r.ID
		}
		desired.Graph.Resources = append(desired.Graph.Resources, r)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{Diff: schema.DiffOptions{RenameHints: []schema.RenameHint{{From: oldID, To: newID}}}})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, p)
	actual, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_colrename"}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = postgres.New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, actual, desired)
}

func TestColumnTransitionsRejectRetainedReadOnlyDependents(t *testing.T) {
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
	_, err = conn.Exec(ctx, `drop schema if exists autosql_coldeps cascade; create schema autosql_coldeps; create table autosql_coldeps.widgets(a text unique,b text); create index widgets_b_idx on autosql_coldeps.widgets(b);`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop schema if exists autosql_coldeps cascade`)
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_coldeps"}})
	if err != nil {
		t.Fatal(err)
	}
	current, err = postgres.New().Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	for name, columnName := range map[string]string{"drop unique column": "a", "rename indexed column": "b"} {
		t.Run(name, func(t *testing.T) {
			desired := current
			desired.Graph.Resources = nil
			var oldID, newID string
			for _, original := range current.Graph.Resources {
				r := original
				if r.Kind == schema.KindColumn && r.Name.Name == columnName {
					oldID = r.ID
					if columnName == "a" {
						continue
					}
					r.Name.Name = "x"
					r.ID = schema.StableID(r.Kind, r.Name)
					newID = r.ID
				}
				desired.Graph.Resources = append(desired.Graph.Resources, r)
			}
			desired, err = postgres.New().Normalize(ctx, desired)
			if err != nil {
				t.Fatal(err)
			}
			options := plan.Options{}
			if newID != "" {
				options.Diff.RenameHints = []schema.RenameHint{{From: oldID, To: newID}}
			}
			failed, buildErr := plan.Build(ctx, postgres.New(), current, desired, options)
			if buildErr == nil || len(failed.Steps) != 0 {
				t.Fatalf("planned=%+v err=%v", failed, buildErr)
			}
		})
	}
}

func TestParentRenameRejectsRetainedOpaqueDescendants(t *testing.T) {
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
	_, err = conn.Exec(ctx, `drop schema if exists autosql_parent_old cascade; drop schema if exists autosql_parent_new cascade; create schema autosql_parent_old; create table autosql_parent_old.widgets(id bigint); create index widgets_id_idx on autosql_parent_old.widgets(id);`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop schema if exists autosql_parent_old cascade; drop schema if exists autosql_parent_new cascade`)
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_parent_old"}})
	if err != nil {
		t.Fatal(err)
	}
	current, err = postgres.New().Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	for name, fixture := range map[string]struct {
		schemaName, tableName string
		schemaHint            bool
	}{"table rename": {"autosql_parent_old", "widgets_new", false}, "schema rename": {"autosql_parent_new", "widgets", true}} {
		t.Run(name, func(t *testing.T) {
			desired, oldSchema, newSchema, oldTable, newTable := renameFixture(current, fixture.schemaName, fixture.tableName)
			hint := schema.RenameHint{From: oldTable, To: newTable}
			if fixture.schemaHint {
				hint = schema.RenameHint{From: oldSchema, To: newSchema}
			}
			failed, buildErr := plan.Build(ctx, postgres.New(), current, desired, plan.Options{Diff: schema.DiffOptions{RenameHints: []schema.RenameHint{hint}}})
			if buildErr == nil || len(failed.Steps) != 0 {
				t.Fatalf("planned=%+v err=%v", failed, buildErr)
			}
		})
	}
}

func TestMaterializedViewRenameDependentGuardAndBareConvergence(t *testing.T) {
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
	_, err = conn.Exec(ctx, `drop schema if exists autosql_mvrename cascade; create schema autosql_mvrename; create table autosql_mvrename.widgets(id bigint); create materialized view autosql_mvrename.widget_mv as select id from autosql_mvrename.widgets; create index widget_mv_id_idx on autosql_mvrename.widget_mv(id);`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop schema if exists autosql_mvrename cascade`)
	inspect := func() schema.Document {
		doc, e := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_mvrename"}})
		if e != nil {
			t.Fatal(e)
		}
		doc, e = postgres.New().Normalize(ctx, doc)
		if e != nil {
			t.Fatal(e)
		}
		return doc
	}
	rename := func(current schema.Document) (schema.Document, []schema.RenameHint) {
		var before schema.Resource
		for _, r := range current.Graph.Resources {
			if r.Kind == schema.KindMaterializedView {
				before = r
			}
		}
		after := before
		after.Name.Name = "widget_mv_new"
		after.ID = schema.StableID(after.Kind, after.Name)
		desired := current
		hints := []schema.RenameHint{{From: before.ID, To: after.ID}}
		desired.Graph.Resources = nil
		for _, original := range current.Graph.Resources {
			r := original
			r.Dependencies = append([]schema.Dependency(nil), r.Dependencies...)
			if r.ID == before.ID {
				r = after
			} else {
				if r.Name.Parent == before.ID {
					r.Name.Parent = after.ID
				}
				for i := range r.Dependencies {
					if r.Dependencies[i].Target == before.ID {
						r.Dependencies[i].Target = after.ID
					}
				}
				old := r.ID
				r.ID = schema.StableID(r.Kind, r.Name)
				if old != r.ID && r.Kind == schema.KindColumn {
					hints = append(hints, schema.RenameHint{From: old, To: r.ID})
				}
			}
			desired.Graph.Resources = append(desired.Graph.Resources, r)
		}
		desired.Normalize()
		return desired, hints
	}
	current := inspect()
	desired, hints := rename(current)
	failed, buildErr := plan.Build(ctx, postgres.New(), current, desired, plan.Options{Diff: schema.DiffOptions{RenameHints: hints}})
	if buildErr == nil || len(failed.Steps) != 0 {
		t.Fatalf("indexed MV rename planned=%+v err=%v", failed, buildErr)
	}
	if _, err = conn.Exec(ctx, `drop index autosql_mvrename.widget_mv_id_idx`); err != nil {
		t.Fatal(err)
	}
	current = inspect()
	desired, hints = rename(current)
	p, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{Diff: schema.DiffOptions{RenameHints: hints}})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, p)
	actual := inspect()
	assertFingerprint(t, actual, desired)
	noop, err := plan.Build(ctx, postgres.New(), actual, desired, plan.Options{})
	if err != nil || len(noop.Steps) != 0 {
		t.Fatalf("bare MV second plan=%+v err=%v", noop, err)
	}
}

func renameFixture(doc schema.Document, newSchemaName, newTableName string) (schema.Document, string, string, string, string) {
	var oldSchema, oldTable schema.Resource
	for _, r := range doc.Graph.Resources {
		if r.Kind == schema.KindSchema {
			oldSchema = r
		}
		if r.Kind == schema.KindTable {
			oldTable = r
		}
	}
	newSchema := oldSchema
	newSchema.Name.Name = newSchemaName
	newSchema.ID = schema.StableID(newSchema.Kind, newSchema.Name)
	newTable := oldTable
	newTable.Dependencies = append([]schema.Dependency(nil), oldTable.Dependencies...)
	newTable.Name.Schema = newSchemaName
	newTable.Name.Name = newTableName
	newTable.Name.Parent = newSchema.ID
	newTable.ID = schema.StableID(newTable.Kind, newTable.Name)
	mapping := map[string]string{oldSchema.ID: newSchema.ID, oldTable.ID: newTable.ID}
	out := doc
	out.Graph.Resources = make([]schema.Resource, 0, len(doc.Graph.Resources))
	for _, r := range doc.Graph.Resources {
		r.Dependencies = append([]schema.Dependency(nil), r.Dependencies...)
		oldID := r.ID
		switch r.ID {
		case oldSchema.ID:
			r = newSchema
		case oldTable.ID:
			r = newTable
		default:
			r.Name.Schema = newSchemaName
			if mapped := mapping[r.Name.Parent]; mapped != "" {
				r.Name.Parent = mapped
			}
			for i := range r.Dependencies {
				if mapped := mapping[r.Dependencies[i].Target]; mapped != "" {
					r.Dependencies[i].Target = mapped
				}
			}
			r.ID = schema.StableID(r.Kind, r.Name)
		}
		mapping[oldID] = r.ID
		out.Graph.Resources = append(out.Graph.Resources, r)
	}
	for idx := range out.Graph.Resources {
		r := &out.Graph.Resources[idx]
		if mapped := mapping[r.Name.Parent]; mapped != "" {
			r.Name.Parent = mapped
		}
		for i := range r.Dependencies {
			if mapped := mapping[r.Dependencies[i].Target]; mapped != "" {
				r.Dependencies[i].Target = mapped
			}
		}
	}
	out.Normalize()
	return out, oldSchema.ID, newSchema.ID, oldTable.ID, newTable.ID
}
func applyTestPlan(t *testing.T, ctx context.Context, conn *pgx.Conn, p plan.Plan) {
	t.Helper()
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range p.Steps {
		if step.Kind == plan.StepTopology {
			continue
		}
		if _, err = tx.Exec(ctx, step.SQL); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("%s: %v", step.SQL, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
}
func assertFingerprint(t *testing.T, actual, desired schema.Document) {
	t.Helper()
	a, _ := schema.SemanticFingerprint(actual)
	d, _ := schema.SemanticFingerprint(desired)
	if a != d {
		t.Fatalf("fingerprint got=%s want=%s", a, d)
	}
}
