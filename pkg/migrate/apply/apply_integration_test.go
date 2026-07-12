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
	"autosql/pkg/executor"
	"autosql/pkg/migrate"
	"autosql/pkg/migrate/revision"
	"autosql/pkg/policy"
	"autosql/pkg/simulate"
	"autosql/pkg/source"
	"github.com/jackc/pgx/v5"
)

type liveAuthority struct{ at, expires time.Time }
type replayAudit struct {
	fail bool
	seen map[string]int
}

func (a *replayAudit) AppendDurable(_ context.Context, e executor.LifecycleEvent) error {
	if a.fail && e.Type == "transaction_committed" {
		a.fail = false
		return errors.New("injected audit failure")
	}
	if a.seen == nil {
		a.seen = map[string]int{}
	}
	a.seen[e.EventID]++
	return nil
}

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

func liveGenerated(t *testing.T, dev, prod string) (string, VerifyArtifact, func()) {
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
	_, e = (migrate.GenerateService{}).Generate(context.Background(), r)
	if e != nil {
		t.Fatal(e)
	}
	snap, e := migrate.LoadSnapshot(dir)
	if e != nil {
		t.Fatal(e)
	}
	policies := map[string]artifact.VerifyPolicy{}
	addPolicies := func(snap migrate.Snapshot) {
		for _, me := range snap.Manifest.Entries {
			a, pe := artifact.Parse(snap.Files[me.ArtifactFile])
			if pe != nil {
				t.Fatal(pe)
			}
			contexts := map[string]string{}
			atts := map[string]artifact.ValidationAttestation{}
			for _, x := range a.ValidationAttestations {
				contexts[x.Stage] = x.ConfigDigest
				atts[x.Stage] = x
			}
			policies[a.Digest] = artifact.VerifyPolicy{Now: time.Now, NoEdits: true, Expected: artifact.ExpectedBindings{PlanDigest: a.Plan.Digest, GeneratedPlanDigest: a.Plan.Digest, ChecksDigest: a.Checks.Digest, GuardrailDigest: a.GuardrailDigest, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: a.DatabaseIdentity, ApprovalIdentity: a.Approval.Identity, ApprovalProofDigest: a.Approval.ProofDigest}, Keys: map[string]artifact.KeyRecord{"release": {PublicKey: sign.Public().(ed25519.PublicKey), Issuer: "issuer", Identity: "signer", Environment: "test", Purpose: "release", Status: "active", NotBefore: now.Add(-time.Hour), NotAfter: expires.Add(time.Hour)}}, Issuer: "issuer", Identity: "signer", Purpose: "release", GeneratorKeys: map[string]artifact.KeyRecord{"gen": {PublicKey: gen.Public().(ed25519.PublicKey), Purpose: "migration-generator"}}, GeneratorPurpose: "migration-generator", ExpectedValidationContextDigests: contexts, ExpectedValidationAttestations: atts}
		}
	}
	addPolicies(snap)
	verify := func(x artifact.Artifact) (artifact.VerifiedArtifact, error) {
		p, ok := policies[x.Digest]
		if !ok {
			return artifact.VerifiedArtifact{}, errors.New("untrusted test artifact")
		}
		return x.VerifyTrusted(p)
	}
	appendSecond := func() {
		doc2, e := source.LoadContext(context.Background(), source.Input{URI: "desired2.sql", Format: source.FormatSQL, Data: []byte("CREATE SCHEMA app; CREATE TABLE app.widgets (id bigint); CREATE TABLE app.gadgets (id bigint);")})
		if e != nil {
			t.Fatal(e)
		}
		r.Version, r.Label, r.Desired = "2", "gadgets", doc2
		if _, e = (migrate.GenerateService{}).Generate(context.Background(), r); e != nil {
			t.Fatal(e)
		}
		next, e := migrate.LoadSnapshot(dir)
		if e != nil {
			t.Fatal(e)
		}
		addPolicies(next)
	}
	return dir, verify, appendSecond
}

