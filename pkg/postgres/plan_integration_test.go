package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"autosql/pkg/plan"
	"autosql/pkg/plugin"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/secret"
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

func TestConstraintIndexBootstrapParity(t *testing.T) {
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
	defer conn.Exec(context.Background(), `drop schema if exists autosql_constraint_bootstrap cascade`)
	fixture := `
drop schema if exists autosql_constraint_bootstrap cascade;
create schema autosql_constraint_bootstrap;
create function autosql_constraint_bootstrap.is_positive(value numeric) returns boolean
language sql immutable strict as $$ select value > 0 $$;
create function autosql_constraint_bootstrap.default_label() returns text
language sql stable as $$ select 'ready'::text $$;
create table autosql_constraint_bootstrap.accounts (
  tenant_id bigint not null,
  account_id bigint not null,
  email text not null,
  balance numeric(12,2) not null,
  label text not null default autosql_constraint_bootstrap.default_label(),
  active boolean not null,
  constraint accounts_pkey primary key (tenant_id, account_id),
  constraint accounts_tenant_email_key unique (tenant_id, email) deferrable initially immediate,
  constraint accounts_balance_check check (autosql_constraint_bootstrap.is_positive(balance)) not valid
);
comment on constraint accounts_pkey on autosql_constraint_bootstrap.accounts is 'composite identity';
create table autosql_constraint_bootstrap.orders (
  tenant_id bigint not null,
  order_id bigint not null,
  account_id bigint not null,
  amount numeric(12,2) not null,
  constraint orders_pkey primary key (tenant_id, order_id),
  constraint orders_account_fkey foreign key (tenant_id, account_id)
    references autosql_constraint_bootstrap.accounts(tenant_id, account_id)
    on update cascade on delete restrict deferrable initially deferred
);
create unique index orders_positive_amount_idx on autosql_constraint_bootstrap.orders
  using btree (tenant_id, order_id) include (account_id) with (fillfactor=80) where amount > 0;
create index orders_amount_expr_idx on autosql_constraint_bootstrap.orders
  using btree ((amount * 100)) where autosql_constraint_bootstrap.is_positive(amount);
comment on index autosql_constraint_bootstrap.orders_positive_amount_idx is 'positive order lookup';
create table autosql_constraint_bootstrap.node_a (
  id bigint primary key,
  b_id bigint,
  parent_id bigint,
  constraint node_a_parent_fkey foreign key (parent_id)
    references autosql_constraint_bootstrap.node_a(id) not valid
);
create table autosql_constraint_bootstrap.node_b (
  id bigint primary key,
  a_id bigint
);
alter table autosql_constraint_bootstrap.node_a add constraint node_a_b_fkey
  foreign key (b_id) references autosql_constraint_bootstrap.node_b(id) not valid;
alter table autosql_constraint_bootstrap.node_b add constraint node_b_a_fkey
  foreign key (a_id) references autosql_constraint_bootstrap.node_a(id) not valid;`
	if _, err = conn.Exec(ctx, fixture); err != nil {
		t.Fatal(err)
	}

	desired, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_constraint_bootstrap"}})
	if err != nil {
		t.Fatal(err)
	}
	hcl, err := source.FormatHCL(desired)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadContext(ctx, source.Input{URI: "constraint-bootstrap.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	renderOptions := reviewedRoutineRenderOptions(desired)
	if report, reportErr := postgres.PreflightProvisioning(ctx, desired, renderOptions); reportErr != nil || !report.Supported {
		t.Fatalf("preflight=%+v err=%v", report, reportErr)
	}

	if _, err = conn.Exec(ctx, `drop schema autosql_constraint_bootstrap cascade`); err != nil {
		t.Fatal(err)
	}
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_constraint_bootstrap"}})
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{Render: renderOptions})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, bootstrap)
	actual, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_constraint_bootstrap"}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = postgres.New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, actual, desired)
	noop, err := plan.Build(ctx, postgres.New(), actual, desired, plan.Options{})
	if err != nil || len(noop.Changes.Changes) != 0 || len(noop.Steps) != 0 {
		t.Fatalf("constraint/index bootstrap did not converge: changes=%d steps=%d err=%v", len(noop.Changes.Changes), len(noop.Steps), err)
	}
}

func TestFunctionReplaceRenameDropLifecycle(t *testing.T) {
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
	defer conn.Exec(context.Background(), `drop schema if exists autosql_function_lifecycle cascade`)
	const createOne = `create or replace function autosql_function_lifecycle.bump(value integer) returns integer language sql immutable strict parallel safe as $$ select value + 1 $$`
	const createTwo = `create or replace function autosql_function_lifecycle.bump(value integer) returns integer language sql immutable strict parallel safe as $$ select value + 2 $$`
	if _, err = conn.Exec(ctx, `drop schema if exists autosql_function_lifecycle cascade; create schema autosql_function_lifecycle; `+createOne+`; comment on function autosql_function_lifecycle.bump(integer) is 'reviewed arithmetic';`); err != nil {
		t.Fatal(err)
	}
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_function_lifecycle"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, createTwo); err != nil {
		t.Fatal(err)
	}
	desired, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_function_lifecycle"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, createOne); err != nil {
		t.Fatal(err)
	}
	replace, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{Render: reviewedRoutineRenderOptions(desired)})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, replace)
	actual, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_function_lifecycle"}})
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, actual, desired)

	if _, err = conn.Exec(ctx, `alter function autosql_function_lifecycle.bump(integer) rename to bump_v2`); err != nil {
		t.Fatal(err)
	}
	renamed, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_function_lifecycle"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `alter function autosql_function_lifecycle.bump_v2(integer) rename to bump`); err != nil {
		t.Fatal(err)
	}
	current, err = postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_function_lifecycle"}})
	if err != nil {
		t.Fatal(err)
	}
	var oldID, newID string
	for _, resource := range current.Graph.Resources {
		if resource.Kind == schema.KindFunction {
			oldID = resource.ID
		}
	}
	for _, resource := range renamed.Graph.Resources {
		if resource.Kind == schema.KindFunction {
			newID = resource.ID
		}
	}
	renamePlan, err := plan.Build(ctx, postgres.New(), current, renamed, plan.Options{Diff: schema.DiffOptions{RenameHints: []schema.RenameHint{{From: oldID, To: newID}}}, Render: reviewedRoutineRenderOptions(renamed)})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, renamePlan)
	actual, err = postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_function_lifecycle"}})
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, actual, renamed)

	withoutFunction := renamed
	withoutFunction.Graph.Resources = slices.DeleteFunc(append([]schema.Resource(nil), renamed.Graph.Resources...), func(resource schema.Resource) bool { return resource.Kind == schema.KindFunction })
	withoutFunction, err = postgres.New().Normalize(ctx, withoutFunction)
	if err != nil {
		t.Fatal(err)
	}
	dropPlan, err := plan.Build(ctx, postgres.New(), actual, withoutFunction, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, dropPlan)
	finalState, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_function_lifecycle"}})
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, finalState, withoutFunction)
}

