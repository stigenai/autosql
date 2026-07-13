package orm

import (
	"autosql/pkg/plugin"
	"autosql/pkg/schema"
	"context"
	"testing"
)

func TestReferenceMatricesAndProviderContract(t *testing.T) {
	adapters := ReferenceAdapters()
	if len(adapters) != 4 {
		t.Fatal(len(adapters))
	}
	n := schema.Name{Schema: "app", Name: "users"}
	d := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{{ID: schema.StableID(schema.KindTable, n), Kind: schema.KindTable, Name: n}}}}
	d.Normalize()
	for _, a := range adapters {
		if err := a.Info().Capability(schema.KindTable).Mode; err != "read_only" {
			t.Fatalf("%s capability=%v", a.Name, err)
		}
		a.LoadFunc = func(context.Context, plugin.SourceRequest) (schema.Document, error) { return d, nil }
		got, err := a.Load(context.Background(), plugin.SourceRequest{URI: "fixture://users"})
		if err != nil || len(got.Graph.Resources) != 1 {
			t.Fatalf("%s: %v", a.Name, err)
		}
	}
}