func TestCoordinatorOutboxPartialDrainTwoFileAllInOneIsIdempotent(t *testing.T) {
	dev, prod := os.Getenv("AUTOSQL_GENERATE_TEST_DSN"), os.Getenv("AUTOSQL_GENERATE_PROD_TEST_DSN")
	if dev == "" || prod == "" {
		t.Skip("live generation databases unset")
	}
	dir, verify, appendSecond := liveGenerated(t, dev, prod)
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
	var lifecycle executor.LifecycleAudit
	guarded := func(ctx context.Context, v artifact.VerifiedArtifact, s executor.Session, tx executor.Tx) (executor.ExternalExecution, error) {
		a, _ := v.Payload()
		x, e := executor.NewPostgreSQL(executor.Config{LockedSession: s, LockAlreadyHeld: true, Transaction: tx, Now: time.Now, Audit: lifecycle, Reauthorize: func(context.Context, artifact.Artifact) error { _, z := verify(a); return z }, State: func(context.Context, executor.Session) (executor.RuntimeState, error) {
			return executor.RuntimeState{Fingerprint: a.Plan.FromFingerprint, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: a.DatabaseIdentity}, nil
		}}, v)
		if e != nil {
			return executor.ExternalExecution{}, e
		}
		_, e = x.ApplyAuthorized(ctx, a.Checks)
		if tx != nil {
			return x.ExternalExecution(), e
		}
		return executor.ExternalExecution{Result: x.Result()}, e
	}
	engine.Apply = guarded
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
	appendSecond()
	retryReq := req
	retryReq.From = ""
	retryReq.To = ""
	retry, e := engine.Run(context.Background(), retryReq)
	if e != nil || retry.Status != "applied" || retry.FinalVersion != "2.0.0" {
		t.Fatalf("retry=%+v err=%v", retry, e)
	}
	no, e := engine.Run(context.Background(), retryReq)
	if e != nil || no.Status != "no_op" {
		t.Fatalf("no-op=%+v err=%v", no, e)
	}
	// A second pinned session cannot pass selection while the target lock is held.
	held, e := store.OpenSession(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	ls, _ := migrate.LoadSnapshot(dir)
	la, _ := artifact.Parse(ls.Files[ls.Manifest.Entries[0].ArtifactFile])
	lk, _ := executor.LockKey(la.DatabaseIdentity, la.TargetEnvironment)
	ok, e := held.Lock(context.Background(), lk)
	if e != nil || !ok {
		t.Fatal(e)
	}
	_, e = engine.Run(context.Background(), Request{Directory: dir, Operator: "operator", TargetIdentity: "contended", Transaction: "file"})
	if !errors.Is(e, ErrBusy) {
		t.Fatalf("contention=%v", e)
	}
	_ = held.Unlock(context.Background(), lk)
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
	ra := &replayAudit{fail: true}
	lifecycle = ra
	atomic, e := (Engine{Store: all, Verify: verify, Apply: guarded}).Run(context.Background(), Request{Directory: dir, Operator: "operator", TargetIdentity: allSchema, Transaction: "all"})
	if e == nil || atomic.Status != "partial_failure" {
		t.Fatalf("atomic=%+v err=%v", atomic, e)
	}
	repaired, e := (Engine{Store: all, Verify: verify, Apply: guarded, Drain: ra.AppendDurable}).Run(context.Background(), Request{Directory: dir, Operator: "operator", TargetIdentity: allSchema, Transaction: "all"})
	if e != nil || repaired.Status != "no_op" {
		t.Fatalf("outbox repair=%+v err=%v", repaired, e)
	}
	for id, n := range ra.seen {
		if id == "" || n != 1 {
			t.Fatalf("non-idempotent audit id=%q count=%d", id, n)
		}
	}
	lifecycle = nil
	clear, _ := pgx.Connect(context.Background(), prod)
	if clear != nil {
		_, _ = clear.Exec(context.Background(), `delete from autosql_migration_history`)
		_ = clear.Close(context.Background())
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
	bad, e := (Engine{Store: fault, Verify: verify, Apply: guarded}).Run(context.Background(), Request{Directory: dir, Operator: "operator", TargetIdentity: faultSchema, Transaction: "file"})
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
	bc, e := pgx.Connect(context.Background(), prod)
	if e != nil {
		t.Fatal(e)
	}
	_, _ = bc.Exec(context.Background(), `drop schema if exists app cascade; drop table if exists autosql_migration_history`)
	_ = bc.Close(context.Background())
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
	check, e := pgx.Connect(context.Background(), prod)
	if e != nil {
		t.Fatal(e)
	}
	defer check.Close(context.Background())
	var exists bool
	if e = check.QueryRow(context.Background(), `select exists(select 1 from pg_namespace where nspname='app')`).Scan(&exists); e != nil || exists {
		t.Fatalf("baseline executed SQL exists=%v err=%v", exists, e)
	}
	var history *string
	if e = check.QueryRow(context.Background(), `select to_regclass('public.autosql_migration_history')::text`).Scan(&history); e != nil || history != nil {
		t.Fatalf("baseline created executor history=%v err=%v", history, e)
	}
}

func TestCoordinatorManifestReplacementWhileAcquiringCanonicalLock(t *testing.T) {
	dev, prod := os.Getenv("AUTOSQL_GENERATE_TEST_DSN"), os.Getenv("AUTOSQL_GENERATE_PROD_TEST_DSN")
	if dev == "" || prod == "" {
		t.Skip("live generation databases unset")
	}
	dir, verify, appendSecond := liveGenerated(t, dev, prod)
	schema := "autosql_swap_" + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
	store, e := revision.Open(revision.Config{URL: prod, Schema: schema})
	if e != nil {
		t.Fatal(e)
	}
	if e = store.Init(context.Background()); e != nil {
		t.Fatal(e)
	}
	called := false
	_, e = (Engine{Store: store, Verify: verify}).Run(context.Background(), Request{Directory: dir, Operator: "op", Transaction: "file", DryRun: true, beforeLock: func() { called = true; appendSecond() }})
	if !called || !errors.Is(e, ErrRefused) {
		t.Fatalf("called=%v err=%v", called, e)
	}
	s, e := store.OpenSession(context.Background())
	if e != nil {
		t.Fatal(e)
	}
	rows, e := s.Revisions(context.Background())
	hist, he := s.ExecutorRecords(context.Background())
	_ = s.Close(context.Background())
	if e != nil || he != nil || len(rows) != 0 || len(hist) != 0 {
		t.Fatalf("rows=%v history=%v err=%v/%v", rows, hist, e, he)
	}
}

func TestCoordinatorDirectRevisionWriterRacesFileAllAndBaseline(t *testing.T) {
	dev, prod := os.Getenv("AUTOSQL_GENERATE_TEST_DSN"), os.Getenv("AUTOSQL_GENERATE_PROD_TEST_DSN")
	if dev == "" || prod == "" {
		t.Skip("live generation databases unset")
	}
	dir, verify, _ := liveGenerated(t, dev, prod)
	for _, mode := range []string{"file", "all", "baseline"} {
		t.Run(mode, func(t *testing.T) {
			schema := "autosql_race_" + mode + strings.ReplaceAll(time.Now().Format("150405.000000"), ".", "")
			store, _ := revision.Open(revision.Config{URL: prod, Schema: schema})
			if e := store.Init(context.Background()); e != nil {
				t.Fatal(e)
			}
			held, e := store.OpenSession(context.Background())
			if e != nil {
				t.Fatal(e)
			}
			ok, e := held.LockWriters(context.Background())
			if e != nil || !ok {
				t.Fatal(e)
			}
			done := make(chan error, 1)
			now := time.Now().UTC()
			go func() {
				done <- store.Insert(context.Background(), revision.Revision{Version: "9.0.0", Description: "race", Kind: "migration", FileName: "unknown.sql", FileDigest: "sha256:x", ManifestDigest: "sha256:y", ManifestGeneration: "race", State: "applied", Attempt: 1, Operator: "racer", StartedAt: now, UpdatedAt: now})
			}()
			select {
			case e := <-done:
				t.Fatalf("writer bypassed barrier: %v", e)
			case <-time.After(30 * time.Millisecond):
			}
			_ = held.UnlockWriters(context.Background())
			_ = held.Close(context.Background())
			if e := <-done; e != nil {
				t.Fatal(e)
			}
			req := Request{Directory: dir, Operator: "op", Transaction: map[bool]string{true: "all", false: "file"}[mode == "all"], DryRun: mode != "baseline", Baseline: mode == "baseline"}
			_, e = (Engine{Store: store, Verify: verify}).Run(context.Background(), req)
			if !errors.Is(e, ErrRefused) {
				t.Fatalf("race accepted: %v", e)
			}
		})
	}
}
