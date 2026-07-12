package migrate

import (
	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/policy"
	"autosql/pkg/simulate"
	"autosql/pkg/source"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

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
	return GenerateRequest{Directory: dir, Version: "1", Label: "create_widgets", Format: "sql", RenameHints: "{}", Desired: doc, DevelopmentURL: dev, DevelopmentIdentity: devID, ProductionIdentity: prodID, Environment: "test", DatabaseIdentity: prodID, SourceRevision: "test-revision", Author: "author", Requester: "requester", PostgresVersion: 16, Policy: policy.Document{Version: policy.LanguageVersion, Rules: []policy.Rule{{Name: "allow", Target: "all", Assert: policy.Expression{Eq: []any{true, true}}, Message: "allowed"}}}, PolicyIdentity: "test-policy/v1", ApprovalPolicy: approval.Policy{Environments: map[string]approval.EnvironmentPolicy{"test": {Allowed: true}}}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), Approval: artifact.Approval{Identity: "generator-approval", ApprovedAt: now, ProofDigest: "sha256:" + strings.Repeat("a", 64)}, GeneratorKeyID: "generator", GeneratorPurpose: "migration-generator", SigningKeyID: "release", GeneratorPrivateKey: generator, SigningPrivateKey: signer}
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
