package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"autosql/pkg/bootstrap"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"

	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
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

func TestDatabaseBootstrapPrepareReportsCompleteInventoryWithoutResolvingCredentials(t *testing.T) {
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
	original := prepareBootstrapAuthorizationInventory
	defer func() { prepareBootstrapAuthorizationInventory = original }()
	definition := `CREATE FUNCTION app.run() RETURNS text LANGUAGE sql AS $$ SELECT 'token=current_setting' $$`
	prepareBootstrapAuthorizationInventory = func(_ context.Context, target bootstrap.DatabaseTarget, desired schema.Document, options postgres.BootstrapAuthorizationInventoryOptions) (postgres.BootstrapAuthorizationInventory, error) {
		if target.Name != "cell" || len(desired.Graph.Resources) != 2 {
			t.Fatalf("target=%+v resources=%d options=%+v", target, len(desired.Graph.Resources), options)
		}
		digest := fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(definition)))
		routine := postgres.BootstrapRoutineAuthorization{ResourceID: "function:1", Kind: "function", Schema: "app", Name: "run", Signature: `"app"."run"()`, Language: "sql", SourceDigest: digest, DigestReviewRequired: true}
		if options.IncludeRoutineSource {
			routine.Definition = definition
		}
		inventory := postgres.BootstrapAuthorizationInventory{
			Version: postgres.BootstrapAuthorizationInventoryVersion, PlanDigest: "sha256:" + strings.Repeat("a", 64), SourceDigest: "sha256:" + strings.Repeat("d", 64), Database: "cell",
			PlanSummary: postgres.BootstrapAuthorizationPlanSummary{SchemaPlanDigest: "sha256:" + strings.Repeat("c", 64), StepCount: 3, PhaseCount: 2},
			Routines:    []postgres.BootstrapRoutineAuthorization{routine},
			Extensions:  []postgres.BootstrapExtensionAuthorization{{ResourceID: "extension:1", Name: "pgcrypto", Version: "1.3", Schema: "app", AllowlistRequired: true, ExactVersionRequired: true, SchemaPolicyRequired: true, ServerPackageRequired: true, Trusted: true}},
		}
		return inventory, nil
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"database", "bootstrap", "prepare", "--file", path, "--json"}, Streams{Out: &stdout, Err: &stderr})
	if code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	output := stdout.String() + stderr.String()
	for _, want := range []string{`"plan_digest":"sha256:`, `"source_digest":"sha256:`, `"name":"pgcrypto"`, `"server_package_required":true`} {
		if !strings.Contains(output, want) {
			t.Fatalf("prepare output missing %q: %s", want, output)
		}
	}
	if strings.Contains(output, "maintenance") || strings.Contains(output, `"definition"`) || strings.Contains(output, "postgres://") {
		t.Fatalf("prepare output disclosed runtime or source material: %s", output)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run(context.Background(), []string{"database", "bootstrap", "prepare", "--file", path, "--hcl"}, Streams{Out: &stdout, Err: &stderr})
	if code != 0 || !strings.Contains(stdout.String(), `bootstrap_authorization_inventory {`) || !strings.Contains(stdout.String(), `routine_review "function:1"`) || strings.Contains(stdout.String(), "definition") {
		t.Fatalf("HCL prepare code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run(context.Background(), []string{"database", "bootstrap", "prepare", "--file", path, "--include-routine-source", "--json"}, Streams{Out: &stdout, Err: &stderr})
	if code != 0 || !strings.Contains(stdout.String(), "token=current_setting") || strings.Contains(stdout.String(), "REDACTED") {
		t.Fatalf("JSON source integrity code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	var jsonOutput struct {
		Data postgres.BootstrapAuthorizationInventory `json:"data"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &jsonOutput); err != nil || len(jsonOutput.Data.Routines) != 1 || jsonOutput.Data.Routines[0].Definition != definition || fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(jsonOutput.Data.Routines[0].Definition))) != jsonOutput.Data.Routines[0].SourceDigest {
		t.Fatalf("JSON source digest mismatch err=%v output=%s", err, stdout.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run(context.Background(), []string{"database", "bootstrap", "prepare", "--file", path, "--include-routine-source", "--hcl"}, Streams{Out: &stdout, Err: &stderr})
	if code != 0 || !strings.Contains(stdout.String(), "token=current_setting") || strings.Contains(stdout.String(), "REDACTED") {
		t.Fatalf("HCL source integrity code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	parsed, diagnostics := hclparse.NewParser().ParseHCL(stdout.Bytes(), "inventory.hcl")
	if diagnostics.HasErrors() {
		t.Fatalf("parse HCL inventory: %s", diagnostics.Error())
	}
	body := parsed.Body.(*hclsyntax.Body)
	routineBlock := body.Blocks[0].Body.Blocks[0]
	hclDefinition, definitionDiagnostics := routineBlock.Body.Attributes["definition"].Expr.Value(nil)
	hclDigest, digestDiagnostics := routineBlock.Body.Attributes["source_digest"].Expr.Value(nil)
	if definitionDiagnostics.HasErrors() || digestDiagnostics.HasErrors() || hclDefinition.AsString() != definition || fmt.Sprintf("sha256:%x", sha256.Sum256([]byte(hclDefinition.AsString()))) != hclDigest.AsString() {
		t.Fatalf("HCL source digest mismatch definition=%q digest=%q", hclDefinition.AsString(), hclDigest.AsString())
	}
}

func TestDatabaseBootstrapPrepareHumanOutputIsReviewable(t *testing.T) {
	inventory := postgres.BootstrapAuthorizationInventory{
		Version: postgres.BootstrapAuthorizationInventoryVersion, PlanDigest: "sha256:" + strings.Repeat("a", 64), SourceDigest: "sha256:" + strings.Repeat("d", 64), Database: "cell",
		PlanSummary: postgres.BootstrapAuthorizationPlanSummary{SchemaPlanDigest: "sha256:" + strings.Repeat("c", 64), StepCount: 3, PhaseCount: 2},
		Routines:    []postgres.BootstrapRoutineAuthorization{{Kind: "function", Signature: `"app"."run"()`, Language: "sql", SourceDigest: "sha256:" + strings.Repeat("b", 64), DigestReviewRequired: true, Dependencies: []string{"contains:schema:app"}}},
		Extensions:  []postgres.BootstrapExtensionAuthorization{{Name: "pgcrypto", Version: "1.3", Schema: "app", Requires: []string{"plpgsql"}, SuperuserRequired: true, UntrustedExtensionAuthorizationRequired: true}},
	}
	text := humanBootstrapAuthorizationInventory(inventory)
	for _, want := range []string{"plan digest:", "routine reviews (1):", `function "app"."run"()`, "extension authorizations (1):", "pgcrypto version=1.3 schema=app", "additional_authorization=untrusted_extension", "authority=superuser"} {
		if !strings.Contains(text, want) {
			t.Fatalf("human inventory missing %q:\n%s", want, text)
		}
	}
}

func TestDatabaseBootstrapPreflightEmitsStructuredAndHumanExtensionReadiness(t *testing.T) {
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
resource "extension" "pgcrypto" {
  schema = "app"
  parent = schema_id("app")
  spec_json = jsonencode({ version = "1.3", relocatable = true, trusted = true, superuser = true, requires = [] })
  deps_json = jsonencode([contains(schema_id("app"))])
}`
	if err := os.WriteFile(path, []byte(hcl), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_EXTENSION_PREFLIGHT_URL", "postgres://operator:runtime-secret@db.internal/postgres")
	original := preflightExtensionReadinessURL
	defer func() { preflightExtensionReadinessURL = original }()
	preflightExtensionReadinessURL = func(_ context.Context, url string, target bootstrap.DatabaseTarget, desired schema.Document, policy postgres.ExtensionPolicy) (postgres.ExtensionReadinessReport, error) {
		if !strings.Contains(url, "runtime-secret") || target.Name != "cell" || len(desired.Graph.Resources) != 3 || !policy.Allowed["pgcrypto"] || !slices.Equal(policy.Versions["pgcrypto"], []string{"1.3"}) || !slices.Equal(policy.Schemas["pgcrypto"], []string{"app"}) {
			t.Fatalf("url/target/policy mismatch target=%+v policy=%+v resources=%d", target, policy, len(desired.Graph.Resources))
		}
		return postgres.ExtensionReadinessReport{Version: postgres.ExtensionReadinessReportVersion, ServerMajor: 18, Extensions: []postgres.ExtensionReadiness{{Name: "pgcrypto", RequestedVersion: "1.3", RequestedSchema: "app", Status: postgres.ExtensionMissingPackage, Reason: "control file absent", Remediation: "install pgcrypto.control"}}}, nil
	}
	args := []string{"database", "bootstrap", "preflight", "--file", path, "--maintenance-url", "env://AUTOSQL_EXTENSION_PREFLIGHT_URL", "--extension-allowlist", "pgcrypto", "--extension-version", "pgcrypto=1.3", "--extension-schema", "pgcrypto=app"}
	var stdout, stderr bytes.Buffer
	if code := Run(context.Background(), append(args, "--json"), Streams{Out: &stdout, Err: &stderr}); code != 0 || !strings.Contains(stdout.String(), `"status":"missing_package_control_file"`) || !strings.Contains(stdout.String(), `"remediation":"install pgcrypto.control"`) || strings.Contains(stdout.String()+stderr.String(), "runtime-secret") {
		t.Fatalf("JSON code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	if code := Run(context.Background(), args, Streams{Out: &stdout, Err: &stderr}); code != 0 || !strings.Contains(stdout.String(), "status=missing_package_control_file") || !strings.Contains(stdout.String(), "remediation: install pgcrypto.control") || strings.Contains(stdout.String()+stderr.String(), "runtime-secret") {
		t.Fatalf("human code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestDatabaseBootstrapRejectsPrepareOnlySourceFlagDuringExecution(t *testing.T) {
	if !strings.Contains(usage(), "database bootstrap prepare --file path [--include-routine-source] [--json|--hcl]") {
		t.Fatalf("prepare usage does not document HCL: %s", usage())
	}
	path := filepath.Join(t.TempDir(), "bootstrap.hcl")
	if err := os.WriteFile(path, []byte(`database "cell" {
  mode = "managed"
  endpoint = { host = "db.internal", port = 5432, tls_mode = "verify-full" }
  maintenance_database = "postgres"
  owner = "postgres"
  connection_limit = -1
  allow_connections = true
}`), 0o600); err != nil {
		t.Fatal(err)
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"database", "bootstrap", "--file", path, "--maintenance-url", "env://UNUSED", "--include-routine-source", "--json"}, Streams{Out: &stdout, Err: &stderr})
	if code != int(ExitUsage) || !strings.Contains(stdout.String()+stderr.String(), "valid only with database bootstrap prepare") {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
}

func TestDatabaseBootstrapAuthorizeAndVerifyManifestBeforeExecution(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "bootstrap.hcl")
	manifestPath := filepath.Join(directory, "authorization.json")
	if err := os.WriteFile(path, []byte(`database "cell" {
  mode = "managed"
  endpoint = { host = "db.internal", port = 5432, tls_mode = "verify-full" }
  maintenance_database = "postgres"
  owner = "postgres"
  connection_limit = -1
  allow_connections = true
}
schema "app" {}
`), 0o600); err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_BOOTSTRAP_AUTH_PRIVATE", base64.RawStdEncoding.EncodeToString(private))
	t.Setenv("AUTOSQL_BOOTSTRAP_AUTH_PUBLIC", base64.RawStdEncoding.EncodeToString(public))
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"database", "bootstrap", "authorize", "--file", path, "--authorization-signing-key", "env://AUTOSQL_BOOTSTRAP_AUTH_PRIVATE", "--authorization-key-id", "auth-key", "--authorization-issuer", "security", "--authorization-signer", "dba", "--output", manifestPath, "--json"}, Streams{Out: &stdout, Err: &stderr})
	if code != 0 || !strings.Contains(stdout.String(), `"status":"authorized"`) {
		t.Fatalf("authorize code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	manifestBytes, err := os.ReadFile(manifestPath)
	if err != nil || strings.Contains(string(manifestBytes), base64.RawStdEncoding.EncodeToString(private)) || strings.Contains(string(manifestBytes), "postgres://") {
		t.Fatalf("unsafe manifest err=%v body=%s", err, manifestBytes)
	}

	originalExecute := executeDatabaseBootstrapURL
	defer func() { executeDatabaseBootstrapURL = originalExecute }()
	executed := false
	executeDatabaseBootstrapURL = func(_ context.Context, maintenanceURL string, whole bootstrap.Plan, _ postgres.BootstrapExecutionHooks) (postgres.BootstrapExecutionResult, error) {
		executed = strings.Contains(maintenanceURL, "runtime-password") && whole.Digest != ""
		return postgres.BootstrapExecutionResult{PlanDigest: whole.Digest, Completed: true, AppliedSteps: len(whole.Steps)}, nil
	}
	stdout.Reset()
	stderr.Reset()
	code = Run(context.Background(), []string{"database", "bootstrap", "--file", path, "--maintenance-url", "env://AUTOSQL_MISSING_MAINTENANCE", "--authorization-manifest", manifestPath, "--authorization-public-key", "env://AUTOSQL_BOOTSTRAP_AUTH_PUBLIC", "--authorization-issuer", "security", "--authorization-signer", "dba", "--reviewed-routine-digest", "sha256:" + strings.Repeat("a", 64), "--json"}, Streams{Out: &stdout, Err: &stderr})
	if code != int(ExitUsage) || executed {
		t.Fatalf("mixed authorization paths code=%d executed=%v stdout=%s stderr=%s", code, executed, stdout.String(), stderr.String())
	}
	for name, legacy := range map[string][]string{"allowlist": {"--extension-allowlist", "pgcrypto"}, "version": {"--extension-version", "pgcrypto=1.3"}, "schema": {"--extension-schema", "pgcrypto=app"}} {
		stdout.Reset()
		stderr.Reset()
		args := []string{"database", "bootstrap", "--file", path, "--maintenance-url", "env://AUTOSQL_MISSING_MAINTENANCE", "--authorization-manifest", manifestPath, "--authorization-public-key", "env://AUTOSQL_BOOTSTRAP_AUTH_PUBLIC", "--authorization-issuer", "security", "--authorization-signer", "dba", "--json"}
		args = append(args, legacy...)
		if code := Run(context.Background(), args, Streams{Out: &stdout, Err: &stderr}); code != int(ExitUsage) || executed {
			t.Fatalf("direct %s conflict code=%d executed=%v stdout=%s stderr=%s", name, code, executed, stdout.String(), stderr.String())
		}
	}

	// A signature-corrupted manifest must fail before even resolving the
	// maintenance URL, proving verification is ahead of every mutation path.
	var corrupted map[string]any
	if err := json.Unmarshal(manifestBytes, &corrupted); err != nil {
		t.Fatal(err)
	}
	corrupted["signature"].(map[string]any)["value"] = "AAAA"
	corruptedBytes, _ := json.Marshal(corrupted)
	corruptedPath := filepath.Join(directory, "corrupted.json")
	if err := os.WriteFile(corruptedPath, corruptedBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code = Run(context.Background(), []string{"database", "bootstrap", "--file", path, "--maintenance-url", "env://AUTOSQL_MISSING_MAINTENANCE", "--authorization-manifest", corruptedPath, "--authorization-public-key", "env://AUTOSQL_BOOTSTRAP_AUTH_PUBLIC", "--authorization-issuer", "security", "--authorization-signer", "dba", "--json"}, Streams{Out: &stdout, Err: &stderr})
	if code != int(ExitValidation) || executed {
		t.Fatalf("corrupt manifest code=%d executed=%v stdout=%s stderr=%s", code, executed, stdout.String(), stderr.String())
	}

	t.Setenv("AUTOSQL_BOOTSTRAP_MANIFEST_URL", "postgres://user:runtime-password@db.internal/postgres")
	stdout.Reset()
	stderr.Reset()
	code = Run(context.Background(), []string{"database", "bootstrap", "--file", path, "--maintenance-url", "env://AUTOSQL_BOOTSTRAP_MANIFEST_URL", "--authorization-manifest", manifestPath, "--authorization-public-key", "env://AUTOSQL_BOOTSTRAP_AUTH_PUBLIC", "--authorization-issuer", "security", "--authorization-signer", "dba", "--json"}, Streams{Out: &stdout, Err: &stderr})
	if code != 0 || !executed || strings.Contains(stdout.String()+stderr.String(), "runtime-password") {
		t.Fatalf("verified manifest code=%d executed=%v stdout=%s stderr=%s", code, executed, stdout.String(), stderr.String())
	}
}

func TestDatabaseBootstrapUsesHCLAuthorizationRuntimeReferences(t *testing.T) {
	directory := t.TempDir()
	path := filepath.Join(directory, "bootstrap.hcl")
	manifestPath := filepath.Join(directory, "authorization.json")
	hcl := fmt.Sprintf(`database "cell" {
  mode = "managed"
  endpoint = { host = "db.internal", port = 5432, tls_mode = "verify-full" }
  maintenance_database = "postgres"
  owner = "postgres"
  connection_limit = -1
  allow_connections = true
  bootstrap_authorization = {
    manifest = %q
    public_key = "env://AUTOSQL_HCL_AUTH_PUBLIC"
    issuer = "security"
    signer = "dba"
    purpose = "bootstrap-authorization"
  }
}
schema "app" {}
`, "file://"+manifestPath)
	if err := os.WriteFile(path, []byte(hcl), 0o600); err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_HCL_AUTH_PRIVATE", base64.RawStdEncoding.EncodeToString(private))
	t.Setenv("AUTOSQL_HCL_AUTH_PUBLIC", base64.RawStdEncoding.EncodeToString(public))
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"database", "bootstrap", "authorize", "--file", path, "--authorization-signing-key", "env://AUTOSQL_HCL_AUTH_PRIVATE", "--authorization-key-id", "hcl-key", "--authorization-issuer", "security", "--authorization-signer", "dba", "--output", manifestPath, "--json"}, Streams{Out: &stdout, Err: &stderr})
	if code != 0 {
		t.Fatalf("authorize code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	originalExecute := executeDatabaseBootstrapURL
	defer func() { executeDatabaseBootstrapURL = originalExecute }()
	executed := false
	executeDatabaseBootstrapURL = func(_ context.Context, maintenanceURL string, whole bootstrap.Plan, _ postgres.BootstrapExecutionHooks) (postgres.BootstrapExecutionResult, error) {
		executed = strings.Contains(maintenanceURL, "hcl-runtime-password")
		return postgres.BootstrapExecutionResult{PlanDigest: whole.Digest, Completed: true}, nil
	}
	for name, legacy := range map[string][]string{
		"routine":   {"--reviewed-routine-digest", "sha256:" + strings.Repeat("a", 64)},
		"allowlist": {"--extension-allowlist", "pgcrypto"},
		"version":   {"--extension-version", "pgcrypto=1.3"},
		"schema":    {"--extension-schema", "pgcrypto=app"},
	} {
		stdout.Reset()
		stderr.Reset()
		args := []string{"database", "bootstrap", "--file", path, "--maintenance-url", "env://AUTOSQL_HCL_NEVER_RESOLVE", "--json"}
		args = append(args, legacy...)
		if code := Run(context.Background(), args, Streams{Out: &stdout, Err: &stderr}); code != int(ExitUsage) || executed || !strings.Contains(stdout.String()+stderr.String(), "legacy routine/extension") {
			t.Fatalf("%s conflict code=%d executed=%v stdout=%s stderr=%s", name, code, executed, stdout.String(), stderr.String())
		}
	}
	t.Setenv("AUTOSQL_HCL_MAINTENANCE", "postgres://operator:hcl-runtime-password@db.internal/postgres")
	stdout.Reset()
	stderr.Reset()
	code = Run(context.Background(), []string{"database", "bootstrap", "--file", path, "--maintenance-url", "env://AUTOSQL_HCL_MAINTENANCE", "--json"}, Streams{Out: &stdout, Err: &stderr})
	if code != 0 || !executed || strings.Contains(stdout.String()+stderr.String(), "hcl-runtime-password") || strings.Contains(stdout.String()+stderr.String(), base64.RawStdEncoding.EncodeToString(public)) {
		t.Fatalf("execute code=%d executed=%v stdout=%s stderr=%s", code, executed, stdout.String(), stderr.String())
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
	directory := t.TempDir()
	path := filepath.Join(directory, "bootstrap-live.hcl")
	manifestPath := filepath.Join(directory, "bootstrap-authorization.json")
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
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_CLI_BOOTSTRAP_AUTH_PRIVATE", base64.RawStdEncoding.EncodeToString(private))
	t.Setenv("AUTOSQL_CLI_BOOTSTRAP_AUTH_PUBLIC", base64.RawStdEncoding.EncodeToString(public))
	var stdout, stderr bytes.Buffer
	code := Run(ctx, []string{"database", "bootstrap", "authorize", "--file", path, "--authorization-signing-key", "env://AUTOSQL_CLI_BOOTSTRAP_AUTH_PRIVATE", "--authorization-key-id", "live-key", "--authorization-issuer", "integration", "--authorization-signer", "live-dba", "--output", manifestPath, "--json"}, Streams{Out: &stdout, Err: &stderr})
	if code != 0 {
		t.Fatalf("authorize code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	stdout.Reset()
	stderr.Reset()
	code = Run(ctx, []string{"database", "bootstrap", "--file", path, "--maintenance-url", "env://AUTOSQL_CLI_BOOTSTRAP_URL", "--authorization-manifest", manifestPath, "--authorization-public-key", "env://AUTOSQL_CLI_BOOTSTRAP_AUTH_PUBLIC", "--authorization-issuer", "integration", "--authorization-signer", "live-dba", "--json"}, Streams{Out: &stdout, Err: &stderr})
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
