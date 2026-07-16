package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosql/pkg/bootstrap"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"

	"github.com/jackc/pgx/v5"
)

func TestDatabaseBootstrapCommandPlansAndExecutesWithoutLeakingMaintenanceURL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bootstrap.hcl")
	hcl := `database "cell" {
  mode = "managed"
  endpoint = { host = "db.internal", port = 5432, tls_mode = "verify-full" }
  maintenance_database = "postgres"
  owner = "postgres"
  connection_limit = -1
  allow_connections = true
}

schema "app" {}
`
	if err := os.WriteFile(path, []byte(hcl), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_BOOTSTRAP_TEST_URL", "postgres://user:seeded-secret@db.internal/postgres")
	originalPlan, originalExecute := planDatabaseBootstrap, executeDatabaseBootstrapURL
	defer func() { planDatabaseBootstrap, executeDatabaseBootstrapURL = originalPlan, originalExecute }()
	planned, executed := false, false
	planDatabaseBootstrap = func(_ context.Context, target bootstrap.DatabaseTarget, desired schema.Document, options plan.Options) (bootstrap.Plan, error) {
		planned = target.Name == "cell" && target.Endpoint.Host == "db.internal" && len(desired.Graph.Resources) == 2 && options.Render["concurrent_indexes"] == "true" && options.Render["reviewed_routine_digests"] == "sha256:reviewed"
		return bootstrap.Plan{Digest: "sha256:plan"}, nil
	}
	executeDatabaseBootstrapURL = func(_ context.Context, maintenanceURL string, _ bootstrap.Plan, _ postgres.BootstrapExecutionHooks) (postgres.BootstrapExecutionResult, error) {
		executed = strings.Contains(maintenanceURL, "seeded-secret")
		return postgres.BootstrapExecutionResult{PlanDigest: "sha256:plan", Completed: true, CreatedDatabase: true, AppliedSteps: 2}, nil
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"database", "bootstrap", "--file", path, "--maintenance-url", "env://AUTOSQL_BOOTSTRAP_TEST_URL", "--reviewed-routine-digest", "sha256:reviewed", "--json"}, Streams{Out: &stdout, Err: &stderr})
	if code != 0 || !planned || !executed {
		t.Fatalf("code=%d planned=%v executed=%v stdout=%s stderr=%s", code, planned, executed, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), "seeded-secret") || !strings.Contains(stdout.String(), `"status":"completed"`) {
		t.Fatalf("unsafe or incomplete output stdout=%s stderr=%s", stdout.String(), stderr.String())
	}
}

func TestDatabaseBootstrapCommandAgainstNewPostgresDatabase(t *testing.T) {
	maintenanceURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if maintenanceURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	config, err := pgx.ParseConfig(maintenanceURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	var owner string
	if err := conn.QueryRow(ctx, `select current_user`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	database := fmt.Sprintf("autosql_cli_bootstrap_%d", time.Now().UnixNano())
	defer postgres.DropDatabaseURL(context.Background(), maintenanceURL, database, true)
	path := filepath.Join(t.TempDir(), "bootstrap-live.hcl")
	hcl := fmt.Sprintf(`database %q {
  mode = "managed"
  endpoint = { host = %q, port = %d, tls_mode = "disable" }
  maintenance_database = %q
  owner = %q
  encoding = "UTF8"
  template = "template0"
  tablespace = "pg_default"
  connection_limit = -1
  allow_connections = true
}
schema "cli_bootstrap" {}
table "items" {
  schema = "cli_bootstrap"
  column "id" {
    type = "bigint"
    nullable = false
    ordinal = 1
  }
}
`, database, config.Host, config.Port, config.Database, owner)
	if err := os.WriteFile(path, []byte(hcl), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_CLI_BOOTSTRAP_URL", maintenanceURL)
	var stdout, stderr bytes.Buffer
	code := Run(ctx, []string{"database", "bootstrap", "--file", path, "--maintenance-url", "env://AUTOSQL_CLI_BOOTSTRAP_URL", "--json"}, Streams{Out: &stdout, Err: &stderr})
	if code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if strings.Contains(stdout.String()+stderr.String(), config.Password) {
		t.Fatal("CLI leaked database password")
	}
	targetConfig := config.Copy()
	targetConfig.Database = database
	targetConnection, err := pgx.ConnectConfig(ctx, targetConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer targetConnection.Close(context.Background())
	document, err := postgres.InspectConn(ctx, targetConnection, postgres.Options{Schemas: []string{"cli_bootstrap"}})
	if err != nil {
		t.Fatal(err)
	}
	kinds := map[schema.Kind]bool{}
	for _, resource := range document.Graph.Resources {
		kinds[resource.Kind] = true
	}
	if !kinds[schema.KindSchema] || !kinds[schema.KindTable] || !kinds[schema.KindColumn] {
		t.Fatalf("CLI bootstrap graph=%+v stdout=%s stderr=%s", document.Graph.Resources, stdout.String(), stderr.String())
	}
}