func TestProcedureBootstrapReplaceParity(t *testing.T) {
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
	defer conn.Exec(context.Background(), `drop schema if exists autosql_procedure_lifecycle cascade`)
	const one = `create or replace procedure autosql_procedure_lifecycle.bump(IN value integer, INOUT result integer) language plpgsql as $$ begin result := value + 1; end $$`
	const two = `create or replace procedure autosql_procedure_lifecycle.bump(IN value integer, INOUT result integer) language plpgsql as $$ begin result := value + 2; end $$`
	if _, err = conn.Exec(ctx, `drop schema if exists autosql_procedure_lifecycle cascade; create schema autosql_procedure_lifecycle; `+one+`; comment on procedure autosql_procedure_lifecycle.bump(integer,integer) is 'reviewed procedure';`); err != nil {
		t.Fatal(err)
	}
	desired, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_procedure_lifecycle"}})
	if err != nil {
		t.Fatal(err)
	}
	options := reviewedRoutineRenderOptions(desired)
	if _, err = conn.Exec(ctx, `drop schema autosql_procedure_lifecycle cascade`); err != nil {
		t.Fatal(err)
	}
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_procedure_lifecycle"}})
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{Render: options})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, bootstrap)
	actual, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_procedure_lifecycle"}})
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, actual, desired)

	if _, err = conn.Exec(ctx, two); err != nil {
		t.Fatal(err)
	}
	replaced, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_procedure_lifecycle"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, one); err != nil {
		t.Fatal(err)
	}
	current, err = postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_procedure_lifecycle"}})
	if err != nil {
		t.Fatal(err)
	}
	replace, err := plan.Build(ctx, postgres.New(), current, replaced, plan.Options{Render: reviewedRoutineRenderOptions(replaced)})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, replace)
	actual, err = postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_procedure_lifecycle"}})
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, actual, replaced)
	noop, err := plan.Build(ctx, postgres.New(), actual, replaced, plan.Options{})
	if err != nil || len(noop.Steps) != 0 {
		t.Fatalf("procedure lifecycle did not converge: steps=%d err=%v", len(noop.Steps), err)
	}
}

func TestTriggerFamilyBootstrapParity(t *testing.T) {
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
	defer conn.Exec(context.Background(), `drop schema if exists autosql_trigger_bootstrap cascade`)
	fixture := `
drop schema if exists autosql_trigger_bootstrap cascade;
create schema autosql_trigger_bootstrap;
create table autosql_trigger_bootstrap.items (
  id bigint primary key,
  value text,
  updated_at timestamptz
);
create view autosql_trigger_bootstrap.item_view as select id, value, updated_at from autosql_trigger_bootstrap.items;
create function autosql_trigger_bootstrap.touch_updated_at() returns trigger
language plpgsql as $$ begin new.updated_at := current_timestamp; return new; end $$;
create function autosql_trigger_bootstrap.audit_event() returns trigger
language plpgsql as $$ begin return coalesce(new, old); end $$;
create function autosql_trigger_bootstrap.route_view_insert() returns trigger
language plpgsql as $$ begin insert into autosql_trigger_bootstrap.items(id,value) values (new.id,new.value); return new; end $$;
create trigger items_touch before insert or update on autosql_trigger_bootstrap.items
for each row execute function autosql_trigger_bootstrap.touch_updated_at();
create trigger items_audit_insert after insert on autosql_trigger_bootstrap.items
for each statement execute function autosql_trigger_bootstrap.audit_event();
create trigger items_value_audit before update of value on autosql_trigger_bootstrap.items
for each row when (old.value is distinct from new.value)
execute function autosql_trigger_bootstrap.audit_event('value');
create constraint trigger items_deferred_audit after insert on autosql_trigger_bootstrap.items
deferrable initially deferred for each row execute function autosql_trigger_bootstrap.audit_event();
create trigger items_transition_audit after update on autosql_trigger_bootstrap.items
referencing old table as old_rows new table as new_rows for each statement
execute function autosql_trigger_bootstrap.audit_event();
create trigger item_view_insert instead of insert on autosql_trigger_bootstrap.item_view
for each row execute function autosql_trigger_bootstrap.route_view_insert();
alter table autosql_trigger_bootstrap.items disable trigger items_audit_insert;
alter table autosql_trigger_bootstrap.items enable replica trigger items_value_audit;
alter table autosql_trigger_bootstrap.items enable always trigger items_transition_audit;
comment on trigger items_touch on autosql_trigger_bootstrap.items is 'updated_at behavior';`
	if _, err = conn.Exec(ctx, fixture); err != nil {
		t.Fatal(err)
	}
	desired, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_trigger_bootstrap"}})
	if err != nil {
		t.Fatal(err)
	}
	hcl, err := source.FormatHCL(desired)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadContext(ctx, source.Input{URI: "trigger-family.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	triggerCount := 0
	for _, resource := range desired.Graph.Resources {
		if resource.Kind == schema.KindTrigger {
			triggerCount++
		}
	}
	if triggerCount != 6 {
		t.Fatalf("trigger inventory=%d want 6", triggerCount)
	}
	options := reviewedRoutineRenderOptions(desired)
	if report, reportErr := postgres.PreflightProvisioning(ctx, desired, options); reportErr != nil || !report.Supported {
		t.Fatalf("trigger preflight=%+v err=%v", report, reportErr)
	}
	if _, err = conn.Exec(ctx, `drop schema autosql_trigger_bootstrap cascade`); err != nil {
		t.Fatal(err)
	}
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_trigger_bootstrap"}})
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{Render: options})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, bootstrap)
	actual, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_trigger_bootstrap"}})
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
		t.Fatalf("trigger bootstrap did not converge: steps=%d err=%v", len(noop.Steps), err)
	}
}

func TestConcurrentIndexCreateRebuildDropLifecycle(t *testing.T) {
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
	defer conn.Exec(context.Background(), `drop schema if exists autosql_concurrent_index cascade`)
	if _, err = conn.Exec(ctx, `drop schema if exists autosql_concurrent_index cascade; create schema autosql_concurrent_index; create table autosql_concurrent_index.items(id bigint primary key, value text, payload text);`); err != nil {
		t.Fatal(err)
	}
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_concurrent_index"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `create index items_value_idx on autosql_concurrent_index.items using btree (value text_pattern_ops) include (payload) with (fillfactor=80) where value is not null; comment on index autosql_concurrent_index.items_value_idx is 'online index';`); err != nil {
		t.Fatal(err)
	}
	desired, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_concurrent_index"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `drop index autosql_concurrent_index.items_value_idx`); err != nil {
		t.Fatal(err)
	}
	options := map[string]string{"concurrent_indexes": "true"}
	createPlan, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{Render: options})
	if err != nil {
		t.Fatal(err)
	}
	if !planContainsTransactionMode(createPlan, plan.TransactionProhibited) {
		t.Fatal("concurrent create did not produce a non-transactional phase")
	}
	applyTestPlanPhased(t, ctx, conn, createPlan)
	actual, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_concurrent_index"}})
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, actual, desired)

	if _, err = conn.Exec(ctx, `drop index autosql_concurrent_index.items_value_idx; create index items_value_idx on autosql_concurrent_index.items (value desc) where value is not null; comment on index autosql_concurrent_index.items_value_idx is 'rebuilt online';`); err != nil {
		t.Fatal(err)
	}
	rebuilt, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_concurrent_index"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `drop index autosql_concurrent_index.items_value_idx; create index items_value_idx on autosql_concurrent_index.items using btree (value text_pattern_ops) include (payload) with (fillfactor=80) where value is not null; comment on index autosql_concurrent_index.items_value_idx is 'online index';`); err != nil {
		t.Fatal(err)
	}
	current, err = postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_concurrent_index"}})
	if err != nil {
		t.Fatal(err)
	}
	rebuildPlan, err := plan.Build(ctx, postgres.New(), current, rebuilt, plan.Options{Render: map[string]string{"allow_rebuild": "true", "concurrent_indexes": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(rebuildPlan.Phases) < 2 || !planContainsTransactionMode(rebuildPlan, plan.TransactionProhibited) {
		t.Fatalf("online rebuild phases=%+v", rebuildPlan.Phases)
	}
	applyTestPlanPhased(t, ctx, conn, rebuildPlan)
	actual, err = postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_concurrent_index"}})
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, actual, rebuilt)

	withoutIndex := rebuilt
	withoutIndex.Graph.Resources = slices.DeleteFunc(append([]schema.Resource(nil), rebuilt.Graph.Resources...), func(resource schema.Resource) bool { return resource.Kind == schema.KindIndex })
	withoutIndex, err = postgres.New().Normalize(ctx, withoutIndex)
	if err != nil {
		t.Fatal(err)
	}
	dropPlan, err := plan.Build(ctx, postgres.New(), actual, withoutIndex, plan.Options{Render: options})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlanPhased(t, ctx, conn, dropPlan)
	finalState, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_concurrent_index"}})
	if err != nil {
		t.Fatal(err)
	}
	assertFingerprint(t, finalState, withoutIndex)
}

func TestConstraintIndexInventoryScaleParity(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	defer conn.Exec(context.Background(), `drop schema if exists autosql_inventory_scale cascade`)
	var fixture strings.Builder
	fixture.WriteString(`drop schema if exists autosql_inventory_scale cascade; create schema autosql_inventory_scale;`)
	for index := 0; index < 69; index++ {
		fmt.Fprintf(&fixture, `create table autosql_inventory_scale.t%02d(id bigint not null,parent_id bigint,value integer not null,unique_value bigint not null,constraint t%02d_pkey primary key(id)`, index, index)
		if index < 27 {
			fmt.Fprintf(&fixture, `,constraint t%02d_unique unique(unique_value)`, index)
		}
		if index < 45 {
			fmt.Fprintf(&fixture, `,constraint t%02d_check check(value >= 0)`, index)
		}
		fixture.WriteString(`);`)
	}
	for index := 1; index <= 56; index++ {
		fmt.Fprintf(&fixture, `alter table autosql_inventory_scale.t%02d add constraint t%02d_parent_fkey foreign key(parent_id) references autosql_inventory_scale.t00(id) on delete restrict;`, index, index)
	}
	for index := 0; index < 315; index++ {
		fmt.Fprintf(&fixture, `create index scale_idx_%03d on autosql_inventory_scale.t%02d(value);`, index, index%69)
	}
	if _, err = conn.Exec(ctx, fixture.String()); err != nil {
		t.Fatal(err)
	}
	desired, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_inventory_scale"}})
	if err != nil {
		t.Fatal(err)
	}
	wantCounts := map[schema.Kind]int{schema.KindIndex: 315, schema.KindPrimaryKey: 69, schema.KindForeignKey: 56, schema.KindCheckConstraint: 45, schema.KindUniqueConstraint: 27}
	gotCounts := map[schema.Kind]int{}
	for _, resource := range desired.Graph.Resources {
		gotCounts[resource.Kind]++
	}
	for kind, count := range wantCounts {
		if gotCounts[kind] != count {
			t.Fatalf("%s count=%d want=%d inventory=%v", kind, gotCounts[kind], count, gotCounts)
		}
	}
	hcl, err := source.FormatHCL(desired)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadContext(ctx, source.Input{URI: "inventory-scale.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	if report, reportErr := postgres.PreflightProvisioning(ctx, desired, nil); reportErr != nil || !report.Supported {
		t.Fatalf("scale preflight=%+v err=%v", report, reportErr)
	}
	if _, err = conn.Exec(ctx, `drop schema autosql_inventory_scale cascade`); err != nil {
		t.Fatal(err)
	}
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_inventory_scale"}})
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, bootstrap)
	actual, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_inventory_scale"}})
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
		t.Fatalf("scale bootstrap did not converge: steps=%d err=%v", len(noop.Steps), err)
	}
}

