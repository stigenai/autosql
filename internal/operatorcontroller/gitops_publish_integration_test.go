package operatorcontroller

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/operator"
	"autosql/pkg/plan"
	"autosql/pkg/policy"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/source"

	"github.com/jackc/pgx/v5"
)

func TestGitOpsPublishBundleIsAcceptedAndAppliedByOperator(t *testing.T) {
	base := strings.TrimSpace(os.Getenv("AUTOSQL_TEST_POSTGRES_URL"))
	if base == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	binary := gitopsTestBinary(t)
	developmentURL, developmentName := gitopsTestDatabase(t, ctx, base, "dev")
	productionURL, productionName := gitopsTestDatabase(t, ctx, base, "target")
	directory := t.TempDir()
	desiredPath := filepath.Join(directory, "global.hcl")
	bootstrapDesiredPath := filepath.Join(directory, "global-bootstrap.hcl")
	routineDefinition := `CREATE OR REPLACE FUNCTION gitops_global.identity(value integer)
 RETURNS integer
 LANGUAGE plpgsql
 IMMUTABLE
 SET search_path TO 'pg_catalog', 'gitops_global', 'public'
AS $function$ BEGIN RETURN value; END $function$`
	routineDigest := sha256.Sum256([]byte(routineDefinition))
	desired := fmt.Sprintf(`database %q {
  mode = "managed"
  endpoint = { host = "postgres.internal", port = 5432, tls_mode = "require" }
  maintenance_database = "postgres"
  owner = "postgres"
  encoding = "UTF8"
  locale_provider = "libc"
  collation = "C"
  character_type = "C"
  template = "template0"
  tablespace = "pg_default"
  connection_limit = -1
  allow_connections = true
}
schema "gitops_global" {}
table "accounts" {
  schema = "gitops_global"
  column "id" {
    type = "bigint"
    nullable = false
    ordinal = 1
  }
}
resource "index" "accounts_id_idx" {
  schema = "gitops_global"
  parent = table_id("gitops_global", "accounts")
  spec_json = jsonencode({
    method = "btree"
    unique = false
    valid = true
    ready = true
    columns = ["id"]
    definition = "CREATE INDEX accounts_id_idx ON gitops_global.accounts USING btree (id)"
  })
  deps_json = jsonencode([
    contains(table_id("gitops_global", "accounts")),
    references(column_id("gitops_global", "accounts", "id")),
  ])
}
`, productionName)
	bootstrapDesired := desired + fmt.Sprintf(`
resource "extension" "hstore" {
  schema = "gitops_global"
  parent = schema_id("gitops_global")
  spec_json = jsonencode({
    version = "1.8"
    relocatable = true
    trusted = true
    superuser = true
    requires = []
    owner = ""
  })
  deps_json = jsonencode([contains(schema_id("gitops_global"))])
  annotations_json = jsonencode({ comment = "data type for storing sets of (key, value) pairs" })
}
resource "function" "identity(value integer)" {
  schema = "gitops_global"
  parent = schema_id("gitops_global")
  spec_json = jsonencode({
    name = "identity"
    identity_arguments = "value integer"
    arguments = "value integer"
    result = "integer"
    returns_set = false
    language = "plpgsql"
    volatility = "i"
    strict = false
    security_definer = false
    leakproof = false
    parallel = "u"
    cost = 100
    rows = 0
    configuration = ["search_path=pg_catalog, gitops_global, public"]
    owner = "postgres"
    definition = %q
    body_digest = "sha256:%x"
  })
  deps_json = jsonencode([contains(schema_id("gitops_global"))])
}
`, routineDefinition, routineDigest)
	if err := os.WriteFile(desiredPath, []byte(desired), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(bootstrapDesiredPath, []byte(bootstrapDesired), 0o600); err != nil {
		t.Fatal(err)
	}
	parsedDesired, err := source.LoadContext(ctx, source.Input{URI: desiredPath, Format: source.FormatHCLSource, Data: []byte(desired)})
	if err != nil {
		t.Fatal(err)
	}
	current, err := postgres.InspectURL(ctx, productionURL, postgres.Options{Schemas: []string{"gitops_global"}})
	if err != nil {
		t.Fatal(err)
	}
	var declaredTargetFound bool
	for _, resource := range parsedDesired.Graph.Resources {
		if resource.Kind == schema.KindDatabase {
			declaredTarget, targetErr := postgres.DatabaseTargetFromResource(resource)
			if targetErr != nil {
				t.Fatal(targetErr)
			}
			transition, planErr := postgres.PlanDatabaseTransition(ctx, declaredTarget, current, parsedDesired, plan.Options{Render: map[string]string{"postgres_version": "16", "concurrent_indexes": "true"}})
			if targetErr = planErr; targetErr != nil {
				t.Fatalf("database-block transition plan failed: %v", targetErr)
			}
			if len(transition.SchemaPlan.Steps) == 0 {
				t.Fatal("database-block transition produced no steps")
			}
			declaredTargetFound = true
		}
	}
	if !declaredTargetFound {
		t.Fatal("database block was not parsed")
	}
	policyPath := filepath.Join(directory, "policy.json")
	policyBytes, _ := json.Marshal(policy.Document{Version: policy.LanguageVersion, Rules: []policy.Rule{{Name: "allow gitops fixture", Target: "all", Assert: policy.Expression{Eq: []any{true, true}}, Message: "allowed"}}})
	if err := os.WriteFile(policyPath, policyBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	_, generator, _ := ed25519.GenerateKey(rand.Reader)
	_, release, _ := ed25519.GenerateKey(rand.Reader)
	_, automation, _ := ed25519.GenerateKey(rand.Reader)
	t.Setenv("AUTOSQL_GITOPS_DEV_URL", developmentURL)
	t.Setenv("AUTOSQL_GITOPS_PROD_URL", productionURL)
	t.Setenv("AUTOSQL_GITOPS_GENERATOR_KEY", base64.RawStdEncoding.EncodeToString(generator))
	t.Setenv("AUTOSQL_GITOPS_RELEASE_KEY", base64.RawStdEncoding.EncodeToString(release))
	t.Setenv("AUTOSQL_GITOPS_APPROVAL_KEY", base64.RawStdEncoding.EncodeToString(automation))
	bundleDir := filepath.Join(directory, "release")
	now := time.Now().UTC()
	configPath := filepath.Join(directory, "publish.json")
	config := map[string]any{
		"DevelopmentURL": "env://AUTOSQL_GITOPS_DEV_URL", "ProductionURL": "env://AUTOSQL_GITOPS_PROD_URL",
		"Environment": "production", "DatabaseIdentity": productionName, "Author": "schema-author", "Requester": "release-bot", "PostgresVersion": 16, "ConcurrentIndexes": true, "Schemas": []string{"gitops_global"},
		"PolicyFile": policyPath, "PolicyIdentity": "platform-policy/v1",
		"ApprovalPolicy":          approval.Policy{Environments: map[string]approval.EnvironmentPolicy{"production": {Allowed: true, Requirements: []approval.Requirement{{MinimumRisk: approval.RiskLow, ApproverCount: 1, Roles: []string{"release-automation"}}}}}},
		"AutomationApprovalKeyID": "ci-approval-1", "AutomationApprovalIdentity": "github-actions", "AutomationApprovalRoles": []string{"release-automation"}, "AutomationApprovalPrivateKeyReference": "env://AUTOSQL_GITOPS_APPROVAL_KEY", "ApprovalTTL": "10m", "ArtifactLifetime": "1h",
		"GenerationApprovalAuditPath": filepath.Join(directory, "generation", "approval.jsonl"),
		"ApprovalAuditPath":           filepath.Join(directory, "approval.jsonl"), "LifecycleAuditPath": filepath.Join(directory, "lifecycle.jsonl"),
		"GeneratorKeyID": "generator-1", "GeneratorPurpose": "operator-generator", "GeneratorPrivateKeyReference": "env://AUTOSQL_GITOPS_GENERATOR_KEY",
		"SigningKeyID": "release-1", "SigningIssuer": "platform-security", "SigningIdentity": "release-automation", "SigningPurpose": "operator-release", "SigningStatus": "active", "SigningPrivateKeyReference": "env://AUTOSQL_GITOPS_RELEASE_KEY", "SigningNotBefore": now.Add(-time.Hour), "SigningNotAfter": now.Add(24 * time.Hour),
		"OperatorArtifactDirectory": filepath.Join(bundleDir, "artifacts"),
	}
	configBytes, _ := json.Marshal(config)
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	stdout, stderr := runGitOpsTestBinary(t, ctx, binary, "operator", "artifact", "publish", "--file", desiredPath, "--config", configPath, "--output-dir", bundleDir, "--source-revision", "git:0123456789abcdef", "--json")
	var envelope struct {
		Data struct {
			ArtifactDigest    string `json:"artifact_digest"`
			PostgresVersion   int    `json:"postgres_version"`
			ConcurrentIndexes bool   `json:"concurrent_indexes"`
		} `json:"data"`
	}
	if err := json.Unmarshal(stdout, &envelope); err != nil || envelope.Data.ArtifactDigest == "" || envelope.Data.PostgresVersion != 16 || !envelope.Data.ConcurrentIndexes {
		t.Fatalf("publish output=%s stderr=%s err=%v", stdout, stderr, err)
	}
	artifactBytes, err := os.ReadFile(filepath.Join(bundleDir, "artifacts", envelope.Data.ArtifactDigest+".json"))
	if err != nil {
		t.Fatal(err)
	}
	published, err := artifact.Parse(artifactBytes)
	if err != nil {
		t.Fatal(err)
	}
	current, err = postgres.InspectURL(ctx, productionURL, postgres.Options{Schemas: []string{"gitops_global"}})
	if err != nil {
		t.Fatal(err)
	}
	inlineDesired, err := source.LoadContext(ctx, source.Input{URI: "operator:inline", Format: source.FormatHCLSource, Data: []byte(desired)})
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range inlineDesired.Graph.Resources {
		if resource.Kind == schema.KindDatabase {
			declaredTarget, _ := postgres.DatabaseTargetFromResource(resource)
			transition, transitionErr := postgres.PlanDatabaseTransition(ctx, declaredTarget, current, inlineDesired, plan.Options{Render: map[string]string{"postgres_version": "16", "concurrent_indexes": "true"}})
			if transitionErr != nil || transition.SchemaPlan.Digest != published.Plan.Digest {
				t.Fatalf("URI-dependent plan published=%s inline=%s err=%v", published.Plan.Digest, transition.SchemaPlan.Digest, transitionErr)
			}
		}
	}
	bootstrapBundleDir := filepath.Join(directory, "bootstrap-release")
	stdout, stderr = runGitOpsTestBinary(t, ctx, binary, "operator", "artifact", "publish", "--file", bootstrapDesiredPath, "--config", configPath, "--output-dir", bootstrapBundleDir, "--source-revision", "git:0123456789abcdef", "--bootstrap", "--json")
	var bootstrapEnvelope struct {
		Data struct {
			ArtifactDigest string `json:"artifact_digest"`
		} `json:"data"`
	}
	if err = json.Unmarshal(stdout, &bootstrapEnvelope); err != nil || bootstrapEnvelope.Data.ArtifactDigest == "" {
		t.Fatalf("bootstrap publish output=%s stderr=%s err=%v", stdout, stderr, err)
	}
	bootstrapBytes, err := os.ReadFile(filepath.Join(bootstrapBundleDir, "artifacts", bootstrapEnvelope.Data.ArtifactDigest+".json"))
	if err != nil {
		t.Fatal(err)
	}
	bootstrapArtifact, err := artifact.Parse(bootstrapBytes)
	if err != nil || bootstrapArtifact.Origin.Kind != "generated" || bootstrapArtifact.Metadata["autosql.operator.mode"] != "bootstrap" || len(bootstrapArtifact.Plan.Changes.Changes) == 0 {
		t.Fatalf("invalid bootstrap artifact changes=%d mode=%q err=%v", len(bootstrapArtifact.Plan.Changes.Changes), bootstrapArtifact.Metadata["autosql.operator.mode"], err)
	}
	t.Setenv("AUTOSQL_APPLY_CONFIG", filepath.Join(bundleDir, "apply-config.json"))
	t.Setenv("AUTOSQL_OPERATOR_ARTIFACT_DIR", filepath.Join(bundleDir, "artifacts"))
	resource := operator.Resource{Spec: operator.Spec{Kind: operator.Declarative, Source: operator.Source{Format: "hcl", Inline: desired}, ArtifactDigest: envelope.Data.ArtifactDigest, PostgresVersion: 16, ConcurrentIndexes: true}, ResolvedSource: desired, ResolvedDatabaseURL: productionURL}
	result, err := ArtifactApply(ctx, resource, envelope.Data.ArtifactDigest)
	if err != nil {
		t.Fatalf("operator apply result=%+v err=%v", result, err)
	}
	if result.Status != "success" && result.Status != "applied" {
		t.Fatalf("operator status=%+v", result)
	}
	document, err := postgres.InspectURL(ctx, productionURL, postgres.Options{Schemas: []string{"gitops_global"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(document.Graph.Resources) < 4 {
		t.Fatalf("operator did not apply complete schema: %d resources", len(document.Graph.Resources))
	}
	_ = developmentName
}

func gitopsTestBinary(t *testing.T) string {
	t.Helper()
	directory, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(directory, "..", ".."))
	binary := filepath.Join(t.TempDir(), "autosql")
	command := exec.Command("go", "build", "-o", binary, "./cmd/autosql")
	command.Dir = root
	if output, buildErr := command.CombinedOutput(); buildErr != nil {
		t.Fatalf("build autosql binary: %v\n%s", buildErr, output)
	}
	return binary
}

func runGitOpsTestBinary(t *testing.T, ctx context.Context, binary string, args ...string) ([]byte, []byte) {
	t.Helper()
	command := exec.CommandContext(ctx, binary, args...)
	var stdout, stderr bytes.Buffer
	command.Stdout, command.Stderr = &stdout, &stderr
	if err := command.Run(); err != nil {
		t.Fatalf("autosql %s: %v\nstdout=%s\nstderr=%s", strings.Join(args, " "), err, stdout.String(), stderr.String())
	}
	return stdout.Bytes(), stderr.Bytes()
}

func gitopsTestDatabase(t *testing.T, ctx context.Context, base, label string) (string, string) {
	t.Helper()
	parsed, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 6)
	if _, err = rand.Read(random); err != nil {
		t.Fatal(err)
	}
	name := "autosql_gitops_" + label + "_" + hex.EncodeToString(random)
	admin, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		_ = admin.Close(ctx)
		t.Fatal(err)
	}
	_ = admin.Close(ctx)
	t.Cleanup(func() {
		cleanup, connectErr := pgx.Connect(context.Background(), base)
		if connectErr != nil {
			return
		}
		_, _ = cleanup.Exec(context.Background(), `SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()`, name)
		_, _ = cleanup.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize())
		_ = cleanup.Close(context.Background())
	})
	copy := *parsed
	copy.Path = "/" + name
	return copy.String(), name
}
