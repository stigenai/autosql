package apply

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/migrate"
	"autosql/pkg/migrate/revision"
	"autosql/pkg/policy"
	"autosql/pkg/simulate"
	"autosql/pkg/source"
	"github.com/jackc/pgx/v5"
)

type liveAuthority struct{ at, expires time.Time }

func (a liveAuthority) ResolveActor(_ context.Context, id string) (approval.Identity, error) {
	if id == "author" || id == "requester" {
		return approval.Identity{ID: id}, nil
	}
	return approval.Identity{}, errors.New("untrusted")
}
func (a liveAuthority) VerifyApproval(_ context.Context, v approval.Approval) (approval.VerifiedApproval, error) {
	if v.Approver != "reviewer" || v.Proof != "proof" {
		return approval.VerifiedApproval{}, errors.New("bad approval")
	}
	return approval.VerifiedApproval{Identity: approval.Identity{ID: "reviewer", Roles: []string{"reviewer"}}, PlanDigest: v.PlanDigest, Environment: v.Environment, ApprovedAt: a.at, ExpiresAt: a.expires}, nil
}

func liveGenerated(t *testing.T, dev, prod string) (string, VerifyArtifact) {
	t.Helper()
	dir := t.TempDir()
	if _, e := migrate.Update(dir, migrate.UpdateRequest{ManifestVersion: migrate.ManifestVersion}); e != nil {
		t.Fatal(e)
	}
	doc, e := source.LoadContext(context.Background(), source.Input{URI: "desired.sql", Format: source.FormatSQL, Data: []byte("CREATE SCHEMA app; CREATE TABLE app.widgets (id bigint);")})
	if e != nil {
		t.Fatal(e)
	}
	_, gen, _ := ed25519.GenerateKey(rand.Reader)
	_, sign, _ := ed25519.GenerateKey(rand.Reader)
	devID, e := simulate.ResolvePostgresIdentity(context.Background(), dev)
	if e != nil {
		t.Fatal(e)
	}
	prodID, e := simulate.ResolvePostgresIdentity(context.Background(), prod)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC().Add(-time.Minute)
	expires := now.Add(time.Hour)
	auth := liveAuthority{now, expires}
	r := migrate.GenerateRequest{Directory: dir, Version: "1", Label: "widgets", Format: "sql", RenameHints: "{}", Desired: doc, DevelopmentURL: dev, DevelopmentIdentity: devID, ProductionIdentity: prodID, Environment: "test", DatabaseIdentity: prodID, SourceRevision: "rev", Author: "author", Requester: "requester", PostgresVersion: 16, Policy: policy.Document{Version: policy.LanguageVersion, Rules: []policy.Rule{{Name: "allow", Target: "all", Assert: policy.Expression{Eq: []any{true, true}}, Message: "ok"}}}, PolicyIdentity: "test/v1", ApprovalPolicy: approval.Policy{Environments: map[string]approval.EnvironmentPolicy{"test": {Allowed: true, Requirements: []approval.Requirement{{MinimumRisk: approval.RiskLow, ApproverCount: 1, Roles: []string{"reviewer"}}}}}}, Authority: auth, ApprovalAudit: &approval.Chain{Sink: &approval.FileSink{Path: dir + ".audit"}}, Approvals: []approval.Approval{{Approver: "reviewer", ApprovedAt: now, ExpiresAt: expires, Proof: "proof"}}, CreatedAt: now, ExpiresAt: expires, GeneratorKeyID: "gen", GeneratorPurpose: "migration-generator", SigningKeyID: "release", GeneratorPrivateKey: gen, SigningPrivateKey: sign}
	got, e := (migrate.GenerateService{}).Generate(context.Background(), r)
	if e != nil {
		t.Fatal(e)
	}
	snap, e := migrate.LoadSnapshot(dir)
	if e != nil {
		t.Fatal(e)
	}
	a, e := artifact.Parse(snap.Files[got.ArtifactFile])
	if e != nil {
		t.Fatal(e)
	}
	contexts := map[string]string{}
	atts := map[string]artifact.ValidationAttestation{}
	for _, x := range a.ValidationAttestations {
		contexts[x.Stage] = x.ConfigDigest
		atts[x.Stage] = x
	}
	vp := artifact.VerifyPolicy{Now: time.Now, NoEdits: true, Expected: artifact.ExpectedBindings{PlanDigest: a.Plan.Digest, GeneratedPlanDigest: a.Plan.Digest, ChecksDigest: a.Checks.Digest, GuardrailDigest: a.GuardrailDigest, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: a.DatabaseIdentity, ApprovalIdentity: a.Approval.Identity, ApprovalProofDigest: a.Approval.ProofDigest}, Keys: map[string]artifact.KeyRecord{"release": {PublicKey: sign.Public().(ed25519.PublicKey), Issuer: "issuer", Identity: "signer", Environment: "test", Purpose: "release", Status: "active", NotBefore: now.Add(-time.Hour), NotAfter: expires.Add(time.Hour)}}, Issuer: "issuer", Identity: "signer", Purpose: "release", GeneratorKeys: map[string]artifact.KeyRecord{"gen": {PublicKey: gen.Public().(ed25519.PublicKey), Purpose: "migration-generator"}}, GeneratorPurpose: "migration-generator", ExpectedValidationContextDigests: contexts, ExpectedValidationAttestations: atts}
	return dir, func(x artifact.Artifact) (artifact.VerifiedArtifact, error) { return x.VerifyTrusted(vp) }
}

