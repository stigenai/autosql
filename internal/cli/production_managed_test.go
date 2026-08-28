package cli

import (
	"testing"

	"autosql/pkg/policy"
	"autosql/pkg/schema"
)

func TestManagedInspectionDocumentPreservesAdvancedScope(t *testing.T) {
	roleID := "role:runtime"
	tableID := "table:widgets"
	got, err := managedInspectionDocument([]policy.Resource{
		{Kind: string(schema.KindRole), Name: "runtime", Attributes: map[string]any{"id": roleID}},
		{Kind: string(schema.KindTable), Name: "public.widgets", Attributes: map[string]any{
			"id": tableID,
			"dependencies": []any{
				map[string]any{"target": roleID, "type": string(schema.DependencyOwns)},
			},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}

	if got.Version != schema.SchemaVersion {
		t.Fatalf("version = %q", got.Version)
	}
	if len(got.Graph.Resources) != 2 {
		t.Fatalf("resources = %#v", got.Graph.Resources)
	}
	if got.Graph.Resources[0].ID != roleID || got.Graph.Resources[0].Kind != schema.KindRole {
		t.Fatalf("role = %#v", got.Graph.Resources[0])
	}
	if got.Graph.Resources[1].ID != tableID || len(got.Graph.Resources[1].Dependencies) != 1 {
		t.Fatalf("owned table = %#v", got.Graph.Resources[1])
	}
	dependency := got.Graph.Resources[1].Dependencies[0]
	if dependency.Target != roleID || dependency.Type != schema.DependencyOwns {
		t.Fatalf("owner dependency = %#v", dependency)
	}
}

func TestManagedInspectionDocumentRejectsMissingID(t *testing.T) {
	_, err := managedInspectionDocument([]policy.Resource{{
		Kind:       string(schema.KindRole),
		Name:       "missing-id",
		Attributes: map[string]any{},
	}})
	if err == nil {
		t.Fatal("missing signed resource ID accepted")
	}
}