func TestRoutineTriggerInventoryScaleParity(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	defer conn.Exec(context.Background(), `drop schema if exists autosql_routine_scale_cell cascade; drop schema if exists autosql_routine_scale_repo cascade`)
	var fixture strings.Builder
	fixture.WriteString(`drop schema if exists autosql_routine_scale_cell cascade; drop schema if exists autosql_routine_scale_repo cascade; create schema autosql_routine_scale_cell; create schema autosql_routine_scale_repo;`)
	for index := 0; index < 15; index++ {
		fmt.Fprintf(&fixture, `create function autosql_routine_scale_cell.cell_fn_%02d(value integer) returns integer language sql immutable strict parallel safe as $$ select value + %d $$;`, index, index)
	}
	fixture.WriteString(`create table autosql_routine_scale_repo.events(id bigint primary key, value integer);`)
	for index := 0; index < 6; index++ {
		fmt.Fprintf(&fixture, `create function autosql_routine_scale_repo.trigger_fn_%02d() returns trigger language plpgsql as $$ begin return new; end $$;`, index)
	}
	for index := 0; index < 18; index++ {
		fmt.Fprintf(&fixture, `create function autosql_routine_scale_repo.repo_fn_%02d(value integer) returns integer language sql stable as $$ select value + %d $$;`, index, index)
	}
	for index := 0; index < 8; index++ {
		fmt.Fprintf(&fixture, `create procedure autosql_routine_scale_repo.repo_proc_%02d(IN value integer, INOUT result integer) language plpgsql as $$ begin result := value + %d; end $$;`, index, index)
	}
	for index := 0; index < 6; index++ {
		fmt.Fprintf(&fixture, `create trigger repo_trigger_%02d before insert or update on autosql_routine_scale_repo.events for each row execute function autosql_routine_scale_repo.trigger_fn_%02d();`, index, index)
	}
	fixture.WriteString(`comment on function autosql_routine_scale_cell.cell_fn_00(integer) is 'cell routine'; comment on trigger repo_trigger_00 on autosql_routine_scale_repo.events is 'repository trigger';`)
	if _, err = conn.Exec(ctx, fixture.String()); err != nil {
		t.Fatal(err)
	}
	schemas := []string{"autosql_routine_scale_cell", "autosql_routine_scale_repo"}
	desired, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: schemas})
	if err != nil {
		t.Fatal(err)
	}
	cellRoutines, repositoryRoutines, triggers := 0, 0, 0
	for _, resource := range desired.Graph.Resources {
		if resource.Kind == schema.KindFunction || resource.Kind == schema.KindProcedure {
			if resource.Name.Schema == schemas[0] {
				cellRoutines++
			} else if resource.Name.Schema == schemas[1] {
				repositoryRoutines++
			}
		}
		if resource.Kind == schema.KindTrigger {
			triggers++
		}
	}
	if cellRoutines != 15 || repositoryRoutines != 32 || triggers != 6 {
		t.Fatalf("inventory cell=%d repo=%d triggers=%d", cellRoutines, repositoryRoutines, triggers)
	}
	hcl, err := source.FormatHCL(desired)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadContext(ctx, source.Input{URI: "routine-trigger-scale.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	options := reviewedRoutineRenderOptions(desired)
	if report, reportErr := postgres.PreflightProvisioning(ctx, desired, options); reportErr != nil || !report.Supported {
		t.Fatalf("routine scale preflight=%+v err=%v", report, reportErr)
	}
	if _, err = conn.Exec(ctx, `drop schema autosql_routine_scale_cell cascade; drop schema autosql_routine_scale_repo cascade`); err != nil {
		t.Fatal(err)
	}
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: schemas})
	if err != nil {
		t.Fatal(err)
	}
	bootstrap, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{Render: options})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, bootstrap)
	actual, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: schemas})
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
		t.Fatalf("routine/trigger scale did not converge: steps=%d err=%v", len(noop.Steps), err)
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

