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

func TestPostgresRepresentativeTransitionsGolden(t *testing.T) {
	makeResource := func(kind schema.Kind, name schema.Name, spec string, deps ...schema.Dependency) schema.Resource {
		r := schema.Resource{Kind: kind, Name: name, Spec: json.RawMessage(spec), Dependencies: deps}
		r.ID = schema.StableID(kind, name)
		return r
	}
	s := makeResource(schema.KindSchema, schema.Name{Name: "public"}, `{}`)
	contained := func(target string) []schema.Dependency {
		return []schema.Dependency{{Target: target, Type: schema.DependencyContains}}
	}
	account := makeResource(schema.KindTable, schema.Name{Schema: "public", Name: "accounts", Parent: s.ID}, `{}`, contained(s.ID)...)
	identityBefore := makeResource(schema.KindColumn, schema.Name{Schema: "public", Name: "id", Parent: account.ID}, `{"type":"int8","identity":"by_default"}`, contained(account.ID)...)
	identityAfter := identityBefore
	identityAfter.Spec = json.RawMessage(`{"type":"bigint","identity":"always"}`)
	enumBefore := makeResource(schema.KindEnum, schema.Name{Schema: "public", Name: "status", Parent: s.ID}, `{"values":["pending"]}`, contained(s.ID)...)
	enumAfter := enumBefore
	enumAfter.Spec = json.RawMessage(`{"values":["pending","active"]}`)
	viewBefore := makeResource(schema.KindView, schema.Name{Schema: "public", Name: "active_accounts", Parent: s.ID}, `{"definition":"SELECT  1"}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains}, schema.Dependency{Target: account.ID, Type: schema.DependencyReferences})
	viewAfter := viewBefore
	viewAfter.Spec = json.RawMessage(`{"definition":"SELECT 2"}`)
	oldIndex := makeResource(schema.KindIndex, schema.Name{Schema: "public", Name: "accounts_email_idx", Parent: account.ID}, `{"definition":"(email)"}`, contained(account.ID)...)
	oldFK := makeResource(schema.KindForeignKey, schema.Name{Schema: "public", Name: "accounts_owner_fkey", Parent: account.ID}, `{"columns":["owner_id"],"references":"users(id)"}`, contained(account.ID)...)
	obsolete := makeResource(schema.KindTable, schema.Name{Schema: "public", Name: "obsolete", Parent: s.ID}, `{}`, contained(s.ID)...)
	obsoleteColumn := makeResource(schema.KindColumn, schema.Name{Schema: "public", Name: "value", Parent: obsolete.ID}, `{"type":"text"}`, contained(obsolete.ID)...)
	invoiceBefore := makeResource(schema.KindTable, schema.Name{Schema: "public", Name: "invoice_old", Parent: s.ID}, `{"persistence":"p"}`, contained(s.ID)...)
	invoiceAfter := makeResource(schema.KindTable, schema.Name{Schema: "public", Name: "invoices", Parent: s.ID}, `{"persistence":"u"}`, contained(s.ID)...)
	newIndex := makeResource(schema.KindIndex, schema.Name{Schema: "public", Name: "accounts_status_idx", Parent: account.ID}, `{"definition":"(status)"}`, contained(account.ID)...)
	current := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{s, account, identityBefore, enumBefore, viewBefore, oldIndex, oldFK, obsolete, obsoleteColumn, invoiceBefore}}}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{s, account, identityAfter, enumAfter, viewAfter, invoiceAfter, newIndex}}}
	current, err := New().Normalize(context.Background(), current)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = New().Normalize(context.Background(), desired)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := schema.Diff(current, desired, schema.DiffOptions{RenameHints: []schema.RenameHint{{From: invoiceBefore.ID, To: invoiceAfter.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err := changes.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	expected, err := os.ReadFile("testdata/representative_transitions.golden.json")
	if err != nil || string(actual) != string(expected) {
		t.Fatalf("representative golden mismatch: %v\nactual: %s\nexpected: %s", err, actual, expected)
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
		if r.Kind == schema.KindPrimaryKey && r.Annotations["autosql.io/generated-name"] != "" {
			t.Fatalf("default-looking name was trusted: %#v", r)
		}
	}
}

func TestAuthoredAnnotationsCannotForgeGeneratedProvenance(t *testing.T) {
	parent := schema.Resource{Kind: schema.KindTable, Name: schema.Name{Schema: "public", Name: "users"}, Spec: json.RawMessage(`{}`)}
	parent.ID = schema.StableID(parent.Kind, parent.Name)
	pk := schema.Resource{Kind: schema.KindPrimaryKey, Name: schema.Name{Schema: "public", Name: "users_pkey", Parent: parent.ID}, Annotations: map[string]string{"autosql.io/generated-name": "true", "autosql.io/name-origin": "generated"}, Dependencies: []schema.Dependency{{Target: parent.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(`{}`)}
	pk.ID = schema.StableID(pk.Kind, pk.Name)
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{parent, pk}}}
	normalized, err := New().Normalize(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range normalized.Graph.Resources {
		if r.Kind == schema.KindPrimaryKey && r.Annotations["autosql.io/generated-name"] != "" {
			t.Fatalf("authored provenance was trusted: %#v", r)
		}
	}
	wire, err := json.Marshal(normalized)
	if err != nil {
		t.Fatal(err)
	}
	var roundTrip schema.Document
	if err := json.Unmarshal(wire, &roundTrip); err != nil {
		t.Fatal(err)
	}
	for _, r := range roundTrip.Graph.Resources {
		if r.Annotations["autosql.io/generated-name"] != "" || r.Annotations["autosql.io/name-origin"] != "" {
			t.Fatal("untrusted provenance leaked into the normalized wire format")
		}
	}
}

func TestNormalizeDefaultEquivalence(t *testing.T) {
	makeDoc := func(defaultValue *string) schema.Document {
		spec := map[string]any{"type": "timestamp"}
		if defaultValue != nil {
			spec["default"] = *defaultValue
		}
		raw, _ := json.Marshal(spec)
		r := schema.Resource{Kind: schema.KindColumn, Name: schema.Name{Schema: "public", Name: "created_at"}, Spec: raw}
		r.ID = schema.StableID(r.Kind, r.Name)
		return schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{r}}}
	}
	forms := []string{"current_timestamp", "CURRENT_TIMESTAMP()", "now()", "transaction_timestamp()"}
	base, _ := New().Normalize(context.Background(), makeDoc(&forms[0]))
	for _, form := range forms[1:] {
		other, _ := New().Normalize(context.Background(), makeDoc(&form))
		changes, err := schema.Diff(base, other, schema.DiffOptions{})
		if err != nil || len(changes.Changes) != 0 {
			t.Fatalf("default form %q differs: %+v %v", form, changes, err)
		}
	}
	null := "NULL"
	withNull, _ := New().Normalize(context.Background(), makeDoc(&null))
	absent, _ := New().Normalize(context.Background(), makeDoc(nil))
	changes, err := schema.Diff(withNull, absent, schema.DiffOptions{})
	if err != nil || len(changes.Changes) != 0 {
		t.Fatalf("DEFAULT NULL differs from absent: %+v %v", changes, err)
	}
	castNull := "NULL::timestamp without time zone"
	withCastNull, _ := New().Normalize(context.Background(), makeDoc(&castNull))
	changes, err = schema.Diff(withCastNull, absent, schema.DiffOptions{})
	if err != nil || len(changes.Changes) != 0 {
		t.Fatalf("cast DEFAULT NULL differs from absent: %+v %v", changes, err)
	}
	precisionA, precisionB := "current_timestamp(3)", "CURRENT_TIMESTAMP(3)"
	a, _ := New().Normalize(context.Background(), makeDoc(&precisionA))
	b, _ := New().Normalize(context.Background(), makeDoc(&precisionB))
	changes, err = schema.Diff(a, b, schema.DiffOptions{})
	if err != nil || len(changes.Changes) != 0 {
		t.Fatalf("precision/casing defaults differ: %+v %v", changes, err)
	}
}

func TestNormalizeTypesConservatively(t *testing.T) {
	cases := map[string]string{
		"varchar(42)":             "character varying(42)",
		"pg_catalog.varchar(8)[]": "character varying(8)[]",
		`"CaseSensitiveType"`:     `"CaseSensitiveType"`,
		"App.CustomType":          "App.CustomType",
	}
	for input, want := range cases {
		if got := postgresTypeAlias(input); got != want {
			t.Errorf("postgresTypeAlias(%q)=%q want %q", input, got, want)
		}
	}
	manual := schema.Resource{Kind: schema.KindIndex, Name: schema.Name{Schema: "public", Name: "users_idx"}, Spec: json.RawMessage(`{}`)}
	manual.ID = schema.StableID(manual.Kind, manual.Name)
	normalized, err := New().Normalize(context.Background(), schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{manual}}})
	if err != nil {
		t.Fatal(err)
	}
	if normalized.Graph.Resources[0].Annotations["autosql.io/generated-name"] != "" {
		t.Fatal("suffix-only name was guessed as generated")
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
