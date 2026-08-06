package source

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
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

func TestFormatHCLRoundTripsExactLargeIntegers(t *testing.T) {
	namespace := schema.Resource{Kind: schema.KindSchema, Name: schema.Name{Name: "app"}, Spec: []byte(`{}`)}
	namespace.ID = schema.StableID(namespace.Kind, namespace.Name)
	sequence := schema.Resource{Kind: schema.KindSequence, Name: schema.Name{Schema: "app", Name: "ids", Parent: namespace.ID}, Dependencies: []schema.Dependency{{Target: namespace.ID, Type: schema.DependencyContains}}, Spec: []byte(`{"start":1,"increment":1,"min":1,"max":9223372036854775807,"cache":1,"cycle":false}`)}
	sequence.ID = schema.StableID(sequence.Kind, sequence.Name)
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{namespace, sequence}}}
	formatted, err := FormatHCL(doc)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := ParseHCL("large-integer.hcl", formatted, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range reloaded.Graph.Resources {
		if resource.ID == sequence.ID {
			if !strings.Contains(string(resource.Spec), `"max":9223372036854775807`) || strings.Contains(string(resource.Spec), `9223372036854776000`) {
				t.Fatalf("large integer changed across HCL round-trip: got=%s", resource.Spec)
			}
			return
		}
	}
	t.Fatal("sequence missing after HCL round-trip")
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

func TestHCLModulesHaveExplicitInputsAndTypedOutputs(t *testing.T) {
	files := map[string][]byte{
		"/tmp/root.hcl": []byte(`
module "core" {
  source = "core.hcl"
  inputs = { schema_name = "app" }
}
table "audit" { schema = module.core.schema_name }
`),
		"/tmp/core.hcl": []byte(`
variable "schema_name" { type = string }
table "users" { schema = var.schema_name }
output "schema_name" {
  type  = string
  value = var.schema_name
}
`),
	}
	loader := HCLLoader{Variables: HCLVariables{"schema_name": "leaked"}, ReadFile: func(path string) ([]byte, error) {
		data, ok := files[path]
		if !ok {
			return nil, errors.New("missing")
		}
		return data, nil
	}}
	doc, err := loader.Load(context.Background(), "/tmp/root.hcl")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"users", "audit"} {
		found := false
		for _, resource := range doc.Graph.Resources {
			if resource.Kind == schema.KindTable && resource.Name.Name == name {
				found = resource.Name.Schema == "app"
			}
		}
		if !found {
			t.Fatalf("module output/input did not configure %s: %+v", name, doc.Graph.Resources)
		}
	}

	files["/tmp/root.hcl"] = []byte(`module "core" { source = "core.hcl" }`)
	if _, err := loader.Load(context.Background(), "/tmp/root.hcl"); err == nil || !strings.Contains(err.Error(), "schema_name is required") {
		t.Fatalf("root variable leaked into module: %v", err)
	}
	files["/tmp/root.hcl"] = []byte(`
module "core" {
  source = "core.hcl"
  inputs = { schema_name = "app", undeclared = true }
}
`)
	if _, err := loader.Load(context.Background(), "/tmp/root.hcl"); err == nil || !strings.Contains(err.Error(), "undeclared inputs: undeclared") {
		t.Fatalf("unknown module input accepted: %v", err)
	}
}