func filterRLSInventory(document schema.Document, namespace, role string) schema.Document {
	keep := map[string]bool{}
	for _, resource := range document.Graph.Resources {
		securityObject := resource.Kind == schema.KindGrant || resource.Kind == schema.KindMembership || resource.Kind == schema.KindDefaultPrivilege
		if !securityObject && (resource.Name.Schema == namespace || resource.Kind == schema.KindSchema && resource.Name.Name == namespace || resource.Kind == schema.KindRole && resource.Name.Name == role) {
			keep[resource.ID] = true
		}
	}
	filtered := document
	filtered.Graph.Resources = nil
	for _, resource := range document.Graph.Resources {
		if !keep[resource.ID] {
			continue
		}
		dependencies := resource.Dependencies[:0]
		for _, dependency := range resource.Dependencies {
			if keep[dependency.Target] && dependency.Type != schema.DependencyOwns {
				dependencies = append(dependencies, dependency)
			}
		}
		resource.Dependencies = dependencies
		if resource.Kind != schema.KindRole {
			var specification map[string]any
			if json.Unmarshal(resource.Spec, &specification) == nil {
				delete(specification, "owner")
				resource.Spec, _ = json.Marshal(specification)
			}
		}
		filtered.Graph.Resources = append(filtered.Graph.Resources, resource)
	}
	filtered.Normalize()
	return filtered
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
	reviewOptions := reviewedRoutineRenderOptions(desired)
	fullReport, err := postgres.PreflightProvisioning(ctx, desired, reviewOptions)
	if err != nil {
		t.Fatal(err)
	}
	if !fullReport.Supported {
		t.Fatalf("review-authorized complete cell still has provisioning blockers: %+v", fullReport.Diagnostics)
	}

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
	fresh, err := plan.Build(ctx, postgres.New(), current, managed, plan.Options{Render: reviewOptions})
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
	noop, err := plan.Build(ctx, postgres.New(), actual, managed, plan.Options{Render: reviewOptions})
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
	incremental, err := plan.Build(ctx, postgres.New(), actual, added, plan.Options{Render: reviewOptions})
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
	finalNoop, err := plan.Build(ctx, postgres.New(), finalState, added, plan.Options{Render: reviewOptions})
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

func reviewedRoutineRenderOptions(document schema.Document) map[string]string {
	var digests []string
	for _, resource := range document.Graph.Resources {
		if resource.Kind != schema.KindFunction && resource.Kind != schema.KindProcedure {
			continue
		}
		var values map[string]any
		_ = json.Unmarshal(resource.Spec, &values)
		if digest, ok := values["body_digest"].(string); ok && digest != "" {
			digests = append(digests, digest)
		}
	}
	return map[string]string{"reviewed_routine_digests": strings.Join(digests, ",")}
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

func TestRLSPolicyInventoryBootstrapParity(t *testing.T) {
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
	defer conn.Close(context.Background())
	const namespace = "autosql_rls_scale"
	const role = "autosql_rls_scale_app"
	defer func() {
		_, _ = conn.Exec(context.Background(), `drop schema if exists autosql_rls_scale cascade`)
		_, _ = conn.Exec(context.Background(), `drop role if exists autosql_rls_scale_app`)
	}()
	if _, err = conn.Exec(ctx, `drop schema if exists autosql_rls_scale cascade; drop role if exists autosql_rls_scale_app; create role autosql_rls_scale_app; create schema autosql_rls_scale`); err != nil {
		t.Fatal(err)
	}
	for index := 0; index < 7; index++ {
		table := fmt.Sprintf("tenant_data_%02d", index)
		fixture := fmt.Sprintf(`
create table %s.%s (tenant_id uuid not null, payload text);
alter table %s.%s enable row level security;
alter table %s.%s force row level security;
create policy %s_select on %s.%s as permissive for select to %s using (tenant_id::text = current_setting('app.tenant_id'));
create policy %s_insert on %s.%s as permissive for insert to %s with check (tenant_id::text = current_setting('app.tenant_id'));
`, namespace, table, namespace, table, namespace, table, table, namespace, table, role, table, namespace, table, role)
		if _, err = conn.Exec(ctx, fixture); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = conn.Exec(ctx, `grant usage on schema autosql_rls_scale to autosql_rls_scale_app; grant select,insert on all tables in schema autosql_rls_scale to autosql_rls_scale_app`); err != nil {
		t.Fatal(err)
	}
	const tenantA, tenantB = "11111111-1111-1111-1111-111111111111", "22222222-2222-2222-2222-222222222222"
	for index := 0; index < 7; index++ {
		table := fmt.Sprintf("tenant_data_%02d", index)
		tx, txErr := conn.Begin(ctx)
		if txErr != nil {
			t.Fatal(txErr)
		}
		if _, txErr = tx.Exec(ctx, `set local role autosql_rls_scale_app`); txErr == nil {
			_, txErr = tx.Exec(ctx, `select set_config('app.tenant_id',$1,true)`, tenantA)
		}
		if txErr == nil {
			_, txErr = tx.Exec(ctx, fmt.Sprintf(`insert into autosql_rls_scale.%s(tenant_id,payload) values($1,'owned')`, table), tenantA)
		}
		if txErr == nil {
			_, txErr = tx.Exec(ctx, `select set_config('app.tenant_id',$1,true)`, tenantB)
		}
		var visible int
		if txErr == nil {
			txErr = tx.QueryRow(ctx, fmt.Sprintf(`select count(*) from autosql_rls_scale.%s`, table)).Scan(&visible)
		}
		if txErr != nil || visible != 0 {
			_ = tx.Rollback(ctx)
			t.Fatalf("tenant isolation table=%s visible=%d err=%v", table, visible, txErr)
		}
		if _, txErr = tx.Exec(ctx, `savepoint denied_insert`); txErr != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(txErr)
		}
		if _, txErr = tx.Exec(ctx, fmt.Sprintf(`insert into autosql_rls_scale.%s(tenant_id,payload) values($1,'cross-tenant')`, table), tenantA); txErr == nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("cross-tenant insert succeeded for %s", table)
		}
		if _, rollbackErr := tx.Exec(ctx, `rollback to savepoint denied_insert`); rollbackErr != nil {
			_ = tx.Rollback(ctx)
			t.Fatal(rollbackErr)
		}
		if txErr = tx.Commit(ctx); txErr != nil {
			t.Fatal(txErr)
		}
	}

	desired, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}, Advanced: true})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	desired = filterRLSInventory(desired, namespace, role)
	policies, rlsTables := 0, 0
	for _, resource := range desired.Graph.Resources {
		switch resource.Kind {
		case schema.KindPolicy:
			policies++
			if len(resource.Dependencies) != 3 {
				t.Fatalf("policy %s dependencies=%+v", resource.Name.String(), resource.Dependencies)
			}
		case schema.KindTable:
			var specification map[string]any
			_ = json.Unmarshal(resource.Spec, &specification)
			if specification["row_security"] == true && specification["force_row_security"] == true {
				rlsTables++
			}
		}
	}
	if policies != 14 || rlsTables != 7 {
		t.Fatalf("policies=%d rls_tables=%d", policies, rlsTables)
	}
	hcl, err := source.FormatHCL(desired)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadContext(ctx, source.Input{URI: "rls-scale.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = conn.Exec(ctx, `drop schema autosql_rls_scale cascade`); err != nil {
		t.Fatal(err)
	}
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}, Advanced: true})
	if err != nil {
		t.Fatal(err)
	}
	current, err = postgres.New().Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	current = filterRLSInventory(current, namespace, role)
	fresh, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, fresh)
	actual, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}, Advanced: true})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = postgres.New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	actual = filterRLSInventory(actual, namespace, role)
	assertFingerprint(t, actual, desired)
	noop, err := plan.Build(ctx, postgres.New(), actual, desired, plan.Options{})
	if err != nil || len(noop.Steps) != 0 || len(noop.Changes.Changes) != 0 {
		t.Fatalf("RLS second plan changes=%d steps=%d err=%v", len(noop.Changes.Changes), len(noop.Steps), err)
	}

	// Exercise rename, predicate replacement, and drop against the live
	// catalog. RLS/FORCE remain enabled throughout each transition.
	var beforePolicy schema.Resource
	for _, resource := range actual.Graph.Resources {
		if resource.Kind == schema.KindPolicy && strings.HasSuffix(resource.Name.Name, "_select") {
			beforePolicy = resource
			break
		}
	}
	renamedPolicy := beforePolicy
	renamedPolicy.Name.Name += "_renamed"
	renamedPolicy.ID = schema.StableID(renamedPolicy.Kind, renamedPolicy.Name)
	renamed := replaceDocumentResource(actual, beforePolicy.ID, renamedPolicy)
	renamePlan, err := plan.Build(ctx, postgres.New(), actual, renamed, plan.Options{Diff: schema.DiffOptions{RenameHints: []schema.RenameHint{{From: beforePolicy.ID, To: renamedPolicy.ID}}}})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, renamePlan)
	current = inspectFilteredRLS(t, ctx, url, namespace, role)
	assertFingerprint(t, current, renamed)

	alteredPolicy := renamedPolicy
	var alteredSpec map[string]any
	if err = json.Unmarshal(alteredPolicy.Spec, &alteredSpec); err != nil {
		t.Fatal(err)
	}
	alteredSpec["using"] = strings.ReplaceAll(alteredSpec["using"].(string), "app.tenant_id", "app.alt_tenant")
	alteredPolicy.Spec, _ = json.Marshal(alteredSpec)
	altered := replaceDocumentResource(current, alteredPolicy.ID, alteredPolicy)
	alterPlan, err := plan.Build(ctx, postgres.New(), current, altered, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, alterPlan)
	current = inspectFilteredRLS(t, ctx, url, namespace, role)
	assertFingerprint(t, current, altered)

	dropped := current
	dropped.Graph.Resources = nil
	for _, resource := range current.Graph.Resources {
		if resource.ID != alteredPolicy.ID {
			dropped.Graph.Resources = append(dropped.Graph.Resources, resource)
		}
	}
	dropPlan, err := plan.Build(ctx, postgres.New(), current, dropped, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, dropPlan)
	current = inspectFilteredRLS(t, ctx, url, namespace, role)
	assertFingerprint(t, current, dropped)
}

