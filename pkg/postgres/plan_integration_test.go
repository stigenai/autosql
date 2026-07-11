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
	desired.Normalize()
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
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, z, a}}}
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
	for index, fixture := range []struct{ name, typ, target string }{{"statuses", `"AutoSQL_UDT".status[][]`, "status"}, {"moods", `"AutoSQL_UDT"."Mood"[][]`, "Mood"}, {"matrix", "integer[][]", ""}} {
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
	desired.Graph.Resources = append([]schema.Resource(nil), current.Graph.Resources...)
	var oldID, newID string
	for i := range desired.Graph.Resources {
		r := &desired.Graph.Resources[i]
		if r.Kind == schema.KindColumn && r.Name.Name == "a" {
			oldID = r.ID
			r.Name.Name = "x"
			r.ID = schema.StableID(r.Kind, r.Name)
			newID = r.ID
		}
	}
	desired.Normalize()
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
