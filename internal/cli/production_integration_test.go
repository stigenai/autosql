package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/guardrail"
	"autosql/pkg/plan"
	"autosql/pkg/planedit"
	"autosql/pkg/postgres"
	"autosql/pkg/precheck"
	"autosql/pkg/schema"
	"github.com/jackc/pgx/v5"
)

type ambiguousConnector struct{ executor.PGXConnector }

func (ambiguousConnector) Connect(ctx context.Context, url string) (executor.Session, error) {
	s, e := (executor.PGXConnector{}).Connect(ctx, url)
	if e != nil {
		return nil, e
	}
	return ambiguousSession{Session: s}, nil
}

type ambiguousSession struct{ executor.Session }

func (s ambiguousSession) Begin(ctx context.Context) (executor.Tx, error) {
	tx, e := s.Session.Begin(ctx)
	if e != nil {
		return nil, e
	}
	return ambiguousTx{Tx: tx}, nil
}

type ambiguousTx struct{ executor.Tx }

func (t ambiguousTx) Commit(ctx context.Context) error {
	if e := t.Tx.Commit(ctx); e != nil {
		return e
	}
	return errors.New("commit password=seeded-commit-secret")
}

func TestProductionServicesVerifiedArtifactApplyAndNoOp(t *testing.T) {
	url := os.Getenv("AUTOSQL_PROD_TEST_URL")
	if url == "" {
		t.Skip("AUTOSQL_PROD_TEST_URL unset")
	}
	devURL := os.Getenv("AUTOSQL_DEV_TEST_URL")
	if devURL == "" {
		t.Skip("AUTOSQL_DEV_TEST_URL unset")
	}
	ctx := context.Background()
	_, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	pub := priv.Public().(ed25519.PublicKey)
	_, editPriv, _ := ed25519.GenerateKey(rand.Reader)
	releaseAt := time.Now().UTC().Add(5 * time.Second)
	dir := t.TempDir()
	name := "autosql_prod_" + strings.ToLower(time.Now().Format("150405000000"))
	cfg := applyConfig{DatabaseURL: "env://AUTOSQL_PROD_TEST_URL", Environment: "test", DatabaseIdentity: "prod-test", SourceRevision: "test-rev", KeyID: "test-key", PublicKey: base64.RawStdEncoding.EncodeToString(pub), Issuer: "test-issuer", Signer: "test-signer", Author: "author", Requester: "requester", ApprovalAuditPath: filepath.Join(dir, "approval.jsonl"), LifecycleAuditPath: filepath.Join(dir, "lifecycle.jsonl"), ArtifactDirectory: dir, PostgresVersion: 15, Schemas: []string{name}, ExpectedPlanDigest: "pending", ExpectedChecksDigest: "pending", ExpectedGuardrailDigest: "pending", ExpectedApprovalIdentity: "release", KeyStatus: "active", KeyPurpose: "plan-artifact", KeyNotBefore: time.Now().UTC().Add(-time.Hour), KeyNotAfter: time.Now().UTC().Add(2 * time.Hour), EditorIdentity: "editor", EditSigningKeyID: "edit-key", EditSigningKeyReference: "env://AUTOSQL_EDIT_TEST_KEY", DevelopmentURLReference: "env://AUTOSQL_DEV_TEST_URL", FreshApprovalIdentity: "fresh-approver", FreshApprovalProofDigest: "sha256:" + strings.Repeat("9", 64), FreshApprovalAt: releaseAt, EditReleaseCreatedAt: releaseAt, EditReleaseExpiresAt: releaseAt.Add(time.Hour)}
	raw, _ := json.Marshal(cfg)
	configPath := filepath.Join(dir, "apply.json")
	if err = os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_APPLY_CONFIG", configPath)
	t.Setenv("AUTOSQL_PROD_TEST_URL", url)
	t.Setenv("AUTOSQL_DEV_TEST_URL", devURL)
	t.Setenv("AUTOSQL_EDIT_TEST_KEY", base64.RawStdEncoding.EncodeToString(editPriv))
	services, err := ProductionServices()
	if err != nil {
		t.Fatal(err)
	}
	resolved := services.Apply.(resolvingApply)
	current := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	current, _ = postgres.New().Normalize(ctx, current)
	desired := current
	desired.Graph.Resources = append(append([]schema.Resource(nil), current.Graph.Resources...), schema.Resource{ID: schema.StableID(schema.KindSchema, schema.Name{Name: name}), Kind: schema.KindSchema, Name: schema.Name{Name: name}, Spec: []byte(`{}`)})
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	changeDigest, err := guardrail.ChangeDigest(p.Changes)
	if err != nil {
		t.Fatal(err)
	}
	var sql []string
	for _, s := range p.Steps {
		if s.Kind == plan.StepExecutable {
			sql = append(sql, s.SQL)
		}
	}
	checks := precheck.Plan{ID: "checks", ChangeDigest: changeDigest, Statements: sql}
	checks.Digest, err = precheck.Digest(checks)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	draft, err := artifact.New(p, checks, now, now.Add(time.Hour), cfg.SourceRevision, cfg.Environment, cfg.DatabaseIdentity, "sha256:"+strings.Repeat("0", 64), artifact.Approval{Identity: "release", ApprovedAt: now}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	in, err := resolved.verified.Input(draft)
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := resolved.verified.Guardrail.BundleDigest(in)
	if err != nil {
		t.Fatal(err)
	}
	a, err := artifact.New(p, checks, now, now.Add(time.Hour), cfg.SourceRevision, cfg.Environment, cfg.DatabaseIdentity, bundle, artifact.Approval{Identity: "release", ApprovedAt: now}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	if err = a.Sign(cfg.KeyID, priv); err != nil {
		t.Fatal(err)
	}
	cfg.ExpectedPlanDigest = p.Digest
	cfg.ExpectedChecksDigest = checks.Digest
	cfg.ExpectedGuardrailDigest = bundle
	raw, _ = json.Marshal(cfg)
	if err = os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	services, err = ProductionServices()
	if err != nil {
		t.Fatal(err)
	}
	artifactRaw, err := a.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "artifact.json")
	if err = os.WriteFile(path, artifactRaw, 0600); err != nil {
		t.Fatal(err)
	}
	sqlPath := filepath.Join(dir, "edited.sql")
	if err = os.WriteFile(sqlPath, []byte("  "+p.Steps[0].SQL), 0600); err != nil {
		t.Fatal(err)
	}
	draftPath := filepath.Join(dir, "draft.json")
	code, out, _ := invoke(t, []string{"plan", "edit", "--artifact", path, "--sql", sqlPath, "--editor", "editor", "--reason", "reviewed", "--output", draftPath, "--json"}, "", false, services)
	if code != 0 {
		t.Fatalf("edit code=%d out=%s", code, out)
	}
	attestedPath := filepath.Join(dir, "attested.json")
	code, out, _ = invoke(t, []string{"plan", "revalidate", "--draft", draftPath, "--output", attestedPath, "--json"}, "", false, services)
	if code != 0 {
		t.Fatalf("revalidate code=%d out=%s", code, out)
	}
	attestedRaw, err := os.ReadFile(attestedPath)
	if err != nil {
		t.Fatal(err)
	}
	var tampered planedit.Eligible
	if err = json.Unmarshal(attestedRaw, &tampered); err != nil {
		t.Fatal(err)
	}
	tampered.GuardrailDigest = "sha256:" + strings.Repeat("f", 64)
	tamperedRaw, _ := json.Marshal(tampered)
	tamperedPath := filepath.Join(dir, "tampered-attested.json")
	if err = os.WriteFile(tamperedPath, tamperedRaw, 0600); err != nil {
		t.Fatal(err)
	}
	tamperedOutput := filepath.Join(dir, "must-not-publish.json")
	code, _, _ = invoke(t, []string{"plan", "publish", "--attested", tamperedPath, "--output", tamperedOutput, "--json"}, "", false, services)
	if code == 0 {
		t.Fatal("tampered eligibility published")
	}
	if _, err = os.Stat(tamperedOutput); !os.IsNotExist(err) {
		t.Fatalf("tampered publish created output: %v", err)
	}
	publishedPath := filepath.Join(dir, "published.json")
	code, out, _ = invoke(t, []string{"plan", "publish", "--attested", attestedPath, "--output", publishedPath, "--json"}, "", false, services)
	if code != 0 {
		t.Fatalf("publish code=%d out=%s", code, out)
	}
	publishedRaw, err := os.ReadFile(publishedPath)
	if err != nil {
		t.Fatal(err)
	}
	published, err := artifact.Parse(publishedRaw)
	if err != nil || published.EditProvenance == nil {
		t.Fatalf("published provenance err=%v", err)
	}
	injected, err := productionServices(ambiguousConnector{})
	if err != nil {
		t.Fatal(err)
	}
	code, out, _ = invoke(t, []string{"apply", "--artifact", path, "--json"}, "", false, injected)
	lastStep := p.Steps[len(p.Steps)-1].ID
	for _, want := range []string{`"status":"uncertain"`, `"applied_steps":0`, `"pending_step":"` + lastStep + `"`, `"execution_id":"` + a.Digest + `"`, "reconcile transaction outcome"} {
		if code != int(ExitMigration) || !strings.Contains(out, want) || strings.Contains(out, "seeded-commit-secret") {
			t.Fatalf("json uncertain code=%d want=%s out=%s", code, want, out)
		}
	}
	reset, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = reset.Exec(ctx, "drop schema if exists "+pgx.Identifier{name}.Sanitize()+" cascade")
	_, _ = reset.Exec(ctx, "delete from autosql_migration_history where artifact_digest=$1", a.Digest)
	_ = reset.Close(ctx)
	injected, err = productionServices(ambiguousConnector{})
	if err != nil {
		t.Fatal(err)
	}
	code, _, human := invoke(t, []string{"apply", "--artifact", path}, "", false, injected)
	for _, want := range []string{"uncertain", lastStep, a.Digest, "reconcile transaction outcome"} {
		if code != int(ExitMigration) || !strings.Contains(human, want) || strings.Contains(human, "seeded-commit-secret") {
			t.Fatalf("human uncertain code=%d want=%s stderr=%s", code, want, human)
		}
	}
	reset, err = pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = reset.Exec(ctx, "drop schema if exists "+pgx.Identifier{name}.Sanitize()+" cascade")
	_, _ = reset.Exec(ctx, "delete from autosql_migration_history where artifact_digest=$1", a.Digest)
	_ = reset.Close(ctx)
	defer func() {
		conn, _ := pgx.Connect(ctx, url)
		if conn != nil {
			_, _ = conn.Exec(ctx, "drop schema if exists "+pgx.Identifier{name}.Sanitize()+" cascade")
			_ = conn.Close(ctx)
		}
	}()
	for i := 0; i < 2; i++ {
		code, out, _ := invoke(t, []string{"apply", "--artifact", path, "--json"}, "", false, services)
		if code != 0 {
			t.Fatalf("apply %d code=%d out=%s", i, code, out)
		}
		if i == 1 && !strings.Contains(out, "no_op") {
			t.Fatalf("second apply=%s", out)
		}
	}
	if err = os.WriteFile(filepath.Join(dir, p.Digest+".json"), artifactRaw, 0600); err != nil {
		t.Fatal(err)
	}
	services.ReadPlan = &fakeRead{from: current, to: desired, p: p}
	code, out, _ = invoke(t, []string{"apply", "--from", "from", "--to", "to", "--approve-digest", p.Digest, "--json"}, "", false, services)
	if code != 0 || !strings.Contains(out, "no_op") {
		t.Fatalf("digest apply code=%d out=%s", code, out)
	}
	auditRaw, err := os.ReadFile(cfg.LifecycleAuditPath)
	if err != nil {
		t.Fatal(err)
	}
	auditText := string(auditRaw)
	for _, event := range []string{"requested", "lock_acquired", "confirmed", "completed"} {
		if !strings.Contains(auditText, `"Type":"`+event+`"`) {
			t.Fatalf("missing lifecycle %s: %s", event, auditText)
		}
	}
	if !strings.Contains(auditText, a.Digest) || !strings.Contains(auditText, p.Digest) || !strings.Contains(auditText, bundle) {
		t.Fatal("lifecycle audit bindings missing")
	}
	// Install the edited release manifest and prove both artifact and digest
	// apply paths consume the freshly published artifact.
	cfg.ExpectedPlanDigest, cfg.ExpectedChecksDigest, cfg.ExpectedGuardrailDigest = published.Plan.Digest, published.Checks.Digest, published.GuardrailDigest
	cfg.ExpectedApprovalIdentity, cfg.KeyID, cfg.PublicKey = published.Approval.Identity, "edit-key", base64.RawStdEncoding.EncodeToString(editPriv.Public().(ed25519.PublicKey))
	cfg.ExpectedApprovalProofDigest = published.Approval.ProofDigest
	cfg.ExpectedValidationContextDigests = map[string]string{}
	cfg.ExpectedValidationAttestations = map[string]artifact.ValidationAttestation{}
	for _, att := range published.EditProvenance.Attestations {
		cfg.ExpectedValidationContextDigests[att.Stage] = att.ConfigDigest
		cfg.ExpectedValidationAttestations[att.Stage] = att
	}
	raw, _ = json.Marshal(cfg)
	if err = os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	editedServices, err := ProductionServices()
	if err != nil {
		t.Fatal(err)
	}
	if wait := time.Until(releaseAt); wait > 0 {
		time.Sleep(wait + 10*time.Millisecond)
	}
	code, out, _ = invoke(t, []string{"apply", "--artifact", publishedPath, "--json"}, "", false, editedServices)
	if code != 0 || !strings.Contains(out, "no_op") {
		t.Fatalf("edited artifact apply code=%d out=%s", code, out)
	}
	if err = os.WriteFile(filepath.Join(dir, published.Plan.Digest+".json"), publishedRaw, 0600); err != nil {
		t.Fatal(err)
	}
	editedServices.ReadPlan = &fakeRead{from: current, to: desired, p: published.Plan}
	code, out, _ = invoke(t, []string{"apply", "--from", "from", "--to", "to", "--approve-digest", published.Plan.Digest, "--json"}, "", false, editedServices)
	if code != 0 || !strings.Contains(out, "no_op") {
		t.Fatalf("edited digest apply code=%d out=%s", code, out)
	}
	code, _, _ = invoke(t, []string{"apply", "--artifact", publishedPath, "--no-edits", "--json"}, "", false, editedServices)
	if code == 0 {
		t.Fatal("request no-edits accepted edited artifact")
	}
	cfg.NoEdits = true
	raw, _ = json.Marshal(cfg)
	if err = os.WriteFile(configPath, raw, 0600); err != nil {
		t.Fatal(err)
	}
	noEditServices, err := ProductionServices()
	if err != nil {
		t.Fatal(err)
	}
	code, _, _ = invoke(t, []string{"apply", "--artifact", publishedPath, "--json"}, "", false, noEditServices)
	if code == 0 {
		t.Fatal("configured no-edits accepted edited artifact")
	}
}
