// Package plugintest contains reusable contract checks for plugin authors.
package plugintest

import (
	"bytes"
	"context"
	"testing"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
)

func Driver(t *testing.T, d plugin.Driver, req plugin.InspectRequest) {
	t.Helper()
	if err := plugin.ValidateDriver(d); err != nil {
		t.Fatalf("invalid driver metadata: %v", err)
	}
	doc, err := plugin.GuardDriver{Driver: d}.Inspect(context.Background(), req)
	if err != nil {
		t.Fatalf("inspect: %v", err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("inspect returned invalid schema: %v", err)
	}
	for _, r := range doc.Graph.Resources {
		if d.Info().Capability(r.Kind).Mode == plugin.Unsupported {
			t.Fatalf("inspect returned %q without advertising capability", r.Kind)
		}
	}
	normalized, err := plugin.GuardDriver{Driver: d}.Normalize(context.Background(), doc)
	if err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if err := normalized.Validate(); err != nil {
		t.Fatalf("normalize returned invalid schema: %v", err)
	}
	again, err := plugin.GuardDriver{Driver: d}.Normalize(context.Background(), doc)
	if err != nil {
		t.Fatalf("second normalize: %v", err)
	}
	firstJSON, err := normalized.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	againJSON, err := again.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(firstJSON, againJSON) {
		t.Fatal("normalize is not deterministic")
	}
	for _, r := range normalized.Graph.Resources {
		if d.Info().Capability(r.Kind).Mode != plugin.Managed {
			continue
		}
		copy := r
		changes := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "conformance-create", Operation: schema.OperationCreate, ResourceID: r.ID, After: &copy}}}
		statements, renderErr := plugin.GuardDriver{Driver: d}.Render(context.Background(), plugin.RenderRequest{Changes: changes})
		if renderErr != nil {
			t.Fatalf("render managed %q: %v", r.Kind, renderErr)
		}
		if len(statements) == 0 || statements[0].ChangeID != "conformance-create" || statements[0].SQL == "" {
			t.Fatalf("render returned no attributable statement for managed %q", r.Kind)
		}
		break
	}
}
func Source(t *testing.T, s plugin.SourceProvider, req plugin.SourceRequest) {
	t.Helper()
	if err := plugin.ValidateSource(s); err != nil {
		t.Fatalf("invalid source metadata: %v", err)
	}
	doc, err := plugin.GuardSource{Source: s}.Load(context.Background(), req)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if err := doc.Validate(); err != nil {
		t.Fatalf("load returned invalid schema: %v", err)
	}
	for _, r := range doc.Graph.Resources {
		if s.Info().Capability(r.Kind).Mode == plugin.Unsupported {
			t.Fatalf("load returned %q without advertising capability", r.Kind)
		}
	}
}
