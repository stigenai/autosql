package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/config"
	"autosql/pkg/migrate"
	"autosql/pkg/simulate"

	"github.com/jackc/pgx/v5"
)

type projectGenerationOutput struct {
	Data struct {
		Generation   migrate.GenerateResult     `json:"generation"`
		Manifest     config.InputManifest       `json:"input_manifest"`
		Verification projectReleaseVerification `json:"project_verification"`
		Evidence     migrate.GenerateEvidence   `json:"release_evidence"`
	} `json:"data"`
}

func projectLiveDatabase(t *testing.T, adminURL, kind string) string {
	t.Helper()
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Fatal("connect disposable test database")
	}
	name := fmt.Sprintf("autosql_project_%s_%x", kind, time.Now().UnixNano())
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE DATABASE "+quoted); err != nil {
		conn.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		if _, err := conn.Exec(cleanup, "DROP DATABASE "+quoted+" WITH (FORCE)"); err != nil {
			t.Errorf("drop disposable database: %v", err)
		}
		conn.Close(cleanup)
	})
	u, err := url.Parse(adminURL)
	if err != nil || u.Scheme != "postgres" && u.Scheme != "postgresql" {
		t.Fatal("AUTOSQL_PROJECT_TEST_DSN must be a disposable PostgreSQL URL")
	}
	u.Path = "/" + name
	return u.String()
}

func projectLiveGeneration(t *testing.T, dev, target string) (string, generationConfig, map[string]ed25519.PublicKey, projectGenerationOutput) {
	t.Helper()
	root, args, c := projectGenerationFixture(t)
	t.Setenv("PROJECT_TEST_TARGET", target)
	t.Setenv("PROJECT_TEST_DEVELOPMENT", dev)
	c.DatabaseIdentity, _ = simulate.ResolvePostgresIdentity(context.Background(), target)
	c.SourceRevision, c.Author, c.Requester = "git:project-live", "author", "requester"
	c.Lifetime, c.ApprovalAuditPath = "1h", filepath.Join(root, "generation.audit")
	c.GeneratorKeyID, c.GeneratorPurpose, c.SigningKeyID = "generator", "migration", "release"
	c.GeneratorPrivateKeyReference, c.SigningPrivateKeyReference = "env://PROJECT_GENERATOR_KEY", "env://PROJECT_RELEASE_KEY"
	c.AutomationApproval = &generationAutomationApproval{KeyID: "approval", Identity: "ci", Roles: []string{"release-automation"}, PrivateKeyReference: "env://PROJECT_APPROVAL_KEY", TTL: "10m"}
	c.ApprovalPolicy = approval.Policy{Environments: map[string]approval.EnvironmentPolicy{"test": {Allowed: true, Requirements: []approval.Requirement{{MinimumRisk: approval.RiskLow, ApproverCount: 1, Roles: []string{"release-automation"}}}}}}
	keys := map[string]ed25519.PublicKey{}
	for name, variable := range map[string]string{"generator": "PROJECT_GENERATOR_KEY", "release": "PROJECT_RELEASE_KEY", "approval": "PROJECT_APPROVAL_KEY"} {
		public, private, err := ed25519.GenerateKey(nil)
		if err != nil {
			t.Fatal(err)
		}
		keys[name] = public
		t.Setenv(variable, base64.RawStdEncoding.EncodeToString(private))
	}
	raw, _ := json.Marshal(c)
	trusted := writeProjectCLIFile(t, root, "generation.json", string(raw))
	code, out, stderr := invoke(t, []string{"migrate", "generate", "--config", args.project, "--version", args.version, "--label", args.label, "--generation-config", trusted, "--json"}, "", false, Services{})
	if code != 0 {
		t.Fatalf("project generation failed code=%d: %s %s", code, out, stderr)
	}
	var result projectGenerationOutput
	decoder := json.NewDecoder(strings.NewReader(out))
	decoder.UseNumber()
	if err := decoder.Decode(&result); err != nil {
		t.Fatal(err)
	}
	if result.Data.Generation.Status != "generated" || result.Data.Manifest.EffectiveInputDigest == "" || len(result.Data.Evidence.ApprovalEvidence) != 1 {
		t.Fatal("project generation omitted release bindings/evidence")
	}
	for _, variable := range []string{"PROJECT_TEST_TARGET", "PROJECT_TEST_DEVELOPMENT", "PROJECT_GENERATOR_KEY", "PROJECT_RELEASE_KEY", "PROJECT_APPROVAL_KEY"} {
		if strings.Contains(out+stderr, os.Getenv(variable)) {
			t.Fatal("project generation disclosed a resolved secret")
		}
	}
	return root, c, keys, result
}

