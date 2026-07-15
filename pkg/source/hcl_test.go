package source

import (
	"context"
	"errors"
	"testing"

	"autosql/pkg/schema"
)

const sampleHCL = `schema "public" {}
table "users" { schema = "public" }
role "app_reader" { managed = true }
resource "table" "orders" {
  schema = "public"
  spec_json = "{\"rls\":true}"
}
`

func TestParseHCLAndFormatRoundTrip(t *testing.T) {
	doc, err := ParseHCL("schema.hcl", []byte(sampleHCL), nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Graph.Resources) != 4 {
		t.Fatalf("resources=%d", len(doc.Graph.Resources))
	}
	formatted, err := FormatHCL(doc)
	if err != nil {
		t.Fatal(err)
	}
	back, err := ParseHCL("formatted.hcl", formatted, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(back.Graph.Resources) != len(doc.Graph.Resources) {
		t.Fatalf("round-trip resources=%d", len(back.Graph.Resources))
	}
	for _, resource := range back.Graph.Resources {
		if resource.Name.Name == "orders" && string(resource.Spec) != `{"rls":true}` {
			t.Fatalf("orders spec changed: %s", resource.Spec)
		}
	}
}

func TestHCLVariablesAndImports(t *testing.T) {
	data := []byte(`table "users" { schema = var.schema }`)
	doc, err := ParseHCL("vars.hcl", data, HCLVariables{"schema": "public"})
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, resource := range doc.Graph.Resources {
		if resource.Kind == schema.KindTable && resource.Name.Name == "users" {
			found = resource.Name.Schema == "public"
		}
	}
	if !found {
		t.Fatalf("variable schema was not applied: %+v", doc.Graph.Resources)
	}
	files := map[string][]byte{"/tmp/root.hcl": []byte(`import "child" { source = "child.hcl" }`), "/tmp/child.hcl": []byte(`table "child" { schema = "public" }`)}
	l := HCLLoader{ReadFile: func(p string) ([]byte, error) {
		b, ok := files[p]
		if !ok {
			return nil, errors.New("missing")
		}
		return b, nil
	}}
	loaded, err := l.Load(context.Background(), "/tmp/root.hcl")
	if err != nil {
		t.Fatal(err)
	}
	if len(loaded.Graph.Resources) != 2 {
		t.Fatalf("imported resources=%d", len(loaded.Graph.Resources))
	}
	files["/tmp/child.hcl"] = []byte(`import "root" { source = "root.hcl" }`)
	if _, err = l.Load(context.Background(), "/tmp/root.hcl"); !errors.Is(err, ErrImportCycle) {
		t.Fatalf("cycle=%v", err)
	}
}

func TestNestedHCLInheritsSchemaWithCanonicalContainment(t *testing.T) {
	doc, err := ParseHCL("nested.hcl", []byte(`schema "app" {
  table "users" {
    column "email" {
      type = "text"
      nullable = false
      ordinal = 1
    }
  }
}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var table, column schema.Resource
	for _, resource := range doc.Graph.Resources {
		switch resource.Kind {
		case schema.KindTable:
			table = resource
		case schema.KindColumn:
			column = resource
		}
	}
	if table.Name.Schema != "app" || column.Name.Schema != "app" {
		t.Fatalf("schema inheritance failed: table=%+v column=%+v", table.Name, column.Name)
	}
	if column.Name.Parent != table.ID {
		t.Fatalf("column parent=%q, want %q", column.Name.Parent, table.ID)
	}
	if len(column.Dependencies) != 1 || column.Dependencies[0].Target != table.ID {
		t.Fatalf("column dependencies are noncanonical: %+v", column.Dependencies)
	}
}

func TestHCLAuthoringHelpersProduceCanonicalIDsJSONAndDependencies(t *testing.T) {
	doc, err := ParseHCL("helpers.hcl", []byte(`schema "app" {}
table "users" {
  schema = "app"
  column "id" {
    type = "bigint"
    nullable = false
    ordinal = 1
  }
}
resource "primary_key" "users_pkey" {
  schema = "app"
  parent = table_id("app", "users")
  spec_json = jsonencode({ definition = "PRIMARY KEY (id)" })
  deps_json = jsonencode([
    contains(resource_id("table", "app", schema_id("app"), "users")),
    references(column_id("app", "users", "id")),
    uses(column_id("app", "users", "id")),
    owns(column_id("app", "users", "id")),
  ])
}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var primary schema.Resource
	for _, resource := range doc.Graph.Resources {
		if resource.Kind == schema.KindPrimaryKey {
			primary = resource
		}
	}
	wantTable := schema.StableID(schema.KindTable, schema.Name{Schema: "app", Parent: schema.StableID(schema.KindSchema, schema.Name{Name: "app"}), Name: "users"})
	wantColumn := schema.StableID(schema.KindColumn, schema.Name{Schema: "app", Parent: wantTable, Name: "id"})
	if primary.Name.Parent != wantTable || string(primary.Spec) != `{"definition":"PRIMARY KEY (id)"}` {
		t.Fatalf("helper result is not canonical: %+v spec=%s", primary.Name, primary.Spec)
	}
	want := map[schema.DependencyType]string{
		schema.DependencyContains:   wantTable,
		schema.DependencyReferences: wantColumn,
		schema.DependencyUses:       wantColumn,
		schema.DependencyOwns:       wantColumn,
	}
	for _, dependency := range primary.Dependencies {
		if want[dependency.Type] != dependency.Target {
			t.Fatalf("dependency=%+v want=%+v", dependency, want)
		}
		delete(want, dependency.Type)
	}
	if len(want) != 0 {
		t.Fatalf("missing helper dependencies: %+v", want)
	}
}
