package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
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

type ambiguousConnector struct {
	executor.PGXConnector
	closed *bool
}

func (c ambiguousConnector) Connect(ctx context.Context, url string) (executor.Session, error) {
	s, e := (executor.PGXConnector{}).Connect(ctx, url)
	if e != nil {
		return nil, e
	}
	return ambiguousSession{Session: s, closed: c.closed}, nil
}

type ambiguousSession struct {
	executor.Session
	closed *bool
}

func (s ambiguousSession) Close(ctx context.Context) error {
	if s.closed != nil {
		*s.closed = true
	}
	return s.Session.Close(ctx)
}

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

type productionTargetSnapshot struct {
	Fingerprint, SchemaDefinition string
	HistoryRows                   int
}

func freshProductionDatabase(t *testing.T, ctx context.Context, baseURL, suffix string) string {
	t.Helper()
	cfg, err := pgx.ParseConfig(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 6)
	if _, err = rand.Read(random); err != nil {
		t.Fatal(err)
	}
	database := "autosql_cs5_" + suffix + "_" + hex.EncodeToString(random)
	adminCfg := cfg.Copy()
	adminCfg.Database = "postgres"
	admin, err := pgx.ConnectConfig(ctx, adminCfg)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, "create database "+pgx.Identifier{database}.Sanitize()); err != nil {
		_ = admin.Close(ctx)
		t.Fatal(err)
	}
	_ = admin.Close(ctx)
	t.Cleanup(func() {
		cleanup, connectErr := pgx.ConnectConfig(context.Background(), adminCfg)
		if connectErr != nil {
			return
		}
		_, _ = cleanup.Exec(context.Background(), `select pg_terminate_backend(pid) from pg_stat_activity where datname=$1 and pid <> pg_backend_pid()`, database)
		_, _ = cleanup.Exec(context.Background(), "drop database if exists "+pgx.Identifier{database}.Sanitize())
		_ = cleanup.Close(context.Background())
	})
	target, err := url.Parse(baseURL)
	if err != nil {
		t.Fatal(err)
	}
	target.Path = "/" + database
	return target.String()
}

func snapshotProductionTarget(t *testing.T, ctx context.Context, url, schemaName, digest string) productionTargetSnapshot {
	t.Helper()
	doc, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{schemaName}})
	if err != nil {
		t.Fatal(err)
	}
	fingerprint, err := schema.SemanticFingerprint(doc)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	var definition string
	err = conn.QueryRow(ctx, `select coalesce((select nspname from pg_namespace where nspname=$1),'')`, schemaName).Scan(&definition)
	if err != nil {
		t.Fatal(err)
	}
	var history int
	var historyTable *string
	if err = conn.QueryRow(ctx, `select to_regclass('autosql_migration_history')::text`).Scan(&historyTable); err != nil {
		t.Fatal(err)
	}
	if historyTable != nil {
		if err = conn.QueryRow(ctx, `select count(*) from autosql_migration_history where artifact_digest=$1`, digest).Scan(&history); err != nil {
			t.Fatal(err)
		}
	}
	return productionTargetSnapshot{Fingerprint: fingerprint, SchemaDefinition: definition, HistoryRows: history}
}

func assertExecutorLockAvailable(t *testing.T, ctx context.Context, url, databaseIdentity, environment string) {
	t.Helper()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	identity := fmt.Sprintf("%d:%s%d:%s", len(databaseIdentity), databaseIdentity, len(environment), environment)
	var locked bool
	if err = conn.QueryRow(ctx, `select pg_try_advisory_lock(hashtextextended($1, 0))`, identity).Scan(&locked); err != nil {
		_ = conn.Close(context.Background())
		t.Fatal(err)
	}
	if !locked {
		_ = conn.Close(context.Background())
		t.Fatalf("executor lock remains held for exact identity %q", identity)
	}
	var unlocked bool
	if err = conn.QueryRow(ctx, `select pg_advisory_unlock(hashtextextended($1, 0))`, identity).Scan(&unlocked); err != nil || !unlocked {
		_ = conn.Close(context.Background())
		t.Fatalf("release exact executor lock %q: unlocked=%v err=%v", identity, unlocked, err)
	}
	if err = conn.Close(ctx); err != nil {
		t.Fatalf("close exact executor lock probe %q: %v", identity, err)
	}
}