func TestRoleLifecycleAndWriteOnlyPassword(t *testing.T) {
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
	defer func() {
		_, _ = conn.Exec(context.Background(), `drop schema if exists autosql_role_owned cascade`)
		_, _ = conn.Exec(context.Background(), `drop role if exists autosql_role_app_v2; drop role if exists autosql_role_app; drop role if exists autosql_role_owner`)
	}()
	if _, err = conn.Exec(ctx, `drop schema if exists autosql_role_owned cascade; drop role if exists autosql_role_app_v2; drop role if exists autosql_role_app; drop role if exists autosql_role_owner`); err != nil {
		t.Fatal(err)
	}
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}, Annotations: map[string]string{"dialect": "postgresql"}}
	owner := integrationRole("autosql_role_owner", false, []string{"search_path=public"})
	app := integrationRole("autosql_role_app", true, []string{"search_path=public", "statement_timeout=5s"})
	desired := empty
	desired.Graph.Resources = []schema.Resource{owner, app}
	desired.Normalize()
	fresh, err := plan.Build(ctx, postgres.New(), empty, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, fresh)
	actual := inspectRolesNamed(t, ctx, url, "autosql_role_owner", "autosql_role_app")
	assertFingerprint(t, actual, desired)

	namespace := schema.Resource{Kind: schema.KindSchema, Name: schema.Name{Name: "autosql_role_owned"}, Dependencies: []schema.Dependency{{Target: app.ID, Type: schema.DependencyOwns}}, Spec: json.RawMessage(`{"owner":"autosql_role_app"}`)}
	namespace.ID = schema.StableID(namespace.Kind, namespace.Name)
	table := schema.Resource{Kind: schema.KindTable, Name: schema.Name{Schema: namespace.Name.Name, Name: "items", Parent: namespace.ID}, Dependencies: []schema.Dependency{{Target: namespace.ID, Type: schema.DependencyContains}, {Target: app.ID, Type: schema.DependencyOwns}}, Spec: json.RawMessage(`{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false,"owner":"autosql_role_app"}`)}
	table.ID = schema.StableID(table.Kind, table.Name)
	owned := actual
	owned.Graph.Resources = append(append([]schema.Resource(nil), actual.Graph.Resources...), namespace, table)
	owned.Normalize()
	ownedPlan, err := plan.Build(ctx, postgres.New(), actual, owned, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, ownedPlan)
	ownedActual := inspectRoleOwnership(t, ctx, url, namespace.Name.Name, "autosql_role_owner", "autosql_role_app")
	assertFingerprint(t, ownedActual, owned)
	transferred := ownedActual
	transferred.Graph.Resources = append([]schema.Resource(nil), ownedActual.Graph.Resources...)
	for index := range transferred.Graph.Resources {
		resource := &transferred.Graph.Resources[index]
		resource.Dependencies = append([]schema.Dependency(nil), resource.Dependencies...)
		if resource.Kind != schema.KindSchema && resource.Kind != schema.KindTable {
			continue
		}
		var specification map[string]any
		_ = json.Unmarshal(resource.Spec, &specification)
		specification["owner"] = owner.Name.Name
		resource.Spec, _ = json.Marshal(specification)
		for dependencyIndex := range resource.Dependencies {
			if resource.Dependencies[dependencyIndex].Type == schema.DependencyOwns {
				resource.Dependencies[dependencyIndex].Target = owner.ID
			}
		}
	}
	transferred.Normalize()
	transferPlan, err := plan.Build(ctx, postgres.New(), ownedActual, transferred, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, transferPlan)
	ownedActual = inspectRoleOwnership(t, ctx, url, namespace.Name.Name, "autosql_role_owner", "autosql_role_app")
	assertFingerprint(t, ownedActual, transferred)
	dropOwnedPlan, err := plan.Build(ctx, postgres.New(), ownedActual, actual, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, dropOwnedPlan)
	hcl, err := source.FormatHCL(actual)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := source.LoadContext(ctx, source.Input{URI: "roles.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, _ = postgres.New().Normalize(ctx, roundTrip)
	assertFingerprint(t, roundTrip, actual)

	ref, _ := secret.Parse("env://AUTOSQL_ROLE_TEST_PASSWORD")
	resolver := secret.NewResolver()
	resolver.Getenv = func(name string) (string, bool) { return "runtime-only-password", name == "AUTOSQL_ROLE_TEST_PASSWORD" }
	if err = postgres.ApplyRolePasswordChange(ctx, conn, resolver, postgres.RolePasswordChange{Role: app.Name.Name, Reference: ref}); err != nil {
		t.Fatal(err)
	}
	var passwordStored bool
	if err = conn.QueryRow(ctx, `select rolpassword is not null from pg_authid where rolname=$1`, app.Name.Name).Scan(&passwordStored); err != nil || !passwordStored {
		t.Fatalf("password write did not reach PostgreSQL: stored=%t err=%v", passwordStored, err)
	}
	actual = inspectRolesNamed(t, ctx, url, "autosql_role_owner", "autosql_role_app")
	assertFingerprint(t, actual, desired) // unreadable password never creates drift

	changed := actual
	changed.Graph.Resources = append([]schema.Resource(nil), actual.Graph.Resources...)
	for index := range changed.Graph.Resources {
		if changed.Graph.Resources[index].Name.Name == app.Name.Name {
			var roleSpec map[string]any
			_ = json.Unmarshal(changed.Graph.Resources[index].Spec, &roleSpec)
			roleSpec["connection_limit"] = float64(8)
			roleSpec["configuration"] = []any{"search_path=public", "statement_timeout=10s"}
			changed.Graph.Resources[index].Spec, _ = json.Marshal(roleSpec)
		}
	}
	changed.Normalize()
	alterPlan, err := plan.Build(ctx, postgres.New(), actual, changed, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, alterPlan)
	actual = inspectRolesNamed(t, ctx, url, "autosql_role_owner", "autosql_role_app")
	assertFingerprint(t, actual, changed)

	renamed := actual
	renamed.Graph.Resources = append([]schema.Resource(nil), actual.Graph.Resources...)
	var oldID, newID string
	for index := range renamed.Graph.Resources {
		if renamed.Graph.Resources[index].Name.Name == app.Name.Name {
			oldID = renamed.Graph.Resources[index].ID
			renamed.Graph.Resources[index].Name.Name = "autosql_role_app_v2"
			renamed.Graph.Resources[index].ID = schema.StableID(schema.KindRole, renamed.Graph.Resources[index].Name)
			newID = renamed.Graph.Resources[index].ID
		}
	}
	renamed.Normalize()
	renamePlan, err := plan.Build(ctx, postgres.New(), actual, renamed, plan.Options{Diff: schema.DiffOptions{RenameHints: []schema.RenameHint{{From: oldID, To: newID}}}})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, renamePlan)
	actual = inspectRolesNamed(t, ctx, url, "autosql_role_owner", "autosql_role_app_v2")
	assertFingerprint(t, actual, renamed)

	retired := actual
	retired.Graph.Resources = nil
	for _, resource := range actual.Graph.Resources {
		if resource.Name.Name != "autosql_role_app_v2" {
			retired.Graph.Resources = append(retired.Graph.Resources, resource)
		}
	}
	dropPlan, err := plan.Build(ctx, postgres.New(), actual, retired, plan.Options{Render: map[string]string{"allow_role_drop": "true", "reassign_owned_to.autosql_role_app_v2": "autosql_role_owner"}})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, dropPlan)
	actual = inspectRolesNamed(t, ctx, url, "autosql_role_owner")
	assertFingerprint(t, actual, retired)
}

