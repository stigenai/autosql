package cli

import (
	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/migrate"
	migrateapply "autosql/pkg/migrate/apply"
	migratedown "autosql/pkg/migrate/down"
	"autosql/pkg/migrate/revision"
	"autosql/pkg/policy"
	"autosql/pkg/postgres"
	"autosql/pkg/safety"
	"autosql/pkg/schema"
	"autosql/pkg/simulate"
	"autosql/pkg/source"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

type downIntegrationAuthority struct{ at, expires time.Time }

func (a downIntegrationAuthority) ResolveActor(_ context.Context, id string) (approval.Identity, error) {
	if id == "author" || id == "requester" {
		return approval.Identity{ID: id}, nil
	}
	return approval.Identity{}, errors.New("untrusted actor")
}
func (a downIntegrationAuthority) VerifyApproval(_ context.Context, p approval.Approval) (approval.VerifiedApproval, error) {
	if p.Proof != "generation-proof" || p.Approver != "reviewer" {
		return approval.VerifiedApproval{}, errors.New("untrusted approval")
	}
	return approval.VerifiedApproval{Identity: approval.Identity{ID: "reviewer", Roles: []string{"reviewer"}}, PlanDigest: p.PlanDigest, Environment: p.Environment, ApprovedAt: a.at, ExpiresAt: a.expires}, nil
}

func proofDigest(value string) string {
	sum := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(sum[:])
}

// TestProductionDownRoundTrip is deliberately monolithic: it exercises the
// shipped production services and one live PostgreSQL database from ordinary
// forward apply through down publication/apply and ordinary forward reapply.
func TestProductionDownRoundTrip(t *testing.T) {
	devBase, prodBase := os.Getenv("AUTOSQL_DOWN_DEV_DSN"), os.Getenv("AUTOSQL_DOWN_PROD_DSN")
	if devBase == "" || prodBase == "" {
		t.Skip("controlled-down PostgreSQL URLs are not configured")
	}
	ctx := context.Background()
	prod := freshProductionDatabase(t, ctx, prodBase, "down_roundtrip")
	dir, artifacts := t.TempDir(), t.TempDir()
	if err := os.Chmod(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(artifacts, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Update(dir, migrate.UpdateRequest{ManifestVersion: migrate.ManifestVersion}); err != nil {
		t.Fatal(err)
	}
	devID, err := simulate.ResolvePostgresIdentity(ctx, devBase)
	if err != nil {
		t.Fatal(err)
	}
	prodID, err := simulate.ResolvePostgresIdentity(ctx, prod)
	if err != nil {
		t.Fatal(err)
	}
	_, generator, _ := ed25519.GenerateKey(rand.Reader)
	_, release, _ := ed25519.GenerateKey(rand.Reader)
	_, downPlanKey, _ := ed25519.GenerateKey(rand.Reader)
	_, downGenerator, _ := ed25519.GenerateKey(rand.Reader)
	_, downRelease, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC().Add(-time.Minute)
	expires := now.Add(45 * time.Minute)
	authority := downIntegrationAuthority{at: now, expires: expires}
	desired := func(sql string) schema.Document {
		doc, loadErr := source.LoadContext(ctx, source.Input{URI: "desired.sql", Format: source.FormatSQL, Data: []byte(sql)})
		if loadErr != nil {
			t.Fatal(loadErr)
		}
		return doc
	}
	request := migrate.GenerateRequest{Directory: dir, Version: "1", Label: "create_widgets", Format: "sql", RenameHints: "{}", Desired: desired("CREATE SCHEMA app; CREATE TABLE app.widgets (id bigint, name text NOT NULL);"), DevelopmentURL: devBase, DevelopmentIdentity: devID, ProductionIdentity: prodID, Environment: "production", DatabaseIdentity: prodID, SourceRevision: "roundtrip-release", Author: "author", Requester: "requester", PostgresVersion: 16, Policy: policy.Document{Version: policy.LanguageVersion, Rules: []policy.Rule{{Name: "allow", Target: "all", Assert: policy.Expression{Eq: []any{true, true}}, Message: "allowed"}}}, PolicyIdentity: "roundtrip-policy/v1", ApprovalPolicy: approval.Policy{Environments: map[string]approval.EnvironmentPolicy{"production": {Allowed: true}}}, Authority: authority, ApprovalAudit: &approval.Chain{Sink: &approval.FileSink{Path: filepath.Join(dir, "generation-approval.jsonl")}}, Approvals: []approval.Approval{{Approver: "reviewer", ApprovedAt: now, ExpiresAt: expires, Proof: "generation-proof"}}, CreatedAt: now, ExpiresAt: expires, GeneratorKeyID: "generator", GeneratorPurpose: "migration-generator", SigningKeyID: "release", GeneratorPrivateKey: generator, SigningPrivateKey: release}
	if _, err = (migrate.GenerateService{}).Generate(ctx, request); err != nil {
		t.Fatal(err)
	}
	request.Version, request.Label = "2", "add_names_view"
	request.Desired = desired("CREATE SCHEMA app; CREATE TABLE app.widgets (id bigint, name text NOT NULL); CREATE VIEW app.widget_names AS SELECT name FROM app.widgets;")
	request.CreatedAt = request.CreatedAt.Add(time.Second)
	if _, err = (migrate.GenerateService{}).Generate(ctx, request); err != nil {
		t.Fatal(err)
	}
	snap, err := migrate.LoadSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	trusted := map[string]migrationTrust{}
	for _, entry := range snap.Manifest.Entries {
		a, parseErr := artifact.Parse(snap.Files[entry.ArtifactFile])
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		contexts := map[string]string{}
		attestations := map[string]artifact.ValidationAttestation{}
		for _, attestation := range a.ValidationAttestations {
			contexts[attestation.Stage] = attestation.ConfigDigest
			attestations[attestation.Stage] = attestation
		}
		trusted[a.Digest] = migrationTrust{Expected: artifact.ExpectedBindings{PlanDigest: a.Plan.Digest, GeneratedPlanDigest: a.Plan.Digest, ChecksDigest: a.Checks.Digest, GuardrailDigest: a.GuardrailDigest, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: a.DatabaseIdentity, ApprovalIdentity: a.Approval.Identity, ApprovalProofDigest: a.Approval.ProofDigest}, ValidationContextDigests: contexts, ValidationAttestations: attestations}
	}
	revisionSchema := "autosql_down_roundtrip_" + strings.ToLower(hex.EncodeToString([]byte(time.Now().Format("150405"))))
	t.Setenv("AUTOSQL_DOWN_ROUNDTRIP_PROD", prod)
	t.Setenv("AUTOSQL_DOWN_ROUNDTRIP_DEV", devBase)
	t.Setenv("AUTOSQL_DOWN_PLAN_KEY", base64.RawStdEncoding.EncodeToString(downPlanKey))
	t.Setenv("AUTOSQL_DOWN_GENERATOR_KEY", base64.RawStdEncoding.EncodeToString(downGenerator))
	t.Setenv("AUTOSQL_DOWN_RELEASE_KEY", base64.RawStdEncoding.EncodeToString(downRelease))
	downApprovalAt := time.Now().UTC().Add(-time.Second)
	downConfig := downConfig{MigrationDirectory: dir, RevisionSchema: revisionSchema, DevelopmentURLReference: "env://AUTOSQL_DOWN_ROUNDTRIP_DEV", DevelopmentIdentity: devID, PlanSigningKeyReference: "env://AUTOSQL_DOWN_PLAN_KEY", PlanSigningKeyID: "down-plan", ArtifactDirectory: artifacts, Operator: "operator", ReleaseKeyReference: "env://AUTOSQL_DOWN_RELEASE_KEY", ReleaseKeyID: "down-release", GeneratorKeyReference: "env://AUTOSQL_DOWN_GENERATOR_KEY", GeneratorKeyID: "down-generator", GeneratorPurpose: "down-generator", Issuer: "down-issuer", Signer: "down-signer", Purpose: "down-release", PlanTTL: 10 * time.Minute, ApprovalPolicy: approval.Policy{Environments: map[string]approval.EnvironmentPolicy{"production": {Allowed: true}}}, Actors: map[string]approval.Identity{"author": {ID: "author"}, "requester": {ID: "requester"}, "down-reviewer": {ID: "down-reviewer", Roles: []string{"reviewer"}}}, VerifiedApprovals: map[string]approval.VerifiedApproval{"down-proof": {Identity: approval.Identity{ID: "down-reviewer", Roles: []string{"reviewer"}}, ApprovedAt: downApprovalAt, ExpiresAt: downApprovalAt.Add(time.Hour)}}, Approvals: []approval.Approval{{Approver: "down-reviewer", ApprovedAt: downApprovalAt, ExpiresAt: downApprovalAt.Add(time.Hour), Proof: "down-proof"}}, ApprovalAuditPath: filepath.Join(artifacts, "down-approval.jsonl"), SafetySuppressions: []safety.Suppression{{Rule: safety.RuleDropObject, ObjectID: "view:875bcb274b427397d7c60eb3", Reason: "approved controlled reversal"}, {Rule: safety.RuleDropObject, ObjectID: "column:c250ef5d3d0277c26d87f7d8", Reason: "approved controlled reversal"}}}
	downRaw, _ := json.Marshal(downConfig)
	downPath := filepath.Join(artifacts, "down.json")
	if err = os.WriteFile(downPath, downRaw, 0600); err != nil {
		t.Fatal(err)
	}
	applyCfg := applyConfig{DatabaseURL: "env://AUTOSQL_DOWN_ROUNDTRIP_PROD", Environment: "production", DatabaseIdentity: prodID, SourceRevision: "roundtrip-release", KeyID: "release", PublicKey: base64.RawStdEncoding.EncodeToString(release.Public().(ed25519.PublicKey)), Issuer: "release-issuer", Signer: "release-signer", Author: "author", Requester: "requester", ApprovalAuditPath: filepath.Join(artifacts, "apply-approval.jsonl"), LifecycleAuditPath: filepath.Join(artifacts, "lifecycle.jsonl"), ArtifactDirectory: artifacts, PostgresVersion: 16, Schemas: []string{"app"}, KeyStatus: "active", KeyPurpose: "release", KeyNotBefore: now.Add(-time.Hour), KeyNotAfter: expires.Add(time.Hour), NoEdits: true, GeneratorKeyID: "generator", GeneratorPublicKey: base64.RawStdEncoding.EncodeToString(generator.Public().(ed25519.PublicKey)), GeneratorPurpose: "migration-generator", DownConfigPath: downPath, TrustedMigrations: trusted}
	applyRaw, _ := json.Marshal(applyCfg)
	applyPath := filepath.Join(artifacts, "apply.json")
	if err = os.WriteFile(applyPath, applyRaw, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_APPLY_CONFIG", applyPath)
	services, err := ProductionServices()
	if err != nil {
		t.Fatal(err)
	}
	store, _ := revision.Open(revision.Config{URL: prod, Schema: revisionSchema})
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	resolved := services.Apply.(resolvingApply)
	forward := func(ctx context.Context, v artifact.VerifiedArtifact, locked executor.Session, tx executor.Tx, attempt int) (executor.ExternalExecution, error) {
		a, _ := v.Payload()
		x, newErr := executor.NewPostgreSQL(executor.Config{LockedSession: locked, LockAlreadyHeld: true, Transaction: tx, Attempt: attempt, Audit: resolved.verified.LifecycleAudit, Reauthorize: func(context.Context, artifact.Artifact) error { return nil }, State: func(ctx context.Context, session executor.Session) (executor.RuntimeState, error) {
			var doc schema.Document
			var inspectErr error
			if rawTx := executor.RawPGXTx(tx); rawTx != nil {
				doc, inspectErr = postgres.InspectTx(ctx, rawTx, postgres.Options{Schemas: []string{"app"}})
			} else {
				doc, inspectErr = postgres.InspectConn(ctx, session.Raw(), postgres.Options{Schemas: []string{"app"}})
			}
			if inspectErr != nil {
				return executor.RuntimeState{}, inspectErr
			}
			doc, inspectErr = postgres.New().Normalize(ctx, doc)
			if inspectErr != nil {
				return executor.RuntimeState{}, inspectErr
			}
			fingerprint, inspectErr := schema.SemanticFingerprint(doc)
			return executor.RuntimeState{Fingerprint: fingerprint, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: a.DatabaseIdentity}, inspectErr
		}}, v)
		if newErr != nil {
			return executor.ExternalExecution{}, newErr
		}
		_, applyErr := x.ApplyAuthorized(ctx, a.Checks)
		return x.ExternalExecution(), applyErr
	}
	engine := migrateapply.Engine{Store: store, Verify: resolved.VerifyArtifact, Apply: func(ctx context.Context, v artifact.VerifiedArtifact, s executor.Session, tx executor.Tx) (executor.ExternalExecution, error) {
		return forward(ctx, v, s, tx, 1)
	}, ApplyAttempt: forward, Drain: resolved.DrainLifecycle}
	applyRequest := migrateapply.Request{Directory: dir, Operator: "operator", TargetIdentity: revisionSchema, Transaction: "file"}
	if result, applyErr := engine.Run(ctx, applyRequest); applyErr != nil || result.Status != "applied" {
		t.Fatalf("initial apply=%+v err=%v", result, applyErr)
	}
	downService := services.Down.(*productionDownService)
	downPlan, err := downService.PlanDown(ctx, "1")
	if err != nil {
		t.Fatalf("%v impacts=%+v", err, downPlan.Impacts)
	}
	published, err := os.ReadFile(downPlan.ArtifactPath)
	publishedArtifact, parseErr := artifact.Parse(published)
	if err != nil || parseErr != nil || publishedArtifact.Digest != downPlan.ArtifactDigest {
		t.Fatalf("published artifact digest mismatch err=%v", err)
	}
	probe, err := pgx.Connect(ctx, prod)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = probe.Exec(ctx, `create table app.stale_probe(id int)`); err != nil {
		t.Fatal(err)
	}
	if status, staleErr := downService.ApplyDown(ctx, downPlan); !errors.Is(staleErr, migratedown.ErrStale) || status != "refused" {
		t.Fatalf("stale live schema accepted status=%s err=%v", status, staleErr)
	}
	if _, err = probe.Exec(ctx, `drop table app.stale_probe`); err != nil {
		t.Fatal(err)
	}
	_ = probe.Close(ctx)
	tampered := append([]byte(nil), published...)
	tampered[len(tampered)/2] ^= 1
	if err = os.WriteFile(downPlan.ArtifactPath, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if status, tamperErr := downService.ApplyDown(ctx, downPlan); tamperErr == nil || status != "refused" {
		t.Fatalf("tampered exact artifact accepted status=%s err=%v", status, tamperErr)
	}
	if err = os.WriteFile(downPlan.ArtifactPath, published, 0600); err != nil {
		t.Fatal(err)
	}
	if status, applyErr := downService.ApplyDown(ctx, downPlan); applyErr != nil || status != "reversed" {
		lifecycle, _ := os.ReadFile(applyCfg.LifecycleAuditPath)
		t.Fatalf("down status=%s err=%v lifecycle=%s", status, applyErr, lifecycle)
	}
	reversed, err := store.Status(ctx, snap.Manifest)
	if err != nil || reversed.Counts["reversed"] != 1 || reversed.Counts["reversal"] != 1 {
		t.Fatalf("reversed status=%+v err=%v", reversed, err)
	}
	if result, applyErr := engine.Run(ctx, applyRequest); applyErr != nil || result.Status != "applied" {
		diagnosticSession, _ := store.OpenSession(ctx)
		diagnosticRevisions, _ := diagnosticSession.Revisions(ctx)
		diagnosticHistory, _ := diagnosticSession.ExecutorRecords(ctx, prodID+"/production")
		_ = diagnosticSession.Close(ctx)
		original, _ := artifact.Parse(snap.Files[snap.Manifest.Entries[1].ArtifactFile])
		t.Fatalf("reapply=%+v err=%v plan=%+v revisions=%+v history=%+v", result, applyErr, original.Plan, diagnosticRevisions, diagnosticHistory)
	}
	applied, err := store.Status(ctx, snap.Manifest)
	if err != nil || applied.Counts["applied"] != 2 || applied.Counts["reapply"] != 1 || applied.Counts["reversed"] != 0 || applied.Dirty {
		t.Fatalf("reapplied status=%+v err=%v", applied, err)
	}
	session, err := store.OpenSession(ctx)
	if err != nil {
		t.Fatal(err)
	}
	revisions, _ := session.Revisions(ctx)
	history, _ := session.ExecutorRecords(ctx, prodID+"/production")
	_ = session.Close(ctx)
	if len(revisions) != 4 || revisions[2].Kind != "reversal" || revisions[2].ToVersion != "1.0.0" || revisions[2].ReversalOf == "" || revisions[3].Kind != "reapply" || revisions[3].ReversalOf != revisions[2].Version || revisions[3].Attempt != 2 {
		t.Fatalf("revision links=%+v", revisions)
	}
	attempts := map[int]int{}
	for _, row := range history {
		if row.PlanDigest == snap.Manifest.Entries[1].Directives.PlanDigest && row.State == "confirmed" {
			attempts[row.Attempt]++
		}
	}
	if attempts[1] == 0 || attempts[2] == 0 {
		t.Fatalf("attempt-scoped executor evidence=%+v", history)
	}
	lifecycle, err := os.ReadFile(applyCfg.LifecycleAuditPath)
	if err != nil || !strings.Contains(string(lifecycle), `"Attempt":1`) || !strings.Contains(string(lifecycle), `"Attempt":2`) {
		t.Fatalf("attempt-scoped lifecycle audit missing: %v %s", err, lifecycle)
	}
}
