package hclpostgres_test

import (
	"context"
	"encoding/json"
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

func TestDatabaseBootstrapHCLUsesSeparateNonSecretTargetContract(t *testing.T) {
	doc := loadHCL(t, context.Background(), "database-bootstrap.hcl")
	var databases []schema.Resource
	for _, resource := range doc.Graph.Resources {
		if resource.Kind == schema.KindDatabase {
			databases = append(databases, resource)
		}
	}
	if len(databases) != 1 {
		t.Fatalf("database resources=%d", len(databases))
	}
	target, err := postgres.DatabaseTargetFromResource(databases[0])
	if err != nil {
		t.Fatal(err)
	}
	if target.Name != "autosql_cell" || target.MaintenanceDatabase != "postgres" || target.Endpoint.Host != "postgres.internal" || target.Owner != "autosql_cell_owner" {
		t.Fatalf("target=%+v", target)
	}
	if strings.Contains(string(databases[0].Spec), "password") || strings.Contains(string(databases[0].Spec), "postgres://") {
		t.Fatal("database HCL contains a credential or resolved URL")
	}
}

func TestExtensionReadinessHCLHasOneTargetAndExactExtensionRequests(t *testing.T) {
	doc := loadHCL(t, context.Background(), "extension-readiness.hcl")
	var databases int
	versions := map[string]string{}
	for _, resource := range doc.Graph.Resources {
		switch resource.Kind {
		case schema.KindDatabase:
			databases++
		case schema.KindExtension:
			var spec struct {
				Version string `json:"version"`
			}
			err := json.Unmarshal(resource.Spec, &spec)
			if err != nil {
				t.Fatal(err)
			}
			versions[resource.Name.Name] = spec.Version
		}
	}
	if databases != 1 || versions["hstore"] != "1.8" || versions["pgcrypto"] != "1.3" {
		t.Fatalf("databases=%d extensions=%v", databases, versions)
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
		`DEFAULT '10.0.0.0/8'::cidr`,
		`DEFAULT '192.0.2.1/24'::inet`,
		`DEFAULT '08:00:2b:01:02:03'::macaddr`,
		`DEFAULT pg_catalog.gen_random_uuid()`,
		`DEFAULT pg_catalog.gen_random_uuid()::text`,
		`DEFAULT 'pending'::defaults_demo.job_status`,
		`DEFAULT '{}'::text[]`,
		`DEFAULT CURRENT_DATE`,
		`DEFAULT CURRENT_TIMESTAMP`,
		`DEFAULT pg_catalog.timezone('utc'::text, CURRENT_TIMESTAMP)`,
		`DEFAULT (extract(epoch from CURRENT_TIMESTAMP) OPERATOR(pg_catalog.*) 1000)::bigint`,
	} {
		if !strings.Contains(sql.String(), want) {
			t.Errorf("documented plan is missing %q", want)
		}
	}
}

func TestDocumentedStoredGeneratedHCLPlansWithExternalRoutine(t *testing.T) {
	ctx := context.Background()
	driver := postgres.New()
	desired, err := driver.Normalize(ctx, loadHCL(t, ctx, "generated.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	current := schema.Document{Version: schema.SchemaVersion}
	for _, resource := range desired.Graph.Resources {
		if resource.Kind == schema.KindSchema || resource.Kind == schema.KindFunction {
			current.Graph.Resources = append(current.Graph.Resources, resource)
		}
	}
	current, err = driver.Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	migration, err := plan.Build(ctx, driver, current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var sql strings.Builder
	for _, step := range migration.Steps {
		sql.WriteString(step.SQL)
		sql.WriteByte('\n')
	}
	if !strings.Contains(sql.String(), `GENERATED ALWAYS AS (generated_demo.lifecycle_state_to_v2(state)) STORED`) {
		t.Fatalf("documented generated-column plan is missing its bounded expression:\n%s", sql.String())
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
