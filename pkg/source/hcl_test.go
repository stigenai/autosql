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