func TestMembershipLifecycleVersionParity(t *testing.T) {
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
	defer func() {
		_, _ = conn.Exec(context.Background(), `drop role if exists autosql_member_v2; drop role if exists autosql_member; drop role if exists autosql_parent`)
	}()
	if _, err = conn.Exec(ctx, `drop role if exists autosql_member_v2; drop role if exists autosql_member; drop role if exists autosql_parent`); err != nil {
		t.Fatal(err)
	}
	var version int
	if err = conn.QueryRow(ctx, `select current_setting('server_version_num')::integer`).Scan(&version); err != nil {
		t.Fatal(err)
	}
	major := strconv.Itoa(version / 10000)
	options := map[string]string{"postgres_version": major}
	parent := integrationRole("autosql_parent", false, []string{})
	member := integrationRole("autosql_member", false, []string{})
	specification := map[string]any{"parent": parent.Name.Name, "member": member.Name.Name, "grantor": "postgres", "admin": false}
	if version >= 160000 {
		specification["inherit"], specification["set"] = false, true
	}
	raw, _ := json.Marshal(specification)
	membership := schema.Resource{Kind: schema.KindMembership, Name: schema.Name{Name: "autosql_member->autosql_parent@postgres"}, Dependencies: []schema.Dependency{{Target: parent.ID, Type: schema.DependencyReferences}, {Target: member.ID, Type: schema.DependencyReferences}}, Spec: raw}
	membership.ID = schema.StableID(membership.Kind, membership.Name)
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}, Annotations: map[string]string{"dialect": "postgresql"}}
	desired := empty
	desired.Graph.Resources = []schema.Resource{parent, member, membership}
	desired.Normalize()
	fresh, err := plan.Build(ctx, postgres.New(), empty, desired, plan.Options{Render: options})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, fresh)
	actual := inspectMembershipNamed(t, ctx, url, "autosql_parent", "autosql_member")
	assertFingerprint(t, actual, desired)
	hcl, err := source.FormatHCL(actual)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := source.LoadContext(ctx, source.Input{URI: "membership.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, _ = postgres.New().Normalize(ctx, roundTrip)
	assertFingerprint(t, roundTrip, actual)

	changed := cloneSchemaDocument(t, actual)
	for index := range changed.Graph.Resources {
		if changed.Graph.Resources[index].Kind != schema.KindMembership {
			continue
		}
		var values map[string]any
		_ = json.Unmarshal(changed.Graph.Resources[index].Spec, &values)
		values["admin"] = true
		if version >= 160000 {
			values["inherit"] = true
		}
		changed.Graph.Resources[index].Spec, _ = json.Marshal(values)
	}
	alterOptions := map[string]string{"postgres_version": major, "allow_membership_admin": "true"}
	alterPlan, err := plan.Build(ctx, postgres.New(), actual, changed, plan.Options{Render: alterOptions})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, alterPlan)
	actual = inspectMembershipNamed(t, ctx, url, "autosql_parent", "autosql_member")
	assertFingerprint(t, actual, changed)

	renamed := cloneSchemaDocument(t, actual)
	var oldRoleID, newRoleID, oldMembershipID, newMembershipID string
	for index := range renamed.Graph.Resources {
		resource := &renamed.Graph.Resources[index]
		if resource.Kind == schema.KindRole && resource.Name.Name == "autosql_member" {
			oldRoleID = resource.ID
			resource.Name.Name = "autosql_member_v2"
			resource.ID = schema.StableID(resource.Kind, resource.Name)
			newRoleID = resource.ID
		}
	}
	var parentRoleID string
	for _, resource := range renamed.Graph.Resources {
		if resource.Kind == schema.KindRole && resource.Name.Name == "autosql_parent" {
			parentRoleID = resource.ID
		}
	}
	for index := range renamed.Graph.Resources {
		resource := &renamed.Graph.Resources[index]
		if resource.Kind != schema.KindMembership {
			continue
		}
		oldMembershipID = resource.ID
		resource.Name.Name = "autosql_member_v2->autosql_parent@postgres"
		resource.ID = schema.StableID(resource.Kind, resource.Name)
		newMembershipID = resource.ID
		var values map[string]any
		_ = json.Unmarshal(resource.Spec, &values)
		values["member"] = "autosql_member_v2"
		resource.Spec, _ = json.Marshal(values)
		resource.Dependencies = []schema.Dependency{{Target: parentRoleID, Type: schema.DependencyReferences}, {Target: newRoleID, Type: schema.DependencyReferences}}
	}
	renamed.Normalize()
	renamePlan, err := plan.Build(ctx, postgres.New(), actual, renamed, plan.Options{Render: alterOptions, Diff: schema.DiffOptions{RenameHints: []schema.RenameHint{{From: oldRoleID, To: newRoleID}, {From: oldMembershipID, To: newMembershipID}}}})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, renamePlan)
	actual = inspectMembershipNamed(t, ctx, url, "autosql_parent", "autosql_member_v2")
	assertFingerprint(t, actual, renamed)

	revoked := cloneSchemaDocument(t, actual)
	revoked.Graph.Resources = revoked.Graph.Resources[:0]
	for _, resource := range actual.Graph.Resources {
		if resource.Kind != schema.KindMembership {
			revoked.Graph.Resources = append(revoked.Graph.Resources, resource)
		}
	}
	revoked.Normalize()
	revokePlan, err := plan.Build(ctx, postgres.New(), actual, revoked, plan.Options{Render: alterOptions})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, revokePlan)
	actual = inspectMembershipNamed(t, ctx, url, "autosql_parent", "autosql_member_v2")
	assertFingerprint(t, actual, revoked)
}