func TestLiveGenerationApplyRetryBaselineAndAtomicEvidence(t *testing.T) {
	dev, prod := os.Getenv("AUTOSQL_GENERATE_TEST_DSN"), os.Getenv("AUTOSQL_GENERATE_PROD_TEST_DSN")
	if dev == "" || prod == "" {
		t.Skip("live generation databases unset")
	}
	dir, verify := liveGenerated(t, dev, prod)
	clean, e := pgx.Connect(context.Background(), prod)
	if e != nil {
		t.Fatal(e)
	}
	_, _ = clean.Exec(context.Background(), `drop schema if exists app cascade`)
	_ = clean.Close(context.Background())
	schema := "autosql_apply_live_" + strings.ToLower(strings.ReplaceAll(time.Now().Format("150405.000000"), ".", ""))
	store, e := revision.Open(revision.Config{URL: prod, Schema: schema})
	if e != nil {
		t.Fatal(e)
	}
	if e = store.Init(context.Background()); e != nil {
		t.Fatal(e)
	}
	engine := Engine{Store: store, Verify: verify}
	one := 1
	dry, e := engine.Run(context.Background(), Request{Directory: dir, Operator: "operator", TargetIdentity: schema, Transaction: "file", Count: &one, DryRun: true})
	if e != nil || dry.Status != "dry_run" || len(dry.Files) != 1 {
		t.Fatalf("dry=%+v err=%v", dry, e)
	}
	req := Request{Directory: dir, From: "1", To: "1", Operator: "operator", TargetIdentity: schema, Transaction: "file"}
	got, e := engine.Run(context.Background(), req)
	if e != nil || got.Status != "applied" || got.Statements == 0 || got.FileResults[0].Duration < 0 {
		t.Fatalf("got=%+v err=%v", got, e)
	}
	retryReq := req
	retryReq.From = ""
	retryReq.To = ""
	retry, e := engine.Run(context.Background(), retryReq)
	if e != nil || retry.Status != "no_op" {
		t.Fatalf("retry=%+v err=%v", retry, e)
	}
	// A second pinned session cannot pass selection while the target lock is held.
	held, e := store.OpenSession(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	ok, e := held.Lock(context.Background(), "autosql/migrate/target")
	if e != nil || !ok {
		t.Fatal(e)
	}
	_, e = engine.Run(context.Background(), Request{Directory: dir, Operator: "operator", TargetIdentity: "contended", Transaction: "file"})
	if !errors.Is(e, ErrBusy) {
		t.Fatalf("contention=%v", e)
	}
	_ = held.Unlock(context.Background(), "autosql/migrate/target")
	_ = held.Close(context.Background())
	// The all-in-one mode uses one transaction for both DDL and evidence.
	c, e := pgx.Connect(context.Background(), prod)
	if e != nil {
		t.Fatal(e)
	}
	_, _ = c.Exec(context.Background(), `drop schema if exists app cascade`)
	_, _ = c.Exec(context.Background(), `drop table if exists autosql_migration_history`)
	_ = c.Close(context.Background())
	allSchema := schema + "a"
	all, e := revision.Open(revision.Config{URL: prod, Schema: allSchema})
	if e != nil {
		t.Fatal(e)
	}
	if e = all.Init(context.Background()); e != nil {
		t.Fatal(e)
	}
	atomic, e := (Engine{Store: all, Verify: verify}).Run(context.Background(), Request{Directory: dir, Operator: "operator", TargetIdentity: allSchema, Transaction: "all"})
	if e != nil || atomic.Status != "applied" {
		t.Fatalf("atomic=%+v err=%v", atomic, e)
	}
	// A DDL fault rolls back the pending revision and reports the exact first
	// statement position; retry cannot mistake an uncommitted row for progress.
	faultSchema := schema + "f"
	fault, e := revision.Open(revision.Config{URL: prod, Schema: faultSchema})
	if e != nil {
		t.Fatal(e)
	}
	if e = fault.Init(context.Background()); e != nil {
		t.Fatal(e)
	}
	bad, e := (Engine{Store: fault, Verify: verify}).Run(context.Background(), Request{Directory: dir, Operator: "operator", TargetIdentity: faultSchema, Transaction: "file"})
	if e == nil || bad.Failure == nil || bad.Failure.StatementPosition != 1 || bad.Failure.Line < 1 || bad.Failure.Column < 1 {
		t.Fatalf("fault=%+v err=%v", bad, e)
	}
	fs, e := fault.OpenSession(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	rows, e := fs.Revisions(context.Background())
	_ = fs.Close(context.Background())
	if e != nil || len(rows) != 0 {
		t.Fatalf("rolled-back rows=%+v err=%v", rows, e)
	}
	baseSchema := schema + "b"
	base, e := revision.Open(revision.Config{URL: prod, Schema: baseSchema})
	if e != nil {
		t.Fatal(e)
	}
	if e = base.Init(context.Background()); e != nil {
		t.Fatal(e)
	}
	b, e := (Engine{Store: base, Verify: verify}).Run(context.Background(), Request{Directory: dir, Operator: "operator", TargetIdentity: baseSchema, Transaction: "file", Baseline: true})
	if e != nil || b.Status != "baselined" || b.Statements != 0 {
		t.Fatalf("baseline=%+v err=%v", b, e)
	}
}
