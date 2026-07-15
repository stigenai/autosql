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

func TestDocumentedDefaultExpressionHCLBuildsFreshPlan(t *testing.T) {
	ctx := context.Background()
	driver := postgres.New()
	desired, err := driver.Normalize(ctx, loadHCL(t, ctx, "defaults.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	empty, err := driver.Normalize(ctx, schema.Document{Version: schema.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	migration, err := plan.Build(ctx, driver, empty, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(migration.Steps) == 0 {
		t.Fatal("documented default-expression catalog produced an empty plan")
	}

	sequenceStep, idStep := -1, -1
	var sql strings.Builder
	for index, step := range migration.Steps {
		sql.WriteString(step.SQL)
		sql.WriteByte('\n')
		if strings.Contains(step.SQL, `CREATE SEQUENCE "defaults_demo"."widget_id_seq"`) {
			sequenceStep = index
		}
		if strings.Contains(step.SQL, `ADD COLUMN "id"`) {
			idStep = index
		}
	}
	if sequenceStep < 0 || idStep < 0 || sequenceStep >= idStep {
		t.Fatalf("sequence dependency order is not documented by the plan: sequence=%d id=%d", sequenceStep, idStep)
	}
	for _, want := range []string{
		`DEFAULT nextval('defaults_demo.widget_id_seq'::regclass)`,
		`DEFAULT 0.00`,
		`DEFAULT '{}'::jsonb`,
		`DEFAULT '550e8400-e29b-41d4-a716-446655440000'::uuid`,
		`DEFAULT pg_catalog.gen_random_uuid()`,
		`DEFAULT 'pending'::defaults_demo.job_status`,
		`DEFAULT '{}'::text[]`,
		`DEFAULT CURRENT_DATE`,
		`DEFAULT CURRENT_TIMESTAMP`,
		`DEFAULT pg_catalog.timezone('utc'::text, CURRENT_TIMESTAMP)`,
	} {
		if !strings.Contains(sql.String(), want) {
			t.Errorf("documented plan is missing %q", want)
		}
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