func TestGrantInventoryLifecycleParity(t *testing.T) {
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
	defer func() {
		_, _ = conn.Exec(context.Background(), `drop schema if exists autosql_grants cascade; drop role if exists autosql_grants_app`)
	}()
	if _, err = conn.Exec(ctx, `drop schema if exists autosql_grants cascade; drop role if exists autosql_grants_app`); err != nil {
		t.Fatal(err)
	}
	role := integrationRole("autosql_grants_app", false, []string{})
	ns := schema.Resource{Kind: schema.KindSchema, Name: schema.Name{Name: "autosql_grants"}, Spec: json.RawMessage(`{}`)}
	ns.ID = schema.StableID(ns.Kind, ns.Name)
	table := schema.Resource{Kind: schema.KindTable, Name: schema.Name{Schema: ns.Name.Name, Name: "items", Parent: ns.ID}, Dependencies: []schema.Dependency{{Target: ns.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(`{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`)}
	table.ID = schema.StableID(table.Kind, table.Name)
	grants := []schema.Resource{}
	for _, fixture := range []struct {
		target    schema.Resource
		privilege string
	}{{ns, "USAGE"}, {table, "SELECT"}, {table, "INSERT"}} {
		name := role.Name.Name + ":" + strings.ToLower(fixture.privilege) + ":postgres"
		grant := schema.Resource{Kind: schema.KindGrant, Name: schema.Name{Schema: fixture.target.Name.Schema, Name: name, Parent: fixture.target.ID}, Dependencies: []schema.Dependency{{Target: fixture.target.ID, Type: schema.DependencyReferences}, {Target: role.ID, Type: schema.DependencyReferences}}, Spec: json.RawMessage(fmt.Sprintf(`{"grantor":"postgres","grantee":%q,"privilege":%q,"grantable":false}`, role.Name.Name, fixture.privilege))}
		grant.ID = schema.StableID(grant.Kind, grant.Name)
		grants = append(grants, grant)
	}
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}, Annotations: map[string]string{"dialect": "postgresql"}}
	desired := empty
	desired.Graph.Resources = append([]schema.Resource{role, ns, table}, grants...)
	desired.Normalize()
	fresh, err := plan.Build(ctx, postgres.New(), empty, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, fresh)
	actual := inspectGrantInventory(t, ctx, url, ns.Name.Name, role.Name.Name)
	assertFingerprint(t, actual, desired)
	hcl, err := source.FormatHCL(actual)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := source.LoadContext(ctx, source.Input{URI: "grants.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, _ = postgres.New().Normalize(ctx, roundTrip)
	assertFingerprint(t, roundTrip, actual)

	grantable := cloneSchemaDocument(t, actual)
	for index := range grantable.Graph.Resources {
		if grantable.Graph.Resources[index].Kind == schema.KindGrant && strings.Contains(grantable.Graph.Resources[index].Name.Name, ":select:") {
			var values map[string]any
			_ = json.Unmarshal(grantable.Graph.Resources[index].Spec, &values)
			values["grantable"] = true
			grantable.Graph.Resources[index].Spec, _ = json.Marshal(values)
		}
	}
	grantPlan, err := plan.Build(ctx, postgres.New(), actual, grantable, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, grantPlan)
	actual = inspectGrantInventory(t, ctx, url, ns.Name.Name, role.Name.Name)
	assertFingerprint(t, actual, grantable)
	partial := cloneSchemaDocument(t, actual)
	partial.Graph.Resources = partial.Graph.Resources[:0]
	for _, resource := range actual.Graph.Resources {
		if resource.Kind != schema.KindGrant || !strings.Contains(resource.Name.Name, ":insert:") {
			partial.Graph.Resources = append(partial.Graph.Resources, resource)
		}
	}
	partial.Normalize()
	partialPlan, err := plan.Build(ctx, postgres.New(), actual, partial, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, partialPlan)
	actual = inspectGrantInventory(t, ctx, url, ns.Name.Name, role.Name.Name)
	assertFingerprint(t, actual, partial)
	revoked := cloneSchemaDocument(t, actual)
	revoked.Graph.Resources = revoked.Graph.Resources[:0]
	for _, resource := range actual.Graph.Resources {
		if resource.Kind != schema.KindGrant {
			revoked.Graph.Resources = append(revoked.Graph.Resources, resource)
		}
	}
	revoked.Normalize()
	revokePlan, err := plan.Build(ctx, postgres.New(), actual, revoked, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, revokePlan)
	actual = inspectGrantInventory(t, ctx, url, ns.Name.Name, role.Name.Name)
	assertFingerprint(t, actual, revoked)
}

func TestDefaultPrivilegeFutureObjectParity(t *testing.T) {
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
	defer func() {
		_, _ = conn.Exec(context.Background(), `drop schema if exists autosql_defaults cascade; drop role if exists autosql_defaults_app`)
	}()
	if _, err = conn.Exec(ctx, `drop schema if exists autosql_defaults cascade; drop role if exists autosql_defaults_app`); err != nil {
		t.Fatal(err)
	}
	role := integrationRole("autosql_defaults_app", false, []string{})
	ns := schema.Resource{Kind: schema.KindSchema, Name: schema.Name{Name: "autosql_defaults"}, Spec: json.RawMessage(`{}`)}
	ns.ID = schema.StableID(ns.Kind, ns.Name)
	defaultACL := schema.Resource{Kind: schema.KindDefaultPrivilege, Name: schema.Name{Name: "postgres:autosql_defaults:r:autosql_defaults_app:select"}, Dependencies: []schema.Dependency{{Target: role.ID, Type: schema.DependencyReferences}, {Target: ns.ID, Type: schema.DependencyReferences}}, Spec: json.RawMessage(`{"owner":"postgres","object_type":"r","schema":"autosql_defaults","grantee":"autosql_defaults_app","privilege":"SELECT","grantable":false}`)}
	defaultACL.ID = schema.StableID(defaultACL.Kind, defaultACL.Name)
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}, Annotations: map[string]string{"dialect": "postgresql"}}
	desired := empty
	desired.Graph.Resources = []schema.Resource{role, ns, defaultACL}
	desired.Normalize()
	fresh, err := plan.Build(ctx, postgres.New(), empty, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, fresh)
	if _, err = conn.Exec(ctx, `create table autosql_defaults.future_one(id bigint)`); err != nil {
		t.Fatal(err)
	}
	var allowed bool
	if err = conn.QueryRow(ctx, `select has_table_privilege('autosql_defaults_app','autosql_defaults.future_one','SELECT')`).Scan(&allowed); err != nil || !allowed {
		t.Fatalf("future table default privilege allowed=%t err=%v", allowed, err)
	}
	actual := inspectDefaultPrivilegeInventory(t, ctx, url, ns.Name.Name, role.Name.Name)
	assertFingerprint(t, actual, desired)
	hcl, err := source.FormatHCL(actual)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := source.LoadContext(ctx, source.Input{URI: "defaults.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, _ = postgres.New().Normalize(ctx, roundTrip)
	assertFingerprint(t, roundTrip, actual)
	grantable := cloneSchemaDocument(t, actual)
	for index := range grantable.Graph.Resources {
		if grantable.Graph.Resources[index].Kind == schema.KindDefaultPrivilege {
			var values map[string]any
			_ = json.Unmarshal(grantable.Graph.Resources[index].Spec, &values)
			values["grantable"] = true
			grantable.Graph.Resources[index].Spec, _ = json.Marshal(values)
		}
	}
	changePlan, err := plan.Build(ctx, postgres.New(), actual, grantable, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, changePlan)
	actual = inspectDefaultPrivilegeInventory(t, ctx, url, ns.Name.Name, role.Name.Name)
	assertFingerprint(t, actual, grantable)
	revoked := cloneSchemaDocument(t, actual)
	revoked.Graph.Resources = revoked.Graph.Resources[:0]
	for _, resource := range actual.Graph.Resources {
		if resource.Kind != schema.KindDefaultPrivilege {
			revoked.Graph.Resources = append(revoked.Graph.Resources, resource)
		}
	}
	revoked.Normalize()
	revokePlan, err := plan.Build(ctx, postgres.New(), actual, revoked, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	applyTestPlan(t, ctx, conn, revokePlan)
	if _, err = conn.Exec(ctx, `create table autosql_defaults.future_two(id bigint)`); err != nil {
		t.Fatal(err)
	}
	if err = conn.QueryRow(ctx, `select has_table_privilege('autosql_defaults_app','autosql_defaults.future_two','SELECT')`).Scan(&allowed); err != nil || allowed {
		t.Fatalf("revoked future default allowed=%t err=%v", allowed, err)
	}
	actual = inspectDefaultPrivilegeInventory(t, ctx, url, ns.Name.Name, role.Name.Name)
	assertFingerprint(t, actual, revoked)
}

func inspectDefaultPrivilegeInventory(t *testing.T, ctx context.Context, url, namespace, role string) schema.Document {
	t.Helper()
	document, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}, Advanced: true})
	if err != nil {
		t.Fatal(err)
	}
	document, err = postgres.New().Normalize(ctx, document)
	if err != nil {
		t.Fatal(err)
	}
	keep := map[string]bool{}
	for _, resource := range document.Graph.Resources {
		if resource.Kind == schema.KindRole && resource.Name.Name == role || resource.Kind == schema.KindSchema && resource.Name.Name == namespace || resource.Kind == schema.KindDefaultPrivilege && stringValueTest(resource.Spec, "grantee") == role {
			keep[resource.ID] = true
		}
	}
	filtered := document
	filtered.Graph.Resources = nil
	for _, resource := range document.Graph.Resources {
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
		if resource.Kind == schema.KindSchema {
			var values map[string]any
			_ = json.Unmarshal(resource.Spec, &values)
			delete(values, "owner")
			resource.Spec, _ = json.Marshal(values)
		}
		filtered.Graph.Resources = append(filtered.Graph.Resources, resource)
	}
	filtered.Normalize()
	return filtered
}