func projectLiveApplyConfig(t *testing.T, root string, generation generationConfig, keys map[string]ed25519.PublicKey, result projectGenerationOutput) (applyConfig, artifact.Artifact, string) {
	t.Helper()
	snapshot, err := migrate.LoadSnapshot(filepath.Join(root, "migrations"))
	if err != nil {
		t.Fatal(err)
	}
	raw := snapshot.Files[result.Data.Generation.ArtifactFile]
	a, err := artifact.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := captureProjectRelease(context.Background(), result.Data.Verification)
	if err != nil {
		t.Fatal(err)
	}
	defer inputs.close()
	identity := approval.Identity{ID: "ci", Roles: generation.AutomationApproval.Roles}
	trust := migrationTrust{Project: &result.Data.Verification, Policy: inputs.Policy, PolicyIdentity: generation.PolicyIdentity, Schemas: []string{"app"}, ApprovalIdentities: map[string]approval.Identity{a.Approval.ProofDigest: identity}, ApprovalEvidence: result.Data.Evidence.ApprovalEvidence, SchemaPolicyResources: result.Data.Evidence.SchemaPolicyResources, MigrationPolicyResources: result.Data.Evidence.MigrationPolicyResources, Expected: artifact.ExpectedBindings{PlanDigest: a.Plan.Digest, GeneratedPlanDigest: a.Plan.Digest, ChecksDigest: a.Checks.Digest, GuardrailDigest: a.GuardrailDigest, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: a.DatabaseIdentity, ApprovalIdentity: a.Approval.Identity, ApprovalProofDigest: a.Approval.ProofDigest}, ValidationContextDigests: map[string]string{}, ValidationAttestations: map[string]artifact.ValidationAttestation{}}
	for _, att := range a.ValidationAttestations {
		trust.ValidationContextDigests[att.Stage], trust.ValidationAttestations[att.Stage] = att.ConfigDigest, att
	}
	c := applyConfig{DatabaseURL: string(generation.ProductionURL), Environment: generation.Environment, DatabaseIdentity: generation.DatabaseIdentity, SourceRevision: generation.SourceRevision, Author: generation.Author, Requester: generation.Requester, PostgresVersion: generation.PostgresVersion, Schemas: trust.Schemas, KeyID: generation.SigningKeyID, PublicKey: base64.RawStdEncoding.EncodeToString(keys["release"]), Issuer: "test", Signer: "release", KeyStatus: "active", KeyPurpose: "release", KeyNotBefore: a.CreatedAt.Add(-time.Hour), KeyNotAfter: a.ExpiresAt.Add(time.Hour), GeneratorKeyID: generation.GeneratorKeyID, GeneratorPurpose: generation.GeneratorPurpose, GeneratorPublicKey: base64.RawStdEncoding.EncodeToString(keys["generator"]), AutomationApprovalKeyID: generation.AutomationApproval.KeyID, AutomationApprovalPublicKey: base64.RawStdEncoding.EncodeToString(keys["approval"]), NoEdits: true, ApprovalPolicy: generation.ApprovalPolicy, TrustedMigrations: map[string]migrationTrust{a.Digest: trust}, ApprovalAuditPath: filepath.Join(root, "apply.audit"), LifecycleAuditPath: filepath.Join(root, "lifecycle.audit")}
	return c, a, writeProjectCLIFile(t, root, "release.json", string(raw))
}

