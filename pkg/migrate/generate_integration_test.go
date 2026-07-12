package migrate

import (
	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/policy"
	"autosql/pkg/precheck"
	"autosql/pkg/schema"
	"autosql/pkg/simulate"
	"autosql/pkg/source"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"golang.org/x/sys/unix"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type generationTestAuthority struct{ at, expires time.Time }

func (a generationTestAuthority) ResolveActor(_ context.Context, id string) (approval.Identity, error) {
	if id == "author" || id == "requester" {
		return approval.Identity{ID: id}, nil
	}
	return approval.Identity{}, errors.New("untrusted actor")
}
func (a generationTestAuthority) VerifyApproval(_ context.Context, v approval.Approval) (approval.VerifiedApproval, error) {
	if v.Proof != "trusted-proof" || v.Approver != "reviewer" {
		return approval.VerifiedApproval{}, errors.New("bad proof")
	}
	return approval.VerifiedApproval{Identity: approval.Identity{ID: "reviewer", Roles: []string{"reviewer"}}, PlanDigest: v.PlanDigest, Environment: v.Environment, ApprovedAt: a.at, ExpiresAt: a.expires}, nil
}

func generationFixture(t *testing.T, dir, dev, prod string) GenerateRequest {
	t.Helper()
	doc, err := source.LoadContext(context.Background(), source.Input{URI: "desired.sql", Format: source.FormatSQL, Data: []byte("CREATE SCHEMA app; CREATE TABLE app.widgets (id bigint, name text NOT NULL);")})
	if err != nil {
		t.Fatal(err)
	}
	_, generator, _ := ed25519.GenerateKey(rand.Reader)
	_, signer, _ := ed25519.GenerateKey(rand.Reader)
	devID, err := simulate.ResolvePostgresIdentity(context.Background(), dev)
	if err != nil {
		t.Fatal(err)
	}
	prodID, err := simulate.ResolvePostgresIdentity(context.Background(), prod)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Add(-time.Minute)
	expires := now.Add(time.Hour)
	return GenerateRequest{Directory: dir, Version: "1", Label: "create_widgets", Format: "sql", RenameHints: "{}", Desired: doc, DevelopmentURL: dev, DevelopmentIdentity: devID, ProductionIdentity: prodID, Environment: "test", DatabaseIdentity: prodID, SourceRevision: "test-revision", Author: "author", Requester: "requester", PostgresVersion: 16, Policy: policy.Document{Version: policy.LanguageVersion, Rules: []policy.Rule{{Name: "allow", Target: "all", Assert: policy.Expression{Eq: []any{true, true}}, Message: "allowed"}}}, PolicyIdentity: "test-policy/v1", ApprovalPolicy: approval.Policy{Environments: map[string]approval.EnvironmentPolicy{"test": {Allowed: true, Requirements: []approval.Requirement{{MinimumRisk: approval.RiskLow, ApproverCount: 1, Roles: []string{"reviewer"}}}}}}, Authority: generationTestAuthority{at: now, expires: expires}, ApprovalAudit: &approval.Chain{Sink: &approval.FileSink{Path: dir + "-approval.audit"}}, Approvals: []approval.Approval{{Approver: "reviewer", ApprovedAt: now, ExpiresAt: expires, Proof: "trusted-proof"}}, CreatedAt: now, ExpiresAt: expires, GeneratorKeyID: "generator", GeneratorPurpose: "migration-generator", SigningKeyID: "release", GeneratorPrivateKey: generator, SigningPrivateKey: signer}
}

func TestGenerateServiceLiveReplaySimulationPublicationAndNoop(t *testing.T) {
	dev, prod := os.Getenv("AUTOSQL_GENERATE_TEST_DSN"), os.Getenv("AUTOSQL_GENERATE_PROD_TEST_DSN")
	if dev == "" || prod == "" {
		t.Skip("generation PostgreSQL URLs not configured")
	}
	d := t.TempDir()
	if err := os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(d, UpdateRequest{ManifestVersion: ManifestVersion}); err != nil {
		t.Fatal(err)
	}
	r := generationFixture(t, d, dev, prod)
	got, err := (GenerateService{}).Generate(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "generated" || got.PlanDigest == "" || got.ChecksDigest == "" || got.BundleDigest == "" || got.Changes == 0 {
		t.Fatalf("incomplete result: %+v", got)
	}
	snap, err := LoadSnapshot(d)
	if err != nil {
		t.Fatal(err)
	}
	a, err := artifact.Parse(snap.Files[got.ArtifactFile])
	if err != nil {
		t.Fatal(err)
	}
	if a.Plan.Digest != got.PlanDigest || a.Checks.Digest != got.ChecksDigest || a.GuardrailDigest != got.BundleDigest || a.Origin.Kind != "generated" {
		t.Fatal("artifact bindings mismatch")
	}
	genPub := r.GeneratorPrivateKey.Public().(ed25519.PublicKey)
	signPub := r.SigningPrivateKey.Public().(ed25519.PublicKey)
	expectedAtt := map[string]artifact.ValidationAttestation{}
	contexts := map[string]string{}
	for _, v := range a.ValidationAttestations {
		expectedAtt[v.Stage] = v
		contexts[v.Stage] = v.ConfigDigest
	}
	verify := artifact.VerifyPolicy{Now: func() time.Time { return r.CreatedAt.Add(time.Second) }, NoEdits: true, Expected: artifact.ExpectedBindings{PlanDigest: got.PlanDigest, GeneratedPlanDigest: got.PlanDigest, ChecksDigest: got.ChecksDigest, GuardrailDigest: got.BundleDigest, SourceRevision: r.SourceRevision, Environment: r.Environment, DatabaseIdentity: r.DatabaseIdentity, ApprovalIdentity: "reviewer", ApprovalProofDigest: sha("trusted-proof")}, Keys: map[string]artifact.KeyRecord{r.SigningKeyID: {PublicKey: signPub, Issuer: "test-issuer", Identity: "release-signer", Environment: r.Environment, Purpose: "release", Status: "active", NotBefore: r.CreatedAt.Add(-time.Hour), NotAfter: r.ExpiresAt.Add(time.Hour)}}, Issuer: "test-issuer", Identity: "release-signer", Purpose: "release", GeneratorKeys: map[string]artifact.KeyRecord{r.GeneratorKeyID: {PublicKey: genPub, Purpose: r.GeneratorPurpose}}, GeneratorPurpose: r.GeneratorPurpose, ExpectedValidationContextDigests: contexts, ExpectedValidationAttestations: expectedAtt}
	if _, err := a.VerifyTrusted(verify); err != nil {
		t.Fatalf("generated artifact did not verify trusted: %v", err)
	}
	mutations := map[string]func(*artifact.Artifact){"plan": func(x *artifact.Artifact) { x.Plan.Digest = sha("wrong") }, "checks": func(x *artifact.Artifact) { x.Checks.Digest = strings.Repeat("0", 64) }, "bundle": func(x *artifact.Artifact) { x.GuardrailDigest = sha("wrong") }, "simulation": func(x *artifact.Artifact) { x.ValidationAttestations[0].Simulation.ToFingerprint = sha("wrong") }, "safety": func(x *artifact.Artifact) { x.ValidationAttestations[1].Safety.DiagnosticsDigest = sha("wrong") }, "policy": func(x *artifact.Artifact) { x.ValidationAttestations[2].Policy.DocumentDigest = sha("wrong") }, "precheck": func(x *artifact.Artifact) {
		x.ValidationAttestations[2].Precheck.ChecksDigest = strings.Repeat("1", 64)
	}}
	for name, mutate := range mutations {
		t.Run("reject_"+name, func(t *testing.T) {
			x := a
			raw, _ := x.MarshalCanonical()
			x, _ = artifact.Parse(raw)
			mutate(&x)
			if _, err := x.VerifyTrusted(verify); err == nil {
				t.Fatal("mutated generated artifact verified")
			}
		})
	}
	before := treeState(t, d)
	r.Version = "2"
	r.Label = "must_not_exist"
	no, err := (GenerateService{}).Generate(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if no.Status != "no_op" {
		t.Fatalf("got %+v", no)
	}
	after := treeState(t, d)
	if before != after {
		t.Fatal("no-op changed directory bytes or metadata")
	}
	artifactPath := filepath.Join(d, ".autosql-generations", snap.Manifest.Generation, got.ArtifactFile)
	tampered := append([]byte(nil), snap.Files[got.ArtifactFile]...)
	tampered[len(tampered)-2] ^= 1
	if err := os.WriteFile(artifactPath, tampered, 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadSnapshot(d); err == nil {
		t.Fatal("tampered signed artifact was accepted")
	}
}

func TestGenerateServiceConcurrentCASHasOneWinner(t *testing.T) {
	dev, prod := os.Getenv("AUTOSQL_GENERATE_TEST_DSN"), os.Getenv("AUTOSQL_GENERATE_PROD_TEST_DSN")
	if dev == "" || prod == "" {
		t.Skip("generation PostgreSQL URLs not configured")
	}
	d := t.TempDir()
	if err := os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(d, UpdateRequest{ManifestVersion: ManifestVersion}); err != nil {
		t.Fatal(err)
	}
	base := generationFixture(t, d, dev, prod)
	type answer struct {
		result GenerateResult
		err    error
	}
	start := make(chan struct{})
	answers := make(chan answer, 2)
	for i := 0; i < 2; i++ {
		go func() {
			<-start
			result, err := (GenerateService{}).Generate(context.Background(), base)
			answers <- answer{result, err}
		}()
	}
	close(start)
	a, b := <-answers, <-answers
	winners, conflicts := 0, 0
	for _, x := range []answer{a, b} {
		if x.err == nil && x.result.Status == "generated" {
			winners++
		}
		if errors.Is(x.err, ErrGenerateConflict) {
			conflicts++
		}
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("winners=%d conflicts=%d a=%v b=%v", winners, conflicts, a.err, b.err)
	}
	snap, err := LoadSnapshot(d)
	if err != nil || len(snap.Manifest.Entries) != 1 {
		t.Fatalf("snapshot entries=%d err=%v", len(snap.Manifest.Entries), err)
	}
}

func TestGenerateServiceLiveRenameHint(t *testing.T) {
	dev, prod := os.Getenv("AUTOSQL_GENERATE_TEST_DSN"), os.Getenv("AUTOSQL_GENERATE_PROD_TEST_DSN")
	if dev == "" || prod == "" {
		t.Skip("generation PostgreSQL URLs not configured")
	}
	d := t.TempDir()
	_ = os.Chmod(d, 0700)
	base := File{Name: "V1__old.sql", SQL: []byte("CREATE SCHEMA app; CREATE TABLE app.old_widgets (id bigint);\n")}
	if _, err := Update(d, UpdateRequest{Files: []File{base}}); err != nil {
		t.Fatal(err)
	}
	r := generationFixture(t, d, dev, prod)
	doc, err := source.LoadContext(context.Background(), source.Input{URI: "rename.sql", Format: source.FormatSQL, Data: []byte("CREATE SCHEMA app; CREATE TABLE app.new_widgets (id bigint);")})
	if err != nil {
		t.Fatal(err)
	}
	r.Desired = doc
	r.Version = "2"
	r.Label = "rename_widgets"
	schemaID := schema.StableID(schema.KindSchema, schema.Name{Name: "app"})
	oldID := schema.StableID(schema.KindTable, schema.Name{Schema: "app", Name: "old_widgets", Parent: schemaID})
	r.RenameHints = `{"` + oldID + `":"app.new_widgets"}`
	got, err := (GenerateService{}).Generate(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	snap, err := LoadSnapshot(d)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(snap.Files[got.File]), "RENAME") {
		t.Fatalf("rename SQL missing: %s", snap.Files[got.File])
	}
}

func TestGenerateServiceCrossProcessCASHasOneWinner(t *testing.T) {
	if child := os.Getenv("AUTOSQL_GENERATE_PROCESS_CHILD"); child != "" {
		r := generationFixture(t, child, os.Getenv("AUTOSQL_GENERATE_TEST_DSN"), os.Getenv("AUTOSQL_GENERATE_PROD_TEST_DSN"))
		_, err := (GenerateService{}).Generate(context.Background(), r)
		if errors.Is(err, ErrGenerateConflict) {
			os.Exit(23)
		}
		if err != nil {
			t.Fatal(err)
		}
		return
	}
	dev, prod := os.Getenv("AUTOSQL_GENERATE_TEST_DSN"), os.Getenv("AUTOSQL_GENERATE_PROD_TEST_DSN")
	if dev == "" || prod == "" {
		t.Skip("generation PostgreSQL URLs not configured")
	}
	d := t.TempDir()
	if err := os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(d, UpdateRequest{ManifestVersion: ManifestVersion}); err != nil {
		t.Fatal(err)
	}
	commands := make([]*exec.Cmd, 2)
	for i := range commands {
		commands[i] = exec.Command(os.Args[0], "-test.run=^TestGenerateServiceCrossProcessCASHasOneWinner$", "-test.v")
		commands[i].Env = append(os.Environ(), "AUTOSQL_GENERATE_PROCESS_CHILD="+d)
		if err := commands[i].Start(); err != nil {
			t.Fatal(err)
		}
	}
	winners, conflicts := 0, 0
	for _, cmd := range commands {
		err := cmd.Wait()
		if err == nil {
			winners++
			continue
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) && ee.ExitCode() == 23 {
			conflicts++
			continue
		}
		t.Fatalf("child failed: %v", err)
	}
	if winners != 1 || conflicts != 1 {
		t.Fatalf("winners=%d conflicts=%d", winners, conflicts)
	}
}

func TestGenerateServiceStageFailuresPublishNothing(t *testing.T) {
	dev, prod := os.Getenv("AUTOSQL_GENERATE_TEST_DSN"), os.Getenv("AUTOSQL_GENERATE_PROD_TEST_DSN")
	if dev == "" || prod == "" {
		t.Skip("generation PostgreSQL URLs not configured")
	}
	for _, stage := range []string{"replay", "simulate", "safety", "policy", "guardrail", "artifact", "publish"} {
		t.Run(stage, func(t *testing.T) {
			d := t.TempDir()
			if err := os.Chmod(d, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := Update(d, UpdateRequest{ManifestVersion: ManifestVersion}); err != nil {
				t.Fatal(err)
			}
			before := treeState(t, d)
			r := generationFixture(t, d, dev, prod)
			r.Stage = func(got string) error {
				if got == stage {
					return os.ErrPermission
				}
				return nil
			}
			if _, err := (GenerateService{}).Generate(context.Background(), r); err == nil {
				t.Fatal("expected injected failure")
			}
			if after := treeState(t, d); before != after {
				t.Fatalf("%s failure published partial state", stage)
			}
		})
	}
}

func TestGenerateControlFailuresPublishNothing(t *testing.T) {
	dev, prod := os.Getenv("AUTOSQL_GENERATE_TEST_DSN"), os.Getenv("AUTOSQL_GENERATE_PROD_TEST_DSN")
	if dev == "" || prod == "" {
		t.Skip("generation PostgreSQL URLs not configured")
	}
	cases := map[string]func(*GenerateRequest){
		"environment": func(r *GenerateRequest) { r.Environment = "forbidden" },
		"proof":       func(r *GenerateRequest) { r.Approvals[0].Proof = "forged" },
		"precheck": func(r *GenerateRequest) {
			r.PrecheckAssertions = []precheck.Assertion{{Name: "must_be_empty", Query: "SELECT count(*) FROM pg_class", MaxAllowed: 0, Timeout: time.Second, Source: precheck.Source{File: "checks.sql", Line: 1, Column: 1}}}
		},
		"policy":             func(r *GenerateRequest) { r.Policy.Rules[0].Assert = policy.Expression{Eq: []any{true, false}} },
		"guardrail_identity": func(r *GenerateRequest) { r.PolicyIdentity = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			d := t.TempDir()
			_ = os.Chmod(d, 0700)
			if _, err := Update(d, UpdateRequest{ManifestVersion: ManifestVersion}); err != nil {
				t.Fatal(err)
			}
			before := treeState(t, d)
			r := generationFixture(t, d, dev, prod)
			mutate(&r)
			if _, err := (GenerateService{}).Generate(context.Background(), r); err == nil {
				t.Fatal("control failure accepted")
			}
			if after := treeState(t, d); after != before {
				t.Fatal("control failure published migration output")
			}
		})
	}
}

func TestGenerateArtifactPublicationFaultMatrix(t *testing.T) {
	dev, prod := os.Getenv("AUTOSQL_GENERATE_TEST_DSN"), os.Getenv("AUTOSQL_GENERATE_PROD_TEST_DSN")
	if dev == "" || prod == "" {
		t.Skip("generation PostgreSQL URLs not configured")
	}
	makeDir := func(t *testing.T) string {
		d := t.TempDir()
		_ = os.Chmod(d, 0700)
		if _, err := Update(d, UpdateRequest{ManifestVersion: ManifestVersion}); err != nil {
			t.Fatal(err)
		}
		return d
	}
	count := 0
	counter := Ops{Write: func(fd int, p []byte) (int, error) { count++; return unix.Write(fd, p) }, Fsync: func(fd int) error { count++; return unix.Fsync(fd) }, Renameat: func(a int, ap string, b int, bp string) error { count++; return unix.Renameat(a, ap, b, bp) }}
	d := makeDir(t)
	r := generationFixture(t, d, dev, prod)
	if _, err := (GenerateService{Ops: counter}).Generate(context.Background(), r); err != nil {
		t.Fatal(err)
	}
	if count < 8 {
		t.Fatalf("unexpected publication boundaries %d", count)
	}
	for fail := 1; fail <= count; fail++ {
		t.Run(fmt.Sprint(fail), func(t *testing.T) {
			d := makeDir(t)
			r := generationFixture(t, d, dev, prod)
			calls, hit := 0, false
			boom := errors.New("injected")
			ops := Ops{Write: func(fd int, p []byte) (int, error) {
				calls++
				if calls == fail {
					hit = true
					return 0, boom
				}
				return unix.Write(fd, p)
			}, Fsync: func(fd int) error {
				calls++
				if calls == fail {
					hit = true
					return boom
				}
				return unix.Fsync(fd)
			}, Renameat: func(a int, ap string, b int, bp string) error {
				calls++
				if calls == fail {
					hit = true
					return boom
				}
				return unix.Renameat(a, ap, b, bp)
			}}
			_, _ = (GenerateService{Ops: ops}).Generate(context.Background(), r)
			if !hit {
				t.Fatal("fault boundary not reached")
			}
			snap, err := LoadSnapshot(d)
			if err != nil {
				t.Fatalf("partial snapshot: %v", err)
			}
			if len(snap.Manifest.Entries) > 1 {
				t.Fatal("mixed snapshot")
			}
			r.ApprovalAudit = &approval.Chain{Sink: &approval.FileSink{Path: d + "-retry.audit"}}
			if _, err = (GenerateService{}).Generate(context.Background(), r); err != nil {
				t.Fatalf("recovery/retry: %v", err)
			}
			snap, err = LoadSnapshot(d)
			if err != nil || len(snap.Manifest.Entries) != 1 || snap.Manifest.Entries[0].ArtifactFile == "" {
				t.Fatalf("new snapshot incomplete: %+v %v", snap.Manifest, err)
			}
		})
	}
}

func treeState(t *testing.T, root string) string {
	t.Helper()
	var b strings.Builder
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		b.WriteString(rel)
		b.WriteByte('|')
		b.WriteString(info.Mode().String())
		b.WriteByte('|')
		b.WriteString(info.ModTime().UTC().Format(time.RFC3339Nano))
		b.WriteByte('|')
		if !info.IsDir() {
			x, e := os.ReadFile(path)
			if e != nil {
				return e
			}
			b.Write(x)
		}
		b.WriteByte('\n')
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return b.String()
}

func TestGenerateFailureBeforePublishLeavesSnapshotExact(t *testing.T) {
	d := t.TempDir()
	if err := os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(d, UpdateRequest{ManifestVersion: ManifestVersion}); err != nil {
		t.Fatal(err)
	}
	before := treeState(t, d)
	r := GenerateRequest{Directory: d, Version: "1", Label: "x", Format: "sql", Stage: func(stage string) error {
		if stage == "snapshot" {
			return os.ErrPermission
		}
		return nil
	}}
	_, err := (GenerateService{}).Generate(context.Background(), r)
	if err == nil {
		t.Fatal("expected failure")
	}
	after := treeState(t, d)
	if before != after {
		t.Fatal("validation failure changed snapshot")
	}
}

func TestGenerationRejectsOnlyDivergedCurrentHead(t *testing.T) {
	resolved := Manifest{Entries: []Migration{
		{Version: "1", NonLinear: true, Parents: []string{"0", "0.1"}},
		{Version: "2", Parents: []string{"1"}},
	}}
	if err := linearHead(resolved); err != nil {
		t.Fatalf("resolved historical topology rejected: %v", err)
	}
	diverged := Manifest{Entries: []Migration{
		{Version: "1"},
		{Version: "2", NonLinear: true, Parents: []string{"1", "1.1"}},
	}}
	err := linearHead(diverged)
	if !errors.Is(err, ErrGenerateConflict) {
		t.Fatalf("head divergence not typed conflict: %v", err)
	}
}
