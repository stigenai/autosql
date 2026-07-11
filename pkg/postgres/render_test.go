package postgres

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
)

func renderResource(kind schema.Kind, name schema.Name, spec string, deps ...schema.Dependency) schema.Resource {
	r := schema.Resource{Kind: kind, Name: name, Spec: json.RawMessage(spec), Dependencies: deps}
	r.ID = schema.StableID(kind, name)
	return r
}

func TestRenderDocumentQuotesAndOrders(t *testing.T) {
	s := renderResource(schema.KindSchema, schema.Name{Name: `Odd"Schema`}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: s.Name.Name, Name: "select", Parent: s.ID}, `{}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	column := renderResource(schema.KindColumn, schema.Name{Schema: s.Name.Name, Name: `a"b`, Parent: table.ID}, `{"type":"character varying(8)","default":"'x'","not_null":true}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{column, table, s}}}
	out, err := RenderDocument(context.Background(), doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`CREATE SCHEMA "Odd""Schema";`, `CREATE TABLE "Odd""Schema"."select" ();`, `ALTER TABLE "Odd""Schema"."select" ADD COLUMN "a""b" character varying(8) DEFAULT 'x' NOT NULL;`}
	if len(out) != len(want) {
		t.Fatalf("out=%+v", out)
	}
	for i := range want {
		if out[i].SQL != want[i] {
			t.Errorf("%d got %q want %q", i, out[i].SQL, want[i])
		}
	}
}

func TestRenderDocumentGolden(t *testing.T) {
	s := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "widgets", Parent: s.ID}, `{}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	column := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "id", Parent: table.ID}, `{"type":"bigint","not_null":true}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{column, table, s}}}
	out, err := RenderDocument(context.Background(), doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	lines := make([]string, len(out))
	for i := range out {
		lines[i] = out[i].SQL
	}
	actual := strings.Join(lines, "\n") + "\n"
	expected, err := os.ReadFile("testdata/render_document.golden.sql")
	if err != nil {
		t.Fatal(err)
	}
	if actual != string(expected) {
		t.Fatalf("golden mismatch\nactual:\n%s\nexpected:\n%s", actual, expected)
	}
}

func TestConcurrentIndexIsTransactionProhibited(t *testing.T) {
	table := renderResource(schema.KindTable, schema.Name{Schema: "public", Name: "users"}, `{}`)
	idx := renderResource(schema.KindIndex, schema.Name{Schema: "public", Name: "users_email_idx", Parent: table.ID}, `{"definition":"(email)"}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{table, idx}}}
	changes, _ := schema.Diff(empty, desired, schema.DiffOptions{})
	out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: empty, Desired: desired, Options: map[string]string{"concurrent_indexes": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if out[len(out)-1].Transactional || !strings.Contains(out[len(out)-1].SQL, "CONCURRENTLY") {
		t.Fatalf("out=%+v", out)
	}
}

func TestIndexAlterRequiresExplicitRebuild(t *testing.T) {
	table := renderResource(schema.KindTable, schema.Name{Schema: "public", Name: "users"}, `{}`)
	before := renderResource(schema.KindIndex, schema.Name{Schema: "public", Name: "users_idx", Parent: table.ID}, `{"definition":"(id)"}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	after := before
	after.Spec = json.RawMessage(`{"definition":"(email)"}`)
	current := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{table, before}}}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{table, after}}}
	changes, _ := schema.Diff(current, desired, schema.DiffOptions{})
	out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired})
	if err == nil || len(out) != 0 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	out, err = New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired, Options: map[string]string{"allow_rebuild": "true"}})
	if err != nil || len(out) != 2 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestEveryManagedKindHasSafeCreateRendering(t *testing.T) {
	s := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "users", Parent: s.ID}, `{}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	resources := map[string]schema.Resource{s.ID: s, table.ID: table}
	cases := []schema.Resource{
		s,
		renderResource(schema.KindExtension, schema.Name{Schema: "app", Name: "hstore", Parent: s.ID}, `{"version":"1.8"}`),
		renderResource(schema.KindEnum, schema.Name{Schema: "app", Name: "status", Parent: s.ID}, `{"values":["new"]}`),
		renderResource(schema.KindDomain, schema.Name{Schema: "app", Name: "positive", Parent: s.ID}, `{"base_type":"integer","not_null":true}`),
		renderResource(schema.KindComposite, schema.Name{Schema: "app", Name: "address", Parent: s.ID}, `{"attributes":[{"name":"zip","type":"integer"}]}`),
		renderResource(schema.KindSequence, schema.Name{Schema: "app", Name: "seq", Parent: s.ID}, `{"start":1,"increment":1}`),
		table,
		renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "id", Parent: table.ID}, `{"type":"bigint"}`),
		renderResource(schema.KindPrimaryKey, schema.Name{Schema: "app", Name: "users_pkey", Parent: table.ID}, `{"definition":"PRIMARY KEY (id)"}`),
		renderResource(schema.KindUniqueConstraint, schema.Name{Schema: "app", Name: "users_id_key", Parent: table.ID}, `{"definition":"UNIQUE (id)"}`),
		renderResource(schema.KindCheckConstraint, schema.Name{Schema: "app", Name: "users_id_check", Parent: table.ID}, `{"definition":"CHECK (id > 0)"}`),
		renderResource(schema.KindForeignKey, schema.Name{Schema: "app", Name: "users_parent_fkey", Parent: table.ID}, `{"definition":"FOREIGN KEY (id) REFERENCES app.users(id)"}`),
		renderResource(schema.KindIndex, schema.Name{Schema: "app", Name: "users_idx", Parent: table.ID}, `{"definition":"(id)"}`),
		renderResource(schema.KindView, schema.Name{Schema: "app", Name: "user_view", Parent: s.ID}, `{"definition":"SELECT 1"}`),
		renderResource(schema.KindMaterializedView, schema.Name{Schema: "app", Name: "user_mv", Parent: s.ID}, `{"definition":"SELECT 1"}`),
	}
	for _, r := range cases {
		resources[r.ID] = r
	}
	for _, r := range cases {
		if _, err := renderCreate(r, resources, nil); err != nil {
			t.Errorf("create %s: %v", r.Kind, err)
		}
	}
}
