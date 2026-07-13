package virtualschema

import (
	"strings"
	"testing"
)

func testSpec(t *testing.T) Spec {
	t.Helper()
	old := SchemaVersion{Name: "app_v1", Tables: []TableView{{Name: "accounts", PhysicalTable: "accounts", Columns: []ColumnView{{Name: "created_at", PhysicalColumn: "created_at"}, {Name: "id", PhysicalColumn: "id"}, {Name: "name", PhysicalColumn: "name"}, {Name: "slug", PhysicalColumn: "slug"}}}}}
	cur := SchemaVersion{Name: "app_v2", Tables: []TableView{{Name: "accounts", PhysicalTable: "accounts", Columns: []ColumnView{{Name: "created_at", PhysicalColumn: "created_at"}, {Name: "display_name", PhysicalColumn: "name"}, {Name: "id", PhysicalColumn: "id"}, {Name: "slug", PhysicalColumn: "slug"}}}}}
	s, err := New(strings.Repeat("a", 64), "physical_app", old, cur)
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestSpecDeterministicStrict(t *testing.T) {
	a := testSpec(t)
	b := testSpec(t)
	if a.Digest != b.Digest {
		t.Fatal("nondeterministic")
	}
	raw, err := a.MarshalJSONCanonical()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseJSON(raw)
	if err != nil || got.Digest != a.Digest {
		t.Fatalf("roundtrip: %v", err)
	}
	bad := a
	bad.Current.Name = bad.Previous.Name
	if err := bad.Validate(); err == nil {
		t.Fatal("expected collision refusal")
	}
	for _, raw := range [][]byte{append(append([]byte(nil), raw...), []byte(` true`)...), []byte(`{"version":"x","version":"y"}`)} {
		if _, err := ParseJSON(raw); err == nil {
			t.Fatalf("accepted ambiguous JSON: %s", raw)
		}
	}
}
