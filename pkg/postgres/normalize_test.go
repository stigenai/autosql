package postgres

import (
	"context"
	"encoding/json"
	"os"
	"reflect"
	"testing"

	"autosql/pkg/schema"
)

func TestPostgresSemanticDiffGolden(t *testing.T) {
	table := schema.Resource{Kind: schema.KindTable, Name: schema.Name{Schema: "public", Name: "accounts"}, Spec: json.RawMessage(`{"persistence":"p"}`)}
	table.ID = schema.StableID(table.Kind, table.Name)
	makeColumn := func(name, typ, def string) schema.Resource {
		r := schema.Resource{Kind: schema.KindColumn, Name: schema.Name{Schema: "public", Name: name, Parent: table.ID}, Dependencies: []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(`{"type":"` + typ + `","default":"` + def + `"}`)}
		r.ID = schema.StableID(r.Kind, r.Name)
		return r
	}
	current := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{makeColumn("created_at", "int4", "now()"), table}}}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{table, makeColumn("created_at", "integer", "CURRENT_TIMESTAMP"), makeColumn("email", "varchar", "NULL")}}}
	current, _ = New().Normalize(context.Background(), current)
	desired, _ = New().Normalize(context.Background(), desired)
	changes, err := schema.Diff(current, desired, schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := changes.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile("testdata/semantic_diff.golden.json")
	if err != nil {
		t.Fatalf("golden missing: %v\n%s", err, actual)
	}
	if string(actual) != string(expected) {
		t.Fatalf("golden mismatch\nactual: %s\nexpected: %s", actual, expected)
	}
}

func TestNormalizePostgresSemanticsAndPreservesUnknown(t *testing.T) {
	parent := schema.Resource{Kind: schema.KindTable, Name: schema.Name{Schema: "public", Name: "users"}, Spec: json.RawMessage(`{}`)}
	parent.ID = schema.StableID(parent.Kind, parent.Name)
	column := schema.Resource{Kind: schema.KindColumn, Name: schema.Name{Schema: "public", Name: "age", Parent: parent.ID}, Dependencies: []schema.Dependency{{Target: parent.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(`{"type":"pg_catalog.int4","default":"(('1'::integer))","future":{"opaque":"kept","type":"int4"}}`)}
	column.ID = schema.StableID(column.Kind, column.Name)
	pk := schema.Resource{Kind: schema.KindPrimaryKey, Name: schema.Name{Schema: "public", Name: "users_pkey", Parent: parent.ID}, Dependencies: []schema.Dependency{{Target: parent.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(`{"definition":"PRIMARY   KEY (id)"}`)}
	pk.ID = schema.StableID(pk.Kind, pk.Name)
	input := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{pk, column, parent}}}
	got, err := New().Normalize(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	again, err := New().Normalize(context.Background(), got)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(got, again) {
		t.Fatal("normalization is not idempotent")
	}
	var spec map[string]any
	for _, r := range got.Graph.Resources {
		if r.Kind == schema.KindColumn {
			_ = json.Unmarshal(r.Spec, &spec)
			if spec["type"] != "integer" || spec["default"] != "'1'" || spec["future"].(map[string]any)["opaque"] != "kept" || spec["future"].(map[string]any)["type"] != "int4" {
				t.Fatalf("spec=%#v", spec)
			}
		}
		if r.Kind == schema.KindPrimaryKey && r.Annotations["autosql.io/generated-name"] != "true" {
			t.Fatalf("generated name not marked: %#v", r)
		}
	}
}

func TestNormalizeAliasesAndStableDefaultsCompareEqual(t *testing.T) {
	makeDoc := func(typ, def, name string) schema.Document {
		r := schema.Resource{Kind: schema.KindColumn, Name: schema.Name{Schema: "public", Name: name}, Spec: json.RawMessage(`{"type":"` + typ + `","default":"` + def + `"}`)}
		r.ID = schema.StableID(r.Kind, r.Name)
		return schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{r}}}
	}
	a, _ := New().Normalize(context.Background(), makeDoc("int4", "now()", "created_at"))
	b, _ := New().Normalize(context.Background(), makeDoc("integer", "CURRENT_TIMESTAMP", "created_at"))
	changes, err := schema.Diff(a, b, schema.DiffOptions{})
	if err != nil || len(changes.Changes) != 0 {
		t.Fatalf("changes=%+v err=%v", changes, err)
	}
}
