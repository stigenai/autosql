package source

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"autosql/pkg/schema"
)

// TestFormatHCLReloadsViaLoadContext proves the bootstrap-to-HCL flow: a SQL
// source is loaded, emitted as HCL by FormatHCL, then reloaded through
// LoadContext's FormatHCLSource path. The resulting graph must be identical,
// so an operator/CI pipeline can adopt HCL as the source of truth with no drift.
func TestFormatHCLReloadsViaLoadContext(t *testing.T) {
	sql := `CREATE SCHEMA app;
CREATE TABLE app.users (
  id bigint NOT NULL,
  email text,
  CONSTRAINT users_pkey PRIMARY KEY (id)
);
CREATE INDEX users_email_idx ON app.users (email);`
	fromSQL, err := Load(Input{URI: "schema.sql", Format: FormatSQL, Data: []byte(sql)})
	if err != nil {
		t.Fatal(err)
	}
	hcl, err := FormatHCL(fromSQL)
	if err != nil {
		t.Fatal(err)
	}
	fromHCL, err := LoadContext(context.Background(), Input{URI: "schema.hcl", Format: FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatalf("reload via FormatHCLSource: %v", err)
	}
	diff, err := schema.Diff(fromSQL, fromHCL, schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Changes) != 0 {
		t.Fatalf("SQL->HCL->graph drifted: %d changes", len(diff.Changes))
	}
}

// TestFormatHCLPreservesAnnotations proves that resource annotations (e.g.
// column/table comments captured by schema inspect) survive FormatHCL ->
// ParseHCL. Without this, inspecting a commented database and re-applying its
// HCL would plan a diff for every comment.
func TestFormatHCLPreservesAnnotations(t *testing.T) {
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{
		manualResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`, nil),
	}}}
	table := manualResource(schema.KindTable, schema.Name{Schema: "app", Name: "users", Parent: doc.Graph.Resources[0].ID}, `{"options":""}`, []schema.Dependency{{Target: doc.Graph.Resources[0].ID, Type: schema.DependencyContains}})
	table.Annotations = map[string]string{"comment": "primary user table"}
	doc.Graph.Resources = append(doc.Graph.Resources, table)
	doc.Normalize()

	hcl, err := FormatHCL(doc)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadContext(context.Background(), Input{URI: "s.hcl", Format: FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	diff, err := schema.Diff(doc, reloaded, schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(diff.Changes) != 0 {
		t.Fatalf("annotations dropped on round-trip: %d changes", len(diff.Changes))
	}
}

// TestFormatHCLPreservesDocumentAnnotations proves document-level metadata (the
// dialect annotation stamped by schema inspect) survives FormatHCL -> LoadContext.
// Without it, plan.Build rejects an inspected-then-reloaded schema as an
// unsupported transition even though every resource matches.
func TestFormatHCLPreservesDocumentAnnotations(t *testing.T) {
	doc := schema.Document{
		Version:     schema.SchemaVersion,
		Annotations: map[string]string{"dialect": "postgresql"},
		Graph:       schema.Graph{Resources: []schema.Resource{manualResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`, nil)}},
	}
	doc.Normalize()
	hcl, err := FormatHCL(doc)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := LoadContext(context.Background(), Input{URI: "s.hcl", Format: FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	if reloaded.Annotations["dialect"] != "postgresql" {
		t.Fatalf("document annotations dropped on round-trip: %#v", reloaded.Annotations)
	}
}

func TestSQLAndNativeProduceEquivalentGraph(t *testing.T) {
	sql := `CREATE SCHEMA app;
CREATE TABLE app.users (
  id bigint NOT NULL,
  email text,
  CONSTRAINT users_pkey PRIMARY KEY (id),
  CONSTRAINT users_email_key UNIQUE (email)
);`
	fromSQL, err := Load(Input{URI: "schema.sql", Format: FormatSQL, Data: []byte(sql)})
	if err != nil {
		t.Fatal(err)
	}
	schemaResource := manualResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`, nil)
	table := manualResource(schema.KindTable, schema.Name{Schema: "app", Name: "users", Parent: schemaResource.ID}, `{"options":""}`, []schema.Dependency{{Target: schemaResource.ID, Type: schema.DependencyContains}})
	id := manualResource(schema.KindColumn, schema.Name{Schema: "app", Name: "id", Parent: table.ID}, `{"nullable":false,"ordinal":1,"type":"bigint"}`, []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}})
	email := manualResource(schema.KindColumn, schema.Name{Schema: "app", Name: "email", Parent: table.ID}, `{"nullable":true,"ordinal":2,"type":"text"}`, []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}})
	primary := manualResource(schema.KindPrimaryKey, schema.Name{Schema: "app", Name: "users_pkey", Parent: table.ID}, `{"definition":"PRIMARY KEY (id)"}`, []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}, {Target: id.ID, Type: schema.DependencyReferences}})
	unique := manualResource(schema.KindUniqueConstraint, schema.Name{Schema: "app", Name: "users_email_key", Parent: table.ID}, `{"definition":"UNIQUE (email)"}`, []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}, {Target: email.ID, Type: schema.DependencyReferences}})
	nativeDoc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{unique, email, schemaResource, primary, table, id}}}
	nativeBytes, err := nativeDoc.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	fromNative, err := Load(Input{URI: "schema.json", Format: FormatNative, Data: nativeBytes})
	if err != nil {
		t.Fatal(err)
	}
	for i := range fromSQL.Graph.Resources {
		fromSQL.Graph.Resources[i].Source = nil
		fromNative.Graph.Resources[i].Source = nil
	}
	a, _ := fromSQL.MarshalCanonical()
	b, _ := fromNative.MarshalCanonical()
	if string(a) != string(b) {
		t.Fatalf("graphs differ\n%s\n%s", a, b)
	}
}

func TestCanceledLoadStopsWithoutOutput(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := LoadContext(ctx, Input{URI: "schema.sql", Format: FormatSQL, Data: []byte("CREATE TABLE t(id int);")}); !errors.Is(err, context.Canceled) {
		t.Fatalf("expected cancellation, got %v", err)
	}
}

func manualResource(kind schema.Kind, name schema.Name, spec string, dependencies []schema.Dependency) schema.Resource {
	return schema.Resource{ID: schema.StableID(kind, name), Kind: kind, Name: name, Spec: json.RawMessage(spec), Dependencies: dependencies}
}

func TestCompositionIsDeterministicAndReportsBothConflicts(t *testing.T) {
	a, err := ParseSQL("a.sql", "CREATE TABLE app.users (id bigint);")
	if err != nil {
		t.Fatal(err)
	}
	ab, _ := a.MarshalCanonical()
	_, err = Load(Input{URI: "a.json", Format: FormatNative, Data: ab}, Input{URI: "b.sql", Format: FormatSQL, Data: []byte("CREATE TABLE app.users (id text);")})
	var conflict *ConflictError
	if !errors.As(err, &conflict) {
		t.Fatalf("expected conflict, got %v", err)
	}
	if conflict.First == "" || conflict.Second == "" {
		t.Fatalf("missing locations: %+v", conflict)
	}
	if _, err = schema.DecodeDocument(ab); err != nil {
		t.Fatal(err)
	}
}

func TestCompositionAllowsCrossSourceDependencies(t *testing.T) {
	doc, err := Load(
		Input{URI: "table.sql", Format: FormatSQL, Data: []byte("CREATE TABLE app.users (id bigint);")},
		Input{URI: "index.sql", Format: FormatSQL, Data: []byte("CREATE INDEX users_id_idx ON app.users (id);")},
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(doc.Graph.Resources) != 4 {
		t.Fatalf("expected schema, table, column, and index; got %d", len(doc.Graph.Resources))
	}
}

func TestNativeSourceCanReferenceResourceFromAnotherSource(t *testing.T) {
	tableDoc, err := parseSQL(context.Background(), "table.sql", "CREATE TABLE app.users (id bigint);", false)
	if err != nil {
		t.Fatal(err)
	}
	var table schema.Resource
	for _, resource := range tableDoc.Graph.Resources {
		if resource.Kind == schema.KindTable {
			table = resource
		}
	}
	indexName := schema.Name{Schema: "app", Name: "users_idx", Parent: table.Name.Parent}
	index := manualResource(schema.KindIndex, indexName, `{"definition":"(id)","unique":false}`, []schema.Dependency{{Target: table.Name.Parent, Type: schema.DependencyContains}, {Target: table.ID, Type: schema.DependencyReferences}})
	partial, err := json.Marshal(schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{index}}})
	if err != nil {
		t.Fatal(err)
	}
	tableSQL := Input{URI: "table.sql", Format: FormatSQL, Data: []byte("CREATE TABLE app.users (id bigint);")}
	doc, err := Load(tableSQL, Input{URI: "index.json", Format: FormatNative, Data: partial})
	if err != nil {
		t.Fatal(err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestOverloadedFunctionsHaveDistinctStableIdentity(t *testing.T) {
	doc, err := ParseSQL("functions.sql", `
CREATE FUNCTION app.find_user(id bigint) RETURNS text LANGUAGE sql AS $$ SELECT 'a' $$;
CREATE FUNCTION app.find_user(email text) RETURNS text LANGUAGE sql AS $$ SELECT 'b' $$;
`)
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, resource := range doc.Graph.Resources {
		if resource.Kind == schema.KindFunction {
			ids = append(ids, resource.ID)
		}
	}
	if len(ids) != 2 || ids[0] == ids[1] {
		t.Fatalf("overloads were not distinct: %#v", doc.Graph.Resources)
	}
}

func TestInlineConstraintsBecomeResources(t *testing.T) {
	doc, err := ParseSQL("inline.sql", `CREATE TABLE app.accounts (
id bigint PRIMARY KEY,
email text UNIQUE,
owner_id bigint REFERENCES app.accounts(id)
);`)
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[schema.Kind]int{}
	for _, resource := range doc.Graph.Resources {
		kinds[resource.Kind]++
	}
	if kinds[schema.KindPrimaryKey] != 1 || kinds[schema.KindUniqueConstraint] != 1 || kinds[schema.KindForeignKey] != 1 {
		t.Fatalf("missing inline constraints: %#v", kinds)
	}
}