func TestProjectReleaseLiveGenerationApplyAndChangedInputRefusal(t *testing.T) {
	admin := os.Getenv("AUTOSQL_PROJECT_TEST_DSN")
	if admin == "" {
		t.Skip("AUTOSQL_PROJECT_TEST_DSN unset; requires a disposable PostgreSQL 17 admin URL")
	}
	dev, target := projectLiveDatabase(t, admin, "dev"), projectLiveDatabase(t, admin, "target")
	root, generation, keys, result := projectLiveGeneration(t, dev, target)
	c, a, artifactPath := projectLiveApplyConfig(t, root, generation, keys, result)
	raw, _ := json.Marshal(c)
	applyPath := writeProjectCLIFile(t, root, "apply.json", string(raw))
	t.Setenv("AUTOSQL_APPLY_CONFIG", applyPath)
	services, err := ProductionServices()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"schema.sql", "policy.json", "tests/first.test.hcl", "registry.lock"} {
		t.Run("reject changed "+name, func(t *testing.T) {
			path := filepath.Join(root, name)
			original, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			writeProjectCLIFile(t, root, name, string(original)+"\n")
			defer writeProjectCLIFile(t, root, name, string(original))
			if _, err := VerifyProductionArtifactForApply(a, true); err == nil {
				t.Fatal("changed current inputs still verified")
			}
			code, out, stderr := invoke(t, []string{"apply", "--artifact", artifactPath, "--no-edits", "--json"}, "", false, services)
			if code != int(ExitMigration) {
				t.Fatalf("changed inputs reached Apply: %d %s %s", code, out, stderr)
			}
		})
	}
	conn, err := pgx.Connect(context.Background(), target)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var count int
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM pg_namespace WHERE nspname='app'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("refused release mutated target: count=%d err=%v", count, err)
	}
	for _, p := range []string{c.ApprovalAuditPath, c.LifecycleAuditPath} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatal("refused release wrote mutation/approval evidence")
		}
	}
	// Rotating a reference to another empty database preserves file identities
	// but must never authorize that different runtime target.
	t.Setenv("PROJECT_TEST_TARGET", dev)
	wrongTargetServices, err := ProductionServices()
	if err != nil {
		t.Fatal(err)
	}
	code, out, stderr := invoke(t, []string{"apply", "--artifact", artifactPath, "--no-edits", "--json"}, "", false, wrongTargetServices)
	if code == 0 {
		t.Fatalf("rotated reference authorized a different target: %s %s", out, stderr)
	}
	wrong, err := pgx.Connect(context.Background(), dev)
	if err != nil {
		t.Fatal(err)
	}
	if err := wrong.QueryRow(context.Background(), "SELECT count(*) FROM pg_namespace WHERE nspname='app'").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rotated reference mutated wrong target: count=%d err=%v", count, err)
	}
	wrong.Close(context.Background())
	t.Setenv("PROJECT_TEST_TARGET", target)
	// A report destination is not an execution input.
	project, _ := os.ReadFile(filepath.Join(root, "project.json"))
	writeProjectCLIFile(t, root, "project.json", strings.Replace(string(project), "reports/a.xml", "reports/b.xml", 1))
	code, out, stderr = invoke(t, []string{"apply", "--artifact", artifactPath, "--no-edits", "--json"}, "", false, services)
	if code != 0 {
		t.Fatalf("valid project release failed Apply: %d %s %s", code, out, stderr)
	}
	if err := conn.QueryRow(context.Background(), "SELECT count(*) FROM pg_namespace WHERE nspname='app'").Scan(&count); err != nil || count != 1 {
		t.Fatalf("successful release did not create schema: count=%d err=%v", count, err)
	}
	if _, err := VerifyProductionArtifactForObservation(a, true); err != nil {
		t.Fatalf("applied current release cannot be observed: %v", err)
	}
}
