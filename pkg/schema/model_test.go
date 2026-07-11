package schema_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"testing"

	"autosql/pkg/schema"
)

func resource(k schema.Kind, n schema.Name, spec string) schema.Resource {
	return schema.Resource{ID: schema.StableID(k, n), Kind: k, Name: n, Spec: json.RawMessage(spec)}
}

func fixture() schema.Document {
	sn := schema.Name{Name: "public"}
	s := resource(schema.KindSchema, sn, `{"owner":"app"}`)
	tn := schema.Name{Schema: "public", Name: "users", Parent: s.ID}
	table := resource(schema.KindTable, tn, `{"persistence":"permanent"}`)
	table.Dependencies = []schema.Dependency{{Target: s.ID, Type: schema.DependencyContains}}
	table.Annotations = map[string]string{"team": "identity"}
	cn := schema.Name{Schema: "public", Name: "id", Parent: table.ID}
	col := resource(schema.KindColumn, cn, `{"nullable":false,"type":"bigint"}`)
	col.Dependencies = []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}
	col.Source = &schema.SourceLocation{URI: "file:///schema.sql", Line: 3, Column: 3}
	return schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{col, table, s}}, Annotations: map[string]string{"environment": "test"}}
}

func TestGoldenCanonicalRoundTrip(t *testing.T) {
	d := fixture()
	got, err := d.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/schema_v1.golden.json")
	if err != nil {
		t.Fatalf("%v; canonical fixture is:\n%s", err, got)
	}
	if !bytes.Equal(got, want) {
		t.Fatalf("canonical JSON mismatch\n got: %s\nwant: %s", got, want)
	}
	decoded, err := schema.DecodeDocument(got)
	if err != nil {
		t.Fatal(err)
	}
	again, err := decoded.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, again) {
		t.Fatalf("round trip changed JSON\nfirst: %s\nagain: %s", got, again)
	}
}

func TestUnknownFieldsArePreserved(t *testing.T) {
	base, err := fixture().MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	var raw map[string]any
	if err := json.Unmarshal(base, &raw); err != nil {
		t.Fatal(err)
	}
	raw["future_document"] = map[string]any{"enabled": true}
	graph := raw["graph"].(map[string]any)
	resources := graph["resources"].([]any)
	resources[0].(map[string]any)["future_resource"] = []any{"x", float64(1)}
	resources[0].(map[string]any)["name"].(map[string]any)["future_name"] = "identity"
	resources[0].(map[string]any)["dependencies"].([]any)[0].(map[string]any)["future_edge"] = true
	resources[0].(map[string]any)["source"].(map[string]any)["future_source"] = 7
	input, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := schema.DecodeDocument(input)
	if err != nil {
		t.Fatal(err)
	}
	output, err := doc.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	var after map[string]any
	if err := json.Unmarshal(output, &after); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(raw["future_document"], after["future_document"]) {
		t.Fatal("document unknown field was lost")
	}
	gotRes := after["graph"].(map[string]any)["resources"].([]any)
	found := false
	for _, r := range gotRes {
		m := r.(map[string]any)
		if v, ok := m["future_resource"]; ok {
			found = reflect.DeepEqual(v, []any{"x", float64(1)})
		}
	}
	if !found {
		t.Fatal("resource unknown field was lost")
	}
	first := gotRes[0].(map[string]any)
	if first["name"].(map[string]any)["future_name"] != "identity" || first["dependencies"].([]any)[0].(map[string]any)["future_edge"] != true || first["source"].(map[string]any)["future_source"] != float64(7) {
		t.Fatal("nested unknown field was lost")
	}
}

func TestCanonicalJSONIgnoresRawObjectKeyOrder(t *testing.T) {
	a := fixture()
	b := fixture()
	a.Graph.Resources[0].Spec = json.RawMessage(`{"z":1,"a":{"y":2,"x":3}}`)
	b.Graph.Resources[0].Spec = json.RawMessage(`{"a":{"x":3,"y":2},"z":1}`)
	aj, err := a.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	bj, err := b.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(aj, bj) {
		t.Fatalf("semantically equal JSON was not canonical\n%s\n%s", aj, bj)
	}
}

func TestAllPostgresKindsAccepted(t *testing.T) {
	kinds := []schema.Kind{schema.KindDatabase, schema.KindSchema, schema.KindExtension, schema.KindEnum, schema.KindDomain, schema.KindComposite, schema.KindSequence, schema.KindTable, schema.KindColumn, schema.KindPrimaryKey, schema.KindUniqueConstraint, schema.KindCheckConstraint, schema.KindForeignKey, schema.KindIndex, schema.KindView, schema.KindMaterializedView, schema.KindFunction, schema.KindProcedure, schema.KindTrigger, schema.KindPolicy, schema.KindRole, schema.KindGrant, schema.KindReferenceData}
	resources := make([]schema.Resource, 0, len(kinds))
	for i, k := range kinds {
		n := schema.Name{Name: string(k)}
		if i > 0 {
			n.Name += string(rune('a' + i))
		}
		resources = append(resources, resource(k, n, `{}`))
	}
	d := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: resources}}
	if err := d.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestUnsupportedKindAndVersion(t *testing.T) {
	d := fixture()
	d.Graph.Resources[0].Kind = "future_kind"
	if err := d.Validate(); !errors.Is(err, schema.ErrUnsupportedKind) {
		t.Fatalf("got %v", err)
	}
	d = fixture()
	d.Version = "autosql.schema/v99"
	if err := d.Validate(); !errors.Is(err, schema.ErrUnsupportedVersion) {
		t.Fatalf("got %v", err)
	}
}

func TestStableIDIncludesParentAndCase(t *testing.T) {
	a := schema.StableID(schema.KindColumn, schema.Name{Name: "ID", Parent: "table:1"})
	b := schema.StableID(schema.KindColumn, schema.Name{Name: "id", Parent: "table:1"})
	c := schema.StableID(schema.KindColumn, schema.Name{Name: "ID", Parent: "table:2"})
	if a == b || a == c || b == c {
		t.Fatal("distinct logical identities collided")
	}
	if a != schema.StableID(schema.KindColumn, schema.Name{Name: "ID", Parent: "table:1"}) {
		t.Fatal("ID is not stable")
	}
}

func TestChangeSetRoundTrip(t *testing.T) {
	r := fixture().Graph.Resources[0]
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "create-1", Operation: schema.OperationCreate, ResourceID: r.ID, After: &r, Details: json.RawMessage(`{"online":true}`), Extra: map[string]json.RawMessage{"future": json.RawMessage(`42`)}}}}
	got, err := cs.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := schema.DecodeChangeSet(got)
	if err != nil {
		t.Fatal(err)
	}
	again, err := decoded.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, again) {
		t.Fatalf("change round trip mismatch\n%s\n%s", got, again)
	}
}

func TestRenameChangesStableIdentity(t *testing.T) {
	before := resource(schema.KindTable, schema.Name{Schema: "public", Name: "old_name"}, `{}`)
	after := resource(schema.KindTable, schema.Name{Schema: "public", Name: "new_name"}, `{}`)
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{
		ID: "rename-1", Operation: schema.OperationRename, ResourceID: after.ID,
		Before: &before, After: &after,
	}}}
	if err := cs.Validate(); err != nil {
		t.Fatal(err)
	}
	cs.Changes[0].ResourceID = before.ID
	if err := cs.Validate(); err == nil {
		t.Fatal("expected resource_id mismatch")
	}
}