func TestHCLDirectoryCompositionIsDeterministicAndDuplicatesAreClear(t *testing.T) {
	directory := t.TempDir()
	moduleDir := filepath.Join(directory, "schema")
	if err := os.Mkdir(moduleDir, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(directory, "root.hcl"): `module "schema" {
  source = "schema"
  inputs = { schema_name = "app" }
}`,
		filepath.Join(moduleDir, "z.hcl"): `table "zebra" { schema = var.schema_name }`,
		filepath.Join(moduleDir, "a.hcl"): `variable "schema_name" { type = string }
table "accounts" { schema = var.schema_name }`,
	}
	for name, contents := range files {
		if err := os.WriteFile(name, []byte(contents), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	loader := HCLLoader{}
	first, err := loader.Load(context.Background(), filepath.Join(directory, "root.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	second, err := loader.Load(context.Background(), filepath.Join(directory, "root.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	one, _ := first.MarshalCanonical()
	two, _ := second.MarshalCanonical()
	if string(one) != string(two) {
		t.Fatal("directory composition changed normalized graph")
	}

	duplicateRoot := filepath.Join(directory, "duplicates.hcl")
	left, right := filepath.Join(directory, "left.hcl"), filepath.Join(directory, "right.hcl")
	if err := os.WriteFile(duplicateRoot, []byte(`import "left" { source = "left.hcl" }
import "right" { source = "right.hcl" }`), 0o600); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{left, right} {
		if err := os.WriteFile(name, []byte(`table "same" { schema = "app" }`), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := loader.Load(context.Background(), duplicateRoot); err == nil || !strings.Contains(err.Error(), "duplicate resource identity") || !strings.Contains(err.Error(), left) || !strings.Contains(err.Error(), right) {
		t.Fatalf("duplicate diagnostic=%v", err)
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

func TestHCLCompositeHelpersProduceOrderedAttributesAndTypedIDs(t *testing.T) {
	doc, err := ParseHCL("composite-helpers.hcl", []byte(`schema "app" {}
composite_type "profile" {
  schema = "app"
  dependencies = [uses(extension_id("app", "hstore"))]
  attributes = [
    composite_attribute("name", "text", 1),
    collated_composite_attribute("label", "text", 2, "pg_catalog.\"C\""),
  ]
}
extension "hstore" {
  schema = "app"
  version = "1.8"
}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range doc.Graph.Resources {
		switch resource.Kind {
		case schema.KindComposite:
			if resource.ID != schema.StableID(schema.KindComposite, resource.Name) || !strings.Contains(string(resource.Spec), `"ordinal":2`) || !strings.Contains(string(resource.Spec), `"collation"`) {
				t.Fatalf("composite helper result=%s id=%s", resource.Spec, resource.ID)
			}
			if len(resource.Dependencies) != 2 {
				t.Fatalf("composite helper dependencies=%+v", resource.Dependencies)
			}
		case schema.KindExtension:
			if resource.ID != schema.StableID(schema.KindExtension, resource.Name) {
				t.Fatalf("extension helper id=%s", resource.ID)
			}
		}
	}
}

func TestHCLSymbolicReferencesResolveIndependentOfDeclarationOrder(t *testing.T) {
	doc, err := ParseHCL("references.hcl", []byte(`
table "accounts" {
  schema = schema.app
  dependencies = [references(table.organizations)]
  owner = role.app_owner

  column "id" {
    type = "bigint"
  }
  column "organization_id" {
    type = "bigint"
  }
  column "status" {
    type = enum.account_status
  }
  index "accounts_organization_idx" {
    columns = [column.organization_id]
  }
}

enum "account_status" {
  schema = schema.app
  values = ["pending", "active"]
}

table "organizations" {
  schema = schema.app
  column "id" {
    type = "bigint"
  }
}

schema "app" {}
role "app_owner" {}
`), nil)
	if err != nil {
		t.Fatal(err)
	}

	resources := map[schema.Kind]map[string]schema.Resource{}
	for _, resource := range doc.Graph.Resources {
		if resources[resource.Kind] == nil {
			resources[resource.Kind] = map[string]schema.Resource{}
		}
		resources[resource.Kind][resource.Name.Name] = resource
	}
	accounts := resources[schema.KindTable]["accounts"]
	organizations := resources[schema.KindTable]["organizations"]
	owner := resources[schema.KindRole]["app_owner"]
	status := resources[schema.KindEnum]["account_status"]
	statusColumn := resources[schema.KindColumn]["status"]
	organizationColumn := resources[schema.KindColumn]["organization_id"]
	index := resources[schema.KindIndex]["accounts_organization_idx"]
	if accounts.ID == "" || organizations.ID == "" || status.ID == "" || index.ID == "" {
		t.Fatalf("missing symbolic resources: %+v", resources)
	}
	assertHCLDependency(t, accounts, organizations.ID, schema.DependencyReferences)
	assertHCLDependency(t, accounts, owner.ID, schema.DependencyOwns)
	assertHCLDependency(t, statusColumn, status.ID, schema.DependencyUses)
	assertHCLDependency(t, index, organizationColumn.ID, schema.DependencyReferences)
	if got := stringValueForHCLTest(statusColumn.Spec, "type"); got != "app.account_status" {
		t.Fatalf("symbolic enum type=%q", got)
	}
	var indexSpec map[string]any
	if err := json.Unmarshal(index.Spec, &indexSpec); err != nil {
		t.Fatal(err)
	}
	columns, _ := indexSpec["columns"].([]any)
	if len(columns) != 1 || columns[0] != organizationColumn.Name.Name {
		t.Fatalf("symbolic index columns=%v want=%s", columns, organizationColumn.Name.Name)
	}
}

func TestHCLSymbolicReferencesSupportQualifiedLookup(t *testing.T) {
	doc, err := ParseHCL("qualified.hcl", []byte(`schema "app" {}
schema "audit" {}
table "users" { schema = schema.app }
table "users" { schema = schema.audit }
role "reader" {
  object = table.app.users
}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var reader, appUsers schema.Resource
	for _, resource := range doc.Graph.Resources {
		if resource.Kind == schema.KindRole {
			reader = resource
		}
		if resource.Kind == schema.KindTable && resource.Name.Schema == "app" {
			appUsers = resource
		}
	}
	assertHCLDependency(t, reader, appUsers.ID, schema.DependencyReferences)
}

func TestHCLSymbolicReferencesFailClosed(t *testing.T) {
	tests := []struct {
		name string
		hcl  string
		want string
	}{
		{
			name: "missing",
			hcl: `schema "app" {}
table "accounts" {
  schema = schema.app
  dependencies = [references(table.missing)]
}`,
			want: "symbolic reference evaluation failed",
		},
		{
			name: "ambiguous",
			hcl: `schema "app" {}
schema "audit" {}
table "users" { schema = schema.app }
table "users" { schema = schema.audit }
role "reader" { object = table.users }`,
			want: "symbolic reference evaluation failed",
		},
		{
			name: "wrong kind",
			hcl: `schema "app" {}
table "users" {
  schema = schema.app
  index "bad" { columns = [table.users] }
}`,
			want: "columns must contain only column references",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			_, err := ParseHCL("fail-closed.hcl", []byte(test.hcl), nil)
			if err == nil || !strings.Contains(err.Error(), test.want) || !strings.Contains(err.Error(), "fail-closed.hcl") {
				t.Fatalf("error=%v want filename and %q", err, test.want)
			}
		})
	}
}

func TestHCLSymbolicReferencesResolveAcrossImports(t *testing.T) {
	files := map[string][]byte{
		"/tmp/root.hcl": []byte(`import "types" { source = "types.hcl" }
table "accounts" {
  schema = schema.app
  column "status" {
    type = enum.account_status
  }
}`),
		"/tmp/types.hcl": []byte(`schema "app" {}
enum "account_status" {
  schema = schema.app
  values = ["pending", "active"]
}`),
	}
	loader := HCLLoader{ReadFile: func(path string) ([]byte, error) {
		value, ok := files[path]
		if !ok {
			return nil, errors.New("missing fixture")
		}
		return value, nil
	}}
	doc, err := loader.Load(context.Background(), "/tmp/root.hcl")
	if err != nil {
		t.Fatal(err)
	}
	var enumResource, column schema.Resource
	for _, resource := range doc.Graph.Resources {
		switch resource.Kind {
		case schema.KindEnum:
			enumResource = resource
		case schema.KindColumn:
			column = resource
		}
	}
	assertHCLDependency(t, column, enumResource.ID, schema.DependencyUses)
	if column.Source == nil || column.Source.URI != "/tmp/root.hcl" || enumResource.Source == nil || enumResource.Source.URI != "/tmp/types.hcl" {
		t.Fatalf("cross-file provenance column=%+v enum=%+v", column.Source, enumResource.Source)
	}
}

func TestHCLSymbolicReferenceCyclesFailClosed(t *testing.T) {
	_, err := ParseHCL("cycle.hcl", []byte(`schema "app" {}
table "a" {
  schema = schema.app
  dependencies = [references(table.b)]
}
table "b" {
  schema = schema.app
  dependencies = [references(table.a)]
}`), nil)
	if err == nil || !strings.Contains(err.Error(), "dependency cycle") || !strings.Contains(err.Error(), "cycle.hcl") {
		t.Fatalf("cycle error=%v", err)
	}
}

func TestHCLTypedSQLExpressionConstructors(t *testing.T) {
	doc, err := ParseHCL("expressions.hcl", []byte(`schema "app" {}
enum "status" {
  schema = schema.app
  values = ["pending", "active"]
}
table "jobs" {
  schema = schema.app
  column "state" {
    type = enum.status
    default = enum_value(enum.status, "pending")
  }
  column "labels" {
    type = "text[]"
    default = cast(sql_array([literal("one"), literal("two")]), "text[]")
  }
  column "created_at" {
    type = "timestamptz"
    default = sql("now()")
  }
}`), nil)
	if err != nil {
		t.Fatal(err)
	}
	var status schema.Resource
	columns := map[string]schema.Resource{}
	for _, resource := range doc.Graph.Resources {
		if resource.Kind == schema.KindEnum {
			status = resource
		}
		if resource.Kind == schema.KindColumn {
			columns[resource.Name.Name] = resource
		}
	}
	if got := stringValueForHCLTest(columns["state"].Spec, "default"); got != "'pending'::app.status" {
		t.Fatalf("enum default=%q", got)
	}
	assertHCLDependency(t, columns["state"], status.ID, schema.DependencyUses)
	if got := stringValueForHCLTest(columns["labels"].Spec, "default"); got != "(ARRAY['one', 'two'])::text[]" {
		t.Fatalf("array default=%q", got)
	}
	if got := stringValueForHCLTest(columns["created_at"].Spec, "default"); got != "now()" {
		t.Fatalf("SQL default=%q", got)
	}
}

func TestHCLTypedSQLExpressionsRejectInvalidComposition(t *testing.T) {
	for name, input := range map[string]string{
		"empty SQL":      `schema "app" {} table "t" { schema = schema.app column "v" { type = "text" default = sql("") } }`,
		"raw cast input": `schema "app" {} table "t" { schema = schema.app column "v" { type = "text" default = cast("x", "text") } }`,
		"wrong enum ref": `schema "app" {} table "t" { schema = schema.app column "v" { type = "text" default = enum_value(table.t, "x") } }`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseHCL("invalid-expression.hcl", []byte(input), nil); err == nil || !strings.Contains(err.Error(), "invalid-expression.hcl") {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func assertHCLDependency(t *testing.T, resource schema.Resource, target string, dependencyType schema.DependencyType) {
	t.Helper()
	for _, dependency := range resource.Dependencies {
		if dependency.Target == target && dependency.Type == dependencyType {
			return
		}
	}
	t.Fatalf("resource %s missing %s dependency on %s: %+v", resource.ID, dependencyType, target, resource.Dependencies)
}

func stringValueForHCLTest(raw json.RawMessage, key string) string {
	var value map[string]any
	_ = json.Unmarshal(raw, &value)
	result, _ := value[key].(string)
	return result
}
