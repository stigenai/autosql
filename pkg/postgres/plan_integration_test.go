package postgres_test

import (
	"context"
	"encoding/json"
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
	column := schema.Resource{Kind: schema.KindColumn, Name: schema.Name{Schema: "autosql_native", Name: "label", Parent: table.ID}, Dependencies: []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(`{"type":"text","not_null":false,"ordinal":1}`)}
	column.ID = schema.StableID(column.Kind, column.Name)
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, column}}}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
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
