package hclpostgres_test

import (
	"context"
	"os"
	"strings"
	"testing"

	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/source"
)

func TestHCLExampleLoadsDiffsAndPlans(t *testing.T) {
	ctx := context.Background()
	v1 := loadHCL(t, ctx, "schema-v1.hcl")
	v2 := loadHCL(t, ctx, "schema-v2.hcl")
	driver := postgres.New()
	v1, err := driver.Normalize(ctx, v1)
	if err != nil {
		t.Fatal(err)
	}
	v2, err = driver.Normalize(ctx, v2)
	if err != nil {
		t.Fatal(err)
	}

	changes, err := schema.Diff(v1, v2, schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if len(changes.Changes) != 2 {
		t.Fatalf("changes=%d, want 2", len(changes.Changes))
	}
	for _, change := range changes.Changes {
		if change.Operation != schema.OperationCreate {
			t.Fatalf("change=%s, want create", change.Operation)
		}
	}

	p, err := plan.Build(ctx, driver, v1, v2, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(p.Steps) != 2 {
		t.Fatalf("steps=%d, want 2", len(p.Steps))
	}
	joined := p.Steps[0].SQL + "\n" + p.Steps[1].SQL
	for _, column := range []string{"display_name", "last_seen_at"} {
		if !strings.Contains(joined, column) {
			t.Fatalf("plan does not add %s: %s", column, joined)
		}
	}
}

func TestAdvancedHCLCoversEveryPostgresCapability(t *testing.T) {
	doc := loadHCL(t, context.Background(), "advanced.hcl")
	seen := make(map[schema.Kind]bool, len(doc.Graph.Resources))
	for _, resource := range doc.Graph.Resources {
		seen[resource.Kind] = true
	}
	for _, capability := range postgres.New().Info().Capabilities {
		if !seen[capability.Kind] {
			t.Errorf("advanced.hcl does not demonstrate PostgreSQL capability kind %q", capability.Kind)
		}
	}
}

func TestFriendlyAdvancedHCLUsesHelpersWithoutEscapedJSONOrLiteralIDs(t *testing.T) {
	doc := loadHCL(t, context.Background(), "friendly-advanced.hcl")
	want := map[schema.Kind]bool{
		schema.KindPrimaryKey: true, schema.KindUniqueConstraint: true,
		schema.KindCheckConstraint: true, schema.KindForeignKey: true,
		schema.KindIndex: true, schema.KindPolicy: true, schema.KindGrant: true,
		schema.KindMembership: true, schema.KindDefaultPrivilege: true,
	}
	for _, resource := range doc.Graph.Resources {
		delete(want, resource.Kind)
	}
	if len(want) != 0 {
		t.Fatalf("friendly helper example is missing kinds: %+v", want)
	}
}

func loadHCL(t *testing.T, ctx context.Context, path string) schema.Document {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	doc, err := source.LoadContext(ctx, source.Input{URI: path, Format: source.FormatHCLSource, Data: data})
	if err != nil {
		t.Fatal(err)
	}
	return doc
}
