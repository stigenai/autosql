package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/config"
	"autosql/pkg/guardrail"
	"autosql/pkg/migrate"
	"autosql/pkg/precheck"
)

func projectGenerationFixture(t *testing.T) (string, trustedGenerateArgs, generationConfig) {
	t.Helper()
	root := t.TempDir()
	body := `{"version":2,"environment":"test","environments":{"test":{"target":"env://PROJECT_TEST_TARGET","schema_sources":[{"kind":"sql","path":"schema.sql"}],"migration_dir":"migrations","policy_files":["policy.json"],"tests":{"files":["tests/*.test.hcl"]},"registry":{"lock":"registry.lock"},"reporting":{"junit":"reports/a.xml"}}}}`
	path := writeProjectCLIFile(t, root, "project.json", body)
	writeProjectCLIFile(t, root, "schema.sql", "CREATE SCHEMA app;")
	writeProjectCLIFile(t, root, "policy.json", `{"version":"v1","rules":[{"name":"allow","target":"all","assert":{"eq":[true,true]},"message":"allowed"}]}`)
	writeProjectCLIFile(t, root, "tests/first.test.hcl", `test "first" {}`)
	writeProjectCLIFile(t, root, "registry.lock", `{"version":1}`)
	if err := os.Mkdir(filepath.Join(root, "migrations"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Update(filepath.Join(root, "migrations"), migrate.UpdateRequest{}); err != nil {
		t.Fatal(err)
	}
	args := trustedGenerateArgs{project: path, version: "1", label: "init", format: "sql"}
	trusted := generationConfig{Environment: "test", ProductionURL: "env://PROJECT_TEST_TARGET", DevelopmentURL: "env://PROJECT_TEST_DEVELOPMENT", PostgresVersion: 17, PolicyFile: "policy.json", PolicyIdentity: "test-policy"}
	return root, args, trusted
}

func TestProjectGenerationCapturesInputsBeforeCredentials(t *testing.T) {
	root, args, trusted := projectGenerationFixture(t)
	inputs, err := loadGenerationInputs(context.Background(), args, trusted, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(inputs.close)
	if inputs.Project == nil || inputs.Verification.MigrationPrefix == nil || *inputs.Verification.MigrationPrefix != 0 || inputs.Directory != filepath.Join(root, "migrations") || len(inputs.Desired.Graph.Resources) == 0 || len(inputs.Project.Manifest.Files) != 4 {
		t.Fatalf("incomplete project generation capture: %+v", inputs)
	}
	writeProjectCLIFile(t, root, "replacement.any", "CREATE SCHEMA replacement;")
	args.to = "sql:replacement.any"
	changed, err := loadGenerationInputs(context.Background(), args, trusted, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(changed.close)
	if len(changed.Verification.Overrides.SourceInputs) != 1 || changed.Project.Manifest.EffectiveInputDigest == inputs.Project.Manifest.EffectiveInputDigest || changed.Desired.Graph.Resources[0].Name.Name != "replacement" {
		t.Fatal("explicit desired source did not replace or bind configured input")
	}
	args.to = ""
	for _, mutate := range []func(*generationConfig){
		func(c *generationConfig) { c.Environment = "other" },
		func(c *generationConfig) { c.ProductionURL = "env://OTHER_TARGET" },
		func(c *generationConfig) { c.PolicyFile = "other.json" },
		func(c *generationConfig) { c.PostgresVersion = 99 },
	} {
		candidate := trusted
		mutate(&candidate)
		if _, err := loadGenerationInputs(context.Background(), args, candidate, nil); err == nil {
			t.Fatal("project and trusted generation mismatch accepted")
		}
	}
}

func TestProjectGenerationRefusesUnsupportedConfigurationBeforeSecrets(t *testing.T) {
	for name, mutate := range map[string]func(string, *trustedGenerateArgs, *generationConfig){
		"v1": func(root string, _ *trustedGenerateArgs, _ *generationConfig) {
			writeProjectCLIFile(t, root, "project.json", `{"version":1,"environment":"test","environments":{"test":{"target":"env://PROJECT_TEST_TARGET","schema_sources":[{"kind":"sql","path":"schema.sql"}],"migration_dir":"migrations"}}}`)
		},
		"filter": func(root string, _ *trustedGenerateArgs, _ *generationConfig) {
			raw, _ := os.ReadFile(filepath.Join(root, "project.json"))
			writeProjectCLIFile(t, root, "project.json", strings.Replace(string(raw), `"policy_files":`, `"include":["app.*"],"policy_files":`, 1))
		},
		"multiple policies": func(root string, _ *trustedGenerateArgs, _ *generationConfig) {
			raw, _ := os.ReadFile(filepath.Join(root, "project.json"))
			writeProjectCLIFile(t, root, "project.json", strings.Replace(string(raw), `["policy.json"]`, `["policy.json","other.json"]`, 1))
		},
		"unsafe label":        func(_ string, a *trustedGenerateArgs, _ *generationConfig) { a.label = "Not_valid" },
		"unknown environment": func(_ string, a *trustedGenerateArgs, _ *generationConfig) { a.environment = "missing" },
		"remote source":       func(_ string, a *trustedGenerateArgs, _ *generationConfig) { a.to = "live:env://SECRET" },
		"noncanonical hints":  func(_ string, a *trustedGenerateArgs, _ *generationConfig) { a.hints = `{"b":"x", "a":"y"}` },
	} {
		t.Run(name, func(t *testing.T) {
			root, args, trusted := projectGenerationFixture(t)
			mutate(root, &args, &trusted)
			inputs, err := loadGenerationInputs(context.Background(), args, trusted, nil)
			defer inputs.close()
			var cliErr *Error
			want := ExitConfig
			if name == "remote source" {
				want = ExitUsage
			}
			if !errors.As(err, &cliErr) || cliErr.Code != want {
				t.Fatalf("unsupported project configuration reached runtime: %v", err)
			}
		})
	}
	for _, args := range [][]string{
		{"migrate", "generate", "--generation-config", "missing.json", "--env", "test"},
		{"migrate", "generate", "--generation-config", "missing.json", "--config", "project.json", "--from", "sql:current.sql"},
	} {
		code, out, stderr := invoke(t, args, "", false, Services{})
		if code != int(ExitUsage) {
			t.Fatalf("ambiguous project flags accepted: %d %s %s", code, out, stderr)
		}
	}
}

type projectPublishedFixture struct {
	root, applyPath, artifactPath string
	artifact                      artifact.Artifact
	settings                      projectReleaseVerification
	captured                      capturedProjectRelease
	apply                         applyConfig
	file                          migrate.File
}

func publishedProjectFixture(t *testing.T) projectPublishedFixture {
	t.Helper()
	root, args, trusted := projectGenerationFixture(t)
	inputs, err := loadProjectGenerationInputs(context.Background(), args, trusted)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(inputs.close)
	_, p := workflowFixture(t)
	changes, _ := guardrail.ChangeDigest(p.Changes)
	checks := precheck.Plan{ID: "project-checks", ChangeDigest: changes}
	for _, step := range p.Steps {
		checks.Statements = append(checks.Statements, step.SQL)
	}
	checks.Digest, err = precheck.Digest(checks)
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	metadata, err := projectReleaseMetadata(nil, inputs.Project.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	a, err := artifact.NewGenerated(p, checks, now, now.Add(time.Hour), "git:test", "test", "db", "sha256:"+strings.Repeat("b", 64), artifact.Approval{Identity: "reviewer", ApprovedAt: now}, metadata, "generator", "migration", private)
	if err != nil {
		t.Fatal(err)
	}
	base := "sha256:" + strings.Repeat("c", 64)
	atts := []artifact.ValidationAttestation{
		{Stage: "replay_simulation", Simulation: &artifact.SimulationAttestation{TargetIdentity: "db", DevelopmentIdentity: "dev", FromFingerprint: p.FromFingerprint, ToFingerprint: p.ToFingerprint, DatabaseVersion: "17", ConfigDigest: base}},
		{Stage: "safety", Safety: &artifact.SafetyAttestation{Analyzers: []string{"compatibility"}, ConfigDigest: base}},
		{Stage: "policy_precheck_guardrail", Policy: &artifact.PolicyAttestation{ConfigDigest: base}, Precheck: &artifact.PrecheckGuardrailAttestation{ConfigDigest: base, ChecksDigest: checks.Digest, GuardrailDigest: a.GuardrailDigest}},
	}
	for index := range atts {
		atts[index].ConfigDigest, atts[index].ResultDigest = base, base
		atts[index].At, atts[index].ExpiresAt = now, a.ExpiresAt
		atts[index].Implementation, atts[index].Version = "test", "1"
	}
	if err := a.SetValidationAttestations(atts); err != nil {
		t.Fatal(err)
	}
	if err := a.BindProjectInputs(inputs.Project.Manifest.EffectiveInputDigest); err != nil {
		t.Fatal(err)
	}
	if err := a.Sign("release", private); err != nil {
		t.Fatal(err)
	}
	raw, _ := a.MarshalCanonical()
	file := migrate.File{Name: "V1__init.sql", SQL: []byte("CREATE SCHEMA app;"), ArtifactName: "V1__init.sql.artifact.json", Artifact: raw}
	if _, err := migrate.Update(inputs.Directory, migrate.UpdateRequest{ExpectedManifestDigest: inputs.Project.Manifest.Migrations.ManifestDigest, Files: []migrate.File{file}}); err != nil {
		t.Fatal(err)
	}
	trust := migrationTrust{Project: inputs.Verification, Policy: inputs.Policy, PolicyIdentity: trusted.PolicyIdentity, Expected: artifact.ExpectedBindings{PlanDigest: p.Digest, GeneratedPlanDigest: p.Digest, ChecksDigest: checks.Digest, GuardrailDigest: a.GuardrailDigest, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: a.DatabaseIdentity, ApprovalIdentity: a.Approval.Identity}, ValidationAttestations: map[string]artifact.ValidationAttestation{}, ValidationContextDigests: map[string]string{}}
	for _, att := range a.ValidationAttestations {
		trust.ValidationAttestations[att.Stage], trust.ValidationContextDigests[att.Stage] = att, att.ConfigDigest
	}
	cfg := applyConfig{DatabaseURL: "env://PROJECT_TEST_TARGET", Environment: "test", DatabaseIdentity: "db", SourceRevision: "git:test", PostgresVersion: 17, KeyID: "release", PublicKey: base64.RawStdEncoding.EncodeToString(public), Issuer: "issuer", Signer: "signer", KeyStatus: "active", KeyPurpose: "release", KeyNotBefore: now.Add(-time.Hour), KeyNotAfter: now.Add(2 * time.Hour), NoEdits: true, GeneratorKeyID: "generator", GeneratorPublicKey: base64.RawStdEncoding.EncodeToString(public), GeneratorPurpose: "migration", ApprovalAuditPath: filepath.Join(root, "approval.audit"), LifecycleAuditPath: filepath.Join(root, "lifecycle.audit"), TrustedMigrations: map[string]migrationTrust{a.Digest: trust}}
	configRaw, _ := json.Marshal(cfg)
	applyPath := writeProjectCLIFile(t, root, "apply.json", string(configRaw))
	artifactPath := writeProjectCLIFile(t, root, "release.json", string(raw))
	return projectPublishedFixture{root: root, applyPath: applyPath, artifactPath: artifactPath, artifact: a, settings: *inputs.Verification, captured: *inputs.Project, apply: cfg, file: file}
}

func TestProjectReleaseVerificationUsesTrustedPathAndCurrentInputs(t *testing.T) {
	fixture := publishedProjectFixture(t)
	t.Setenv("AUTOSQL_APPLY_CONFIG", fixture.applyPath)
	t.Setenv("PROJECT_TEST_TARGET", "")
	if _, err := VerifyProductionArtifactForApply(fixture.artifact, true); err != nil {
		t.Fatalf("current project release refused: %v", err)
	}
	if _, err := VerifyProductionArtifactForObservation(fixture.artifact, true); err != nil {
		t.Fatal(err)
	}
	services, err := productionVerificationServices()
	if err != nil {
		t.Fatal(err)
	}
	service := services.Apply.(resolvingApply).verified
	cached, err := service.PolicyFor(fixture.artifact)
	if err != nil {
		t.Fatal(err)
	}
	service.InstallPolicy(fixture.artifact.Digest, cached)
	writeProjectCLIFile(t, fixture.root, "schema.sql", "CREATE SCHEMA app; -- same SQL, different input bytes\n")
	called := false
	service.Mutation = func(artifact.VerifiedArtifact) (guardrail.AuthorizedMutation, error) {
		called = true
		return nil, errors.New("must not construct mutation")
	}
	result, err := service.Apply(context.Background(), ApplyRequest{ArtifactPath: fixture.artifactPath, ApprovalMode: "artifact", NoEdits: true})
	if err == nil || called || result.Status != "refused" {
		t.Fatalf("cached policy bypassed current input verification: %v called=%t result=%+v", err, called, result)
	}
	for _, name := range []string{fixture.apply.ApprovalAuditPath, fixture.apply.LifecycleAuditPath} {
		if _, err := os.Stat(name); !os.IsNotExist(err) {
			t.Fatalf("read-only verification wrote evidence: %v", err)
		}
	}
}

func TestProjectReleaseVerificationRejectsInputAndTrustEdits(t *testing.T) {
	for _, name := range []string{"schema.sql", "policy.json", "tests/first.test.hcl", "registry.lock"} {
		t.Run(name, func(t *testing.T) {
			fixture := publishedProjectFixture(t)
			path := filepath.Join(fixture.root, name)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := verifyCurrentProjectRelease(fixture.artifact, &fixture.settings, fixture.applyPath, 17, fixture.captured.Policy); err == nil {
				t.Fatal("changed current input verified")
			}
		})
	}
	fixture := publishedProjectFixture(t)
	for _, mutate := range []func(*projectReleaseVerification){
		func(s *projectReleaseVerification) { s.Path = "missing-project.json" },
		func(s *projectReleaseVerification) { s.MigrationPrefix = nil },
		func(s *projectReleaseVerification) { s.Execution.PostgresVersion = 16 },
		func(s *projectReleaseVerification) { s.Execution.RenameHints = `{"old":"new"}` },
		func(s *projectReleaseVerification) {
			s.Overrides.SourceInputs = []config.SchemaSource{{Kind: "sql", Path: "schema.sql"}}
		},
	} {
		settings := fixture.settings
		mutate(&settings)
		if _, err := verifyCurrentProjectRelease(fixture.artifact, &settings, fixture.applyPath, 17, fixture.captured.Policy); err == nil {
			t.Fatal("changed trusted execution settings verified")
		}
	}
	if _, err := verifyCurrentProjectRelease(fixture.artifact, nil, fixture.applyPath, 17, fixture.captured.Policy); err == nil {
		t.Fatal("artifact metadata supplied its own trusted project configuration")
	}
}

func TestProjectReleaseAllowsReportMoveAndLaterMigrationButRequiresPublishedArtifact(t *testing.T) {
	fixture := publishedProjectFixture(t)
	path := filepath.Join(fixture.root, "project.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(raw), "reports/a.xml", "reports/b.xml", 1)), 0600); err != nil {
		t.Fatal(err)
	}
	settings := fixture.settings
	settings.Path = "project.json"
	if _, err := verifyCurrentProjectRelease(fixture.artifact, &settings, fixture.applyPath, 17, fixture.captured.Policy); err != nil {
		t.Fatalf("report destination or trusted relative project path changed execution identity: %v", err)
	}
	dir := filepath.Join(fixture.root, "migrations")
	before, err := migrate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Update(dir, migrate.UpdateRequest{ExpectedManifestDigest: before.Digest, Files: []migrate.File{fixture.file, {Name: "V2__next.sql", SQL: []byte("SELECT 2;")}}}); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyCurrentProjectRelease(fixture.artifact, &settings, fixture.applyPath, 17, fixture.captured.Policy); err != nil {
		t.Fatalf("later migration changed the release's preceding input snapshot: %v", err)
	}
	after, err := migrate.Load(dir)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Update(dir, migrate.UpdateRequest{ExpectedManifestDigest: after.Digest}); err != nil {
		t.Fatal(err)
	}
	if _, err := verifyCurrentProjectRelease(fixture.artifact, &settings, fixture.applyPath, 17, fixture.captured.Policy); err == nil {
		t.Fatal("project release absent from current publication was accepted")
	}
}