func inspectGrantInventory(t *testing.T, ctx context.Context, url, namespace, role string) schema.Document {
	t.Helper()
	document, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}, Advanced: true})
	if err != nil {
		t.Fatal(err)
	}
	document, err = postgres.New().Normalize(ctx, document)
	if err != nil {
		t.Fatal(err)
	}
	keep := map[string]bool{}
	for _, resource := range document.Graph.Resources {
		if resource.Kind == schema.KindRole && resource.Name.Name == role || resource.Kind == schema.KindSchema && resource.Name.Name == namespace || resource.Kind == schema.KindTable && resource.Name.Schema == namespace || resource.Kind == schema.KindGrant && stringValueTest(resource.Spec, "grantee") == role {
			keep[resource.ID] = true
		}
	}
	filtered := document
	filtered.Graph.Resources = nil
	for _, resource := range document.Graph.Resources {
		if !keep[resource.ID] {
			continue
		}
		dependencies := resource.Dependencies[:0]
		for _, dependency := range resource.Dependencies {
			if keep[dependency.Target] && dependency.Type != schema.DependencyOwns {
				dependencies = append(dependencies, dependency)
			}
		}
		resource.Dependencies = dependencies
		if resource.Kind != schema.KindRole && resource.Kind != schema.KindGrant {
			var values map[string]any
			_ = json.Unmarshal(resource.Spec, &values)
			delete(values, "owner")
			resource.Spec, _ = json.Marshal(values)
		}
		filtered.Graph.Resources = append(filtered.Graph.Resources, resource)
	}
	filtered.Normalize()
	return filtered
}

func stringValueTest(raw json.RawMessage, key string) string {
	var values map[string]any
	_ = json.Unmarshal(raw, &values)
	value, _ := values[key].(string)
	return value
}

func inspectMembershipNamed(t *testing.T, ctx context.Context, url string, roleNames ...string) schema.Document {
	t.Helper()
	document, err := postgres.InspectURL(ctx, url, postgres.Options{Advanced: true})
	if err != nil {
		t.Fatal(err)
	}
	document, err = postgres.New().Normalize(ctx, document)
	if err != nil {
		t.Fatal(err)
	}
	wanted := map[string]bool{}
	for _, name := range roleNames {
		wanted[name] = true
	}
	roleIDs := map[string]bool{}
	for _, resource := range document.Graph.Resources {
		if resource.Kind == schema.KindRole && wanted[resource.Name.Name] {
			roleIDs[resource.ID] = true
		}
	}
	filtered := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}, Annotations: map[string]string{"dialect": "postgresql"}}
	for _, resource := range document.Graph.Resources {
		if resource.Kind == schema.KindRole && roleIDs[resource.ID] {
			filtered.Graph.Resources = append(filtered.Graph.Resources, resource)
			continue
		}
		if resource.Kind == schema.KindMembership {
			all := true
			for _, dependency := range resource.Dependencies {
				all = all && roleIDs[dependency.Target]
			}
			if all {
				filtered.Graph.Resources = append(filtered.Graph.Resources, resource)
			}
		}
	}
	filtered.Normalize()
	return filtered
}

func integrationRole(name string, login bool, configuration []string) schema.Resource {
	specification, _ := json.Marshal(map[string]any{"superuser": false, "inherit": true, "create_role": false, "create_database": false, "login": login, "replication": false, "bypass_rls": false, "connection_limit": -1, "configuration": configuration})
	resource := schema.Resource{Kind: schema.KindRole, Name: schema.Name{Name: name}, Spec: specification}
	resource.ID = schema.StableID(resource.Kind, resource.Name)
	return resource
}

func inspectRolesNamed(t *testing.T, ctx context.Context, url string, names ...string) schema.Document {
	t.Helper()
	document, err := postgres.InspectURL(ctx, url, postgres.Options{Advanced: true})
	if err != nil {
		t.Fatal(err)
	}
	document, err = postgres.New().Normalize(ctx, document)
	if err != nil {
		t.Fatal(err)
	}
	wanted := map[string]bool{}
	for _, name := range names {
		wanted[name] = true
	}
	filtered := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}, Annotations: map[string]string{"dialect": "postgresql"}}
	for _, resource := range document.Graph.Resources {
		if resource.Kind == schema.KindRole && wanted[resource.Name.Name] {
			filtered.Graph.Resources = append(filtered.Graph.Resources, resource)
		}
	}
	filtered.Normalize()
	return filtered
}

func inspectRoleOwnership(t *testing.T, ctx context.Context, url, namespace string, roles ...string) schema.Document {
	t.Helper()
	document, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}, Advanced: true})
	if err != nil {
		t.Fatal(err)
	}
	document, err = postgres.New().Normalize(ctx, document)
	if err != nil {
		t.Fatal(err)
	}
	wantedRoles := map[string]bool{}
	for _, role := range roles {
		wantedRoles[role] = true
	}
	keep := map[string]bool{}
	for _, resource := range document.Graph.Resources {
		if resource.Kind == schema.KindRole && wantedRoles[resource.Name.Name] || resource.Kind == schema.KindSchema && resource.Name.Name == namespace || resource.Name.Schema == namespace {
			if resource.Kind != schema.KindGrant && resource.Kind != schema.KindMembership && resource.Kind != schema.KindDefaultPrivilege {
				keep[resource.ID] = true
			}
		}
	}
	filtered := document
	filtered.Graph.Resources = nil
	for _, resource := range document.Graph.Resources {
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
		filtered.Graph.Resources = append(filtered.Graph.Resources, resource)
	}
	filtered.Normalize()
	return filtered
}

func replaceDocumentResource(document schema.Document, id string, replacement schema.Resource) schema.Document {
	out := document
	out.Graph.Resources = append([]schema.Resource(nil), document.Graph.Resources...)
	for index := range out.Graph.Resources {
		if out.Graph.Resources[index].ID == id {
			out.Graph.Resources[index] = replacement
		}
	}
	out.Normalize()
	return out
}

func inspectFilteredRLS(t *testing.T, ctx context.Context, url, namespace, role string) schema.Document {
	t.Helper()
	document, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}, Advanced: true})
	if err != nil {
		t.Fatal(err)
	}
	document, err = postgres.New().Normalize(ctx, document)
	if err != nil {
		t.Fatal(err)
	}
	return filterRLSInventory(document, namespace, role)
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

func planContainsTransactionMode(p plan.Plan, mode plan.TransactionMode) bool {
	for _, step := range p.Steps {
		if step.Kind == plan.StepExecutable && step.Transaction == mode {
			return true
		}
	}
	return false
}

func applyTestPlanPhased(t *testing.T, ctx context.Context, conn *pgx.Conn, p plan.Plan) {
	t.Helper()
	steps := map[string]plan.Step{}
	for _, step := range p.Steps {
		steps[step.ID] = step
	}
	for _, phase := range p.Phases {
		if phase.Transaction == plan.TransactionProhibited {
			for _, id := range phase.StepIDs {
				step := steps[id]
				if step.Kind != plan.StepTopology {
					if _, err := conn.Exec(ctx, step.SQL); err != nil {
						t.Fatalf("%s: %v", step.SQL, err)
					}
				}
			}
			continue
		}
		tx, err := conn.Begin(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, id := range phase.StepIDs {
			step := steps[id]
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
}
func assertFingerprint(t *testing.T, actual, desired schema.Document) {
	t.Helper()
	a, _ := schema.SemanticFingerprint(actual)
	d, _ := schema.SemanticFingerprint(desired)
	if a != d {
		t.Fatalf("fingerprint got=%s want=%s", a, d)
	}
}