func fileSize(t *testing.T, path string) int64 {
	t.Helper()
	info, err := os.Stat(path)
	if os.IsNotExist(err) {
		return 0
	}
	if err != nil {
		t.Fatal(err)
	}
	return info.Size()
}

func assertTargetUnchanged(t *testing.T, before, after productionTargetSnapshot, lifecyclePath, approvalPath string, lifecycleBytes, approvalBytes int64) {
	t.Helper()
	if before != after {
		t.Fatalf("refused apply changed target: before=%+v after=%+v", before, after)
	}
	if got := fileSize(t, lifecyclePath); got != lifecycleBytes {
		t.Fatalf("refused apply changed lifecycle audit: before=%d after=%d", lifecycleBytes, got)
	}
	if got := fileSize(t, approvalPath); got != approvalBytes {
		t.Fatalf("refused apply changed approval audit: before=%d after=%d", approvalBytes, got)
	}
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
	schemaNonce := make([]byte, 6)
	if _, err = rand.Read(schemaNonce); err != nil {
		t.Fatal(err)
	}
	name := "autosql_prod_" + hex.EncodeToString(schemaNonce)
	cfg := applyConfig{DatabaseURL: "env://AUTOSQL_PROD_TEST_URL", Environment: "test/" + name, DatabaseIdentity: "prod-test/" + name, SourceRevision: "test-rev", KeyID: "test-key", PublicKey: base64.RawStdEncoding.EncodeToString(pub), Issuer: "test-issuer", Signer: "test-signer", Author: "author", Requester: "requester", ApprovalAuditPath: filepath.Join(dir, "approval.jsonl"), LifecycleAuditPath: filepath.Join(dir, "lifecycle.jsonl"), ArtifactDirectory: dir, PostgresVersion: 15, Schemas: []string{name}, ExpectedPlanDigest: "pending", ExpectedChecksDigest: "pending", ExpectedGuardrailDigest: "pending", ExpectedApprovalIdentity: "release", KeyStatus: "active", KeyPurpose: "plan-artifact", KeyNotBefore: time.Now().UTC().Add(-time.Hour), KeyNotAfter: time.Now().UTC().Add(2 * time.Hour), EditorIdentity: "editor", EditSigningKeyID: "edit-key", EditSigningKeyReference: "env://AUTOSQL_EDIT_TEST_KEY", DevelopmentURLReference: "env://AUTOSQL_DEV_TEST_URL", FreshApprovalIdentity: "fresh-approver", FreshApprovalProofDigest: "sha256:" + strings.Repeat("9", 64), FreshApprovalAt: releaseAt, EditReleaseCreatedAt: releaseAt, EditReleaseExpiresAt: releaseAt.Add(time.Hour)}
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
	baseEditService, ok := services.PlanEdit.(*productionEditService)
	if !ok {
		t.Fatal("production edit service is not injectable")
	}
	assertFailureOrder := func(label string, calls, order []string, failAt int) {
		t.Helper()
		prefix := order[:failAt+1]
		if len(calls) < len(prefix) {
			t.Fatalf("%s calls=%v want prefix=%v", label, calls, prefix)
		}
		for i := range prefix {
			if calls[i] != prefix[i] {
				t.Fatalf("%s calls=%v want prefix=%v", label, calls, prefix)
			}
		}
		for _, got := range calls[len(prefix):] {
			if got != "audit_edit_rejected" && got != "audit_edit_publish_rejected" {
				t.Fatalf("%s performed later work after %s: %v", label, order[failAt], calls)
			}
		}
	}
	failedService := func(fail string, calls *[]string) *productionEditService {
		t.Helper()
		copy := *baseEditService
		copy.stage = func(stage string) error {
			*calls = append(*calls, stage)
			if stage == fail {
				return errors.New("injected " + stage)
			}
			return nil
		}
		return &copy
	}
	assertNoPublishedSuffix := func(before int64) {
		t.Helper()
		after, readErr := os.ReadFile(cfg.LifecycleAuditPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if int64(len(after)) < before || strings.Contains(string(after[before:]), `"Type":"edit_published"`) {
			t.Fatalf("failed operation recorded publication: %s", after[before:])
		}
	}

	editOrder := []string{"original_verify", "atomic_create"}
	for failAt, fail := range editOrder {
		t.Run("edit_failure_"+fail, func(t *testing.T) {
			var calls []string
			output := filepath.Join(dir, "failed-edit-"+fail+".json")
			beforeAudit := fileSize(t, cfg.LifecycleAuditPath)
			code, _, _ := invoke(t, []string{"plan", "edit", "--artifact", path, "--sql", sqlPath, "--editor", "editor", "--reason", "failure matrix", "--output", output, "--json"}, "", false, Services{PlanEdit: failedService(fail, &calls)})
			if code == 0 || !os.IsNotExist(func() error { _, statErr := os.Stat(output); return statErr }()) {
				t.Fatalf("edit failure %s created output code=%d", fail, code)
			}
			assertFailureOrder("edit "+fail, calls, editOrder, failAt)
			assertNoPublishedSuffix(beforeAudit)
		})
	}

	revalidateOrder := []string{"audit_edit_requested", "original_verify", "target_inspect_stale", "identity_isolation", "parse", "ast_bind", "rebind", "simulation", "fingerprint", "safety", "policy", "precheck", "guardrail", "audit_edit_validated", "atomic_create"}
	for failAt, fail := range revalidateOrder {
		t.Run("revalidate_failure_"+fail, func(t *testing.T) {
			var calls []string
			output := filepath.Join(dir, "failed-revalidate-"+fail+".json")
			beforeAudit := fileSize(t, cfg.LifecycleAuditPath)
			beforeTarget := snapshotProductionTarget(t, ctx, url, name, a.Digest)
			code, _, _ := invoke(t, []string{"plan", "revalidate", "--draft", draftPath, "--output", output, "--json"}, "", false, Services{PlanEdit: failedService(fail, &calls)})
			if code == 0 || !os.IsNotExist(func() error { _, statErr := os.Stat(output); return statErr }()) {
				t.Fatalf("revalidate failure %s created output code=%d", fail, code)
			}
			assertFailureOrder("revalidate "+fail, calls, revalidateOrder, failAt)
			assertNoPublishedSuffix(beforeAudit)
			afterTarget := snapshotProductionTarget(t, ctx, url, name, a.Digest)
			if beforeTarget != afterTarget {
				t.Fatalf("revalidate failure %s mutated production: before=%+v after=%+v", fail, beforeTarget, afterTarget)
			}
			assertExecutorLockAvailable(t, ctx, url, cfg.DatabaseIdentity, cfg.Environment)
		})
	}

	publishOrder := []string{"audit_edit_publish_requested", "immediate_rerun", "audit_edit_requested", "original_verify", "target_inspect_stale", "identity_isolation", "parse", "ast_bind", "rebind", "simulation", "fingerprint", "safety", "policy", "precheck", "guardrail", "audit_edit_validated", "approval_freshness_proof", "sign", "atomic_create", "audit_edit_published"}
	for failAt, fail := range publishOrder {
		t.Run("publish_failure_"+fail, func(t *testing.T) {
			var calls []string
			output := filepath.Join(dir, "failed-publish-"+fail+".json")
			beforeAudit := fileSize(t, cfg.LifecycleAuditPath)
			beforeTarget := snapshotProductionTarget(t, ctx, url, name, a.Digest)
			code, _, _ := invoke(t, []string{"plan", "publish", "--attested", attestedPath, "--output", output, "--json"}, "", false, Services{PlanEdit: failedService(fail, &calls)})
			if code == 0 || !os.IsNotExist(func() error { _, statErr := os.Stat(output); return statErr }()) {
				t.Fatalf("publish failure %s left output code=%d", fail, code)
			}
			assertFailureOrder("publish "+fail, calls, publishOrder, failAt)
			assertNoPublishedSuffix(beforeAudit)
			afterTarget := snapshotProductionTarget(t, ctx, url, name, a.Digest)
			if beforeTarget != afterTarget {
				t.Fatalf("publish failure %s mutated production: before=%+v after=%+v", fail, beforeTarget, afterTarget)
			}
			assertExecutorLockAvailable(t, ctx, url, cfg.DatabaseIdentity, cfg.Environment)
		})
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
	// A valid revalidation cannot be published by replaying the original
	// approval identity, time, or proof.
	freshIdentity, freshAt, freshProof := cfg.FreshApprovalIdentity, cfg.FreshApprovalAt, cfg.FreshApprovalProofDigest
	cfg.FreshApprovalIdentity, cfg.FreshApprovalAt, cfg.FreshApprovalProofDigest = a.Approval.Identity, a.Approval.ApprovedAt, a.Approval.ProofDigest
	replayConfig, _ := json.Marshal(cfg)
	if err = os.WriteFile(configPath, replayConfig, 0600); err != nil {
		t.Fatal(err)
	}
	replayServices, replayErr := ProductionServices()
	if replayErr != nil {
		t.Fatal(replayErr)
	}
	replayOutput := filepath.Join(dir, "replayed-approval-must-not-publish.json")
	code, _, _ = invoke(t, []string{"plan", "publish", "--attested", attestedPath, "--output", replayOutput, "--json"}, "", false, replayServices)
	if code == 0 {
		t.Fatal("original approval replay published")
	}
	if _, statErr := os.Stat(replayOutput); !os.IsNotExist(statErr) {
		t.Fatalf("approval replay created output: %v", statErr)
	}
	cfg.FreshApprovalIdentity, cfg.FreshApprovalAt, cfg.FreshApprovalProofDigest = freshIdentity, freshAt, freshProof
	restoredConfig, _ := json.Marshal(cfg)
	if err = os.WriteFile(configPath, restoredConfig, 0600); err != nil {
		t.Fatal(err)
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
	jsonAmbiguousURL := freshProductionDatabase(t, ctx, url, "ambiguity_json")
	t.Setenv("AUTOSQL_PROD_TEST_URL", jsonAmbiguousURL)
	jsonClosed := false
	injected, err := productionServices(ambiguousConnector{closed: &jsonClosed})
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
	if !jsonClosed {
		t.Fatal("JSON ambiguity path did not close executor session")
	}
	assertExecutorLockAvailable(t, ctx, jsonAmbiguousURL, cfg.DatabaseIdentity, cfg.Environment)
	humanAmbiguousURL := freshProductionDatabase(t, ctx, url, "ambiguity_human")
	t.Setenv("AUTOSQL_PROD_TEST_URL", humanAmbiguousURL)
	humanClosed := false
	injected, err = productionServices(ambiguousConnector{closed: &humanClosed})
	if err != nil {
		t.Fatal(err)
	}
	code, _, human := invoke(t, []string{"apply", "--artifact", path}, "", false, injected)
	for _, want := range []string{"uncertain", lastStep, a.Digest, "reconcile transaction outcome"} {
		if code != int(ExitMigration) || !strings.Contains(human, want) || strings.Contains(human, "seeded-commit-secret") {
			t.Fatalf("human uncertain code=%d want=%s stderr=%s", code, want, human)
		}
	}
	if !humanClosed {
		t.Fatal("human ambiguity path did not close executor session")
	}
	assertExecutorLockAvailable(t, ctx, humanAmbiguousURL, cfg.DatabaseIdentity, cfg.Environment)
	t.Setenv("AUTOSQL_PROD_TEST_URL", url)
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
	// Install the edited release manifest. Every following apply scenario gets
	// its own database and audit files so prior execution cannot turn a real
	// first mutation into an accidental no-op or hide side effects.
	cfg.ExpectedPlanDigest, cfg.ExpectedChecksDigest, cfg.ExpectedGuardrailDigest = published.Plan.Digest, published.Checks.Digest, published.GuardrailDigest
	cfg.ExpectedApprovalIdentity, cfg.KeyID, cfg.PublicKey = published.Approval.Identity, "edit-key", base64.RawStdEncoding.EncodeToString(editPriv.Public().(ed25519.PublicKey))
	cfg.ExpectedApprovalProofDigest = published.Approval.ProofDigest
	cfg.ExpectedValidationContextDigests = map[string]string{}
	cfg.ExpectedValidationAttestations = map[string]artifact.ValidationAttestation{}
	for _, att := range published.EditProvenance.Attestations {
		cfg.ExpectedValidationContextDigests[att.Stage] = att.ConfigDigest
		cfg.ExpectedValidationAttestations[att.Stage] = att
	}
	if wait := time.Until(releaseAt); wait > 0 {
		time.Sleep(wait + 10*time.Millisecond)
	}
	if err = os.WriteFile(filepath.Join(dir, published.Plan.Digest+".json"), publishedRaw, 0600); err != nil {
		t.Fatal(err)
	}

	executableSteps := 0
	for _, step := range published.Plan.Steps {
		if step.Kind == plan.StepExecutable {
			executableSteps++
		}
	}
	applyEdited := func(mode, targetURL string) {
		t.Helper()
		cfg.NoEdits = false
		cfg.LifecycleAuditPath = filepath.Join(dir, "edited-"+mode+"-lifecycle.jsonl")
		cfg.ApprovalAuditPath = filepath.Join(dir, "edited-"+mode+"-approval.jsonl")
		raw, _ = json.Marshal(cfg)
		if err = os.WriteFile(configPath, raw, 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AUTOSQL_PROD_TEST_URL", targetURL)
		modeServices, serviceErr := ProductionServices()
		if serviceErr != nil {
			t.Fatal(serviceErr)
		}
		args := []string{"apply", "--artifact", publishedPath, "--json"}
		if mode == "digest" {
			modeServices.ReadPlan = &fakeRead{from: current, to: desired, p: published.Plan}
			args = []string{"apply", "--from", "from", "--to", "to", "--approve-digest", published.Plan.Digest, "--json"}
		}
		before := snapshotProductionTarget(t, ctx, targetURL, name, published.Digest)
		if before.Fingerprint != published.Plan.FromFingerprint || before.SchemaDefinition != "" || before.HistoryRows != 0 {
			t.Fatalf("%s target not fresh: %+v from=%s url=%s", mode, before, published.Plan.FromFingerprint, targetURL)
		}
		assertExecutorLockAvailable(t, ctx, targetURL, cfg.DatabaseIdentity, cfg.Environment)
		code, out, _ = invoke(t, args, "", false, modeServices)
		if code != 0 || strings.Contains(out, "no_op") || !strings.Contains(out, fmt.Sprintf(`"applied_steps":%d`, executableSteps)) {
			t.Fatalf("edited %s first apply code=%d out=%s", mode, code, out)
		}
		after := snapshotProductionTarget(t, ctx, targetURL, name, published.Digest)
		if after.SchemaDefinition != name || after.Fingerprint != published.Plan.ToFingerprint || after.HistoryRows != executableSteps {
			t.Fatalf("edited %s mutation evidence=%+v want fingerprint=%s history=%d", mode, after, published.Plan.ToFingerprint, executableSteps)
		}
		assertExecutorLockAvailable(t, ctx, targetURL, cfg.DatabaseIdentity, cfg.Environment)
		conn, connectErr := pgx.Connect(ctx, targetURL)
		if connectErr != nil {
			t.Fatal(connectErr)
		}
		var confirmed, exactSQL int
		if connectErr = conn.QueryRow(ctx, `select count(*), count(*) filter (where step_hash <> '' and plan_digest=$2 and bundle_digest=$3 and state='confirmed') from autosql_migration_history where artifact_digest=$1`, published.Digest, published.Plan.Digest, published.GuardrailDigest).Scan(&confirmed, &exactSQL); connectErr != nil {
			t.Fatal(connectErr)
		}
		_ = conn.Close(ctx)
		if confirmed != executableSteps || exactSQL != executableSteps {
			t.Fatalf("edited %s SQL/history not confirmed: rows=%d bound=%d", mode, confirmed, exactSQL)
		}
		lifecycle, readErr := os.ReadFile(cfg.LifecycleAuditPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		for _, event := range []string{"requested", "lock_acquired", "confirmed", "completed"} {
			if !strings.Contains(string(lifecycle), `"Type":"`+event+`"`) {
				t.Fatalf("edited %s lifecycle missing %s: %s", mode, event, lifecycle)
			}
		}
		for _, binding := range []string{published.Digest, published.Plan.Digest, published.GuardrailDigest} {
			if !strings.Contains(string(lifecycle), binding) {
				t.Fatalf("edited %s lifecycle missing binding %s", mode, binding)
			}
		}
		approvalAudit, readErr := os.ReadFile(cfg.ApprovalAuditPath)
		if readErr != nil || !strings.Contains(string(approvalAudit), published.GuardrailDigest) || !strings.Contains(string(approvalAudit), cfg.Requester) || !strings.Contains(string(approvalAudit), `"type":"apply_authorized"`) {
			t.Fatalf("edited %s approval audit missing release binding: err=%v audit=%s", mode, readErr, approvalAudit)
		}
		code, out, _ = invoke(t, args, "", false, modeServices)
		if code != 0 || !strings.Contains(out, "no_op") {
			t.Fatalf("edited %s second apply code=%d out=%s", mode, code, out)
		}
		second := snapshotProductionTarget(t, ctx, targetURL, name, published.Digest)
		if second != after {
			t.Fatalf("edited %s no-op changed target: first=%+v second=%+v", mode, after, second)
		}
		assertExecutorLockAvailable(t, ctx, targetURL, cfg.DatabaseIdentity, cfg.Environment)
	}

	artifactTarget := freshProductionDatabase(t, ctx, url, "artifact")
	digestTarget := freshProductionDatabase(t, ctx, url, "digest")
	applyEdited("artifact", artifactTarget)
	applyEdited("digest", digestTarget)

	assertRefused := func(label, mode, targetURL, artifactPath string, refusedPlan plan.Plan, requestNoEdits, configuredNoEdits bool) {
		t.Helper()
		cfg.NoEdits = configuredNoEdits
		cfg.LifecycleAuditPath = filepath.Join(dir, label+"-lifecycle.jsonl")
		cfg.ApprovalAuditPath = filepath.Join(dir, label+"-approval.jsonl")
		raw, _ = json.Marshal(cfg)
		if err = os.WriteFile(configPath, raw, 0600); err != nil {
			t.Fatal(err)
		}
		t.Setenv("AUTOSQL_PROD_TEST_URL", targetURL)
		refusingServices, serviceErr := ProductionServices()
		if serviceErr != nil {
			t.Fatal(serviceErr)
		}
		refusingServices.ReadPlan = &fakeRead{from: current, to: desired, p: refusedPlan}
		before := snapshotProductionTarget(t, ctx, targetURL, name, published.Digest)
		assertExecutorLockAvailable(t, ctx, targetURL, cfg.DatabaseIdentity, cfg.Environment)
		artifactBefore, readErr := os.ReadFile(artifactPath)
		if readErr != nil {
			t.Fatal(readErr)
		}
		parsedBefore, parseErr := artifact.Parse(artifactBefore)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		lifecycleBytes, approvalBytes := fileSize(t, cfg.LifecycleAuditPath), fileSize(t, cfg.ApprovalAuditPath)
		args := []string{"apply", "--artifact", artifactPath, "--json"}
		if mode == "digest" {
			args = []string{"apply", "--from", "from", "--to", "to", "--approve-digest", refusedPlan.Digest, "--json"}
		}
		if requestNoEdits {
			args = append(args[:len(args)-1], "--no-edits", "--json")
		}
		code, _, _ = invoke(t, args, "", false, refusingServices)
		if code == 0 {
			t.Fatalf("%s accepted refused artifact", label)
		}
		after := snapshotProductionTarget(t, ctx, targetURL, name, published.Digest)
		assertTargetUnchanged(t, before, after, cfg.LifecycleAuditPath, cfg.ApprovalAuditPath, lifecycleBytes, approvalBytes)
		assertExecutorLockAvailable(t, ctx, targetURL, cfg.DatabaseIdentity, cfg.Environment)
		artifactAfter, readErr := os.ReadFile(artifactPath)
		if readErr != nil || !bytes.Equal(artifactBefore, artifactAfter) {
			t.Fatalf("%s changed artifact bytes: err=%v", label, readErr)
		}
		parsedAfter, parseErr := artifact.Parse(artifactAfter)
		if parseErr != nil || parsedAfter.Digest != parsedBefore.Digest || parsedAfter.Signature != parsedBefore.Signature {
			t.Fatalf("%s changed artifact digest/signature: err=%v", label, parseErr)
		}
	}

	for _, mode := range []string{"artifact", "digest"} {
		assertRefused("request-no-edits-"+mode, mode, freshProductionDatabase(t, ctx, url, "request_no_edits_"+mode), publishedPath, published.Plan, true, false)
		assertRefused("configured-no-edits-"+mode, mode, freshProductionDatabase(t, ctx, url, "configured_no_edits_"+mode), publishedPath, published.Plan, false, true)
	}
	// Once the trusted manifest advances to the edited release, replaying the
	// originally approved artifact must fail before audit, lock, history, SQL,
	// schema, or fingerprint state changes on a fresh target.
	for _, mode := range []string{"artifact", "digest"} {
		assertRefused("original-approval-replay-"+mode, mode, freshProductionDatabase(t, ctx, url, "approval_replay_"+mode), path, p, false, false)
	}
}
