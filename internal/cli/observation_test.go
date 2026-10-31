package cli

import (
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
	"autosql/pkg/guardrail"
	"autosql/pkg/precheck"
)

func TestProductionObservationUsesExecutionPolicyWithoutCredentials(t *testing.T) {
	_, plan := workflowFixture(t)
	changeDigest, err := guardrail.ChangeDigest(plan.Changes)
	if err != nil {
		t.Fatal(err)
	}
	checks := precheck.Plan{ID: "observation-checks", ChangeDigest: changeDigest}
	for _, step := range plan.Steps {
		checks.Statements = append(checks.Statements, step.SQL)
	}
	checks.Digest, err = precheck.Digest(checks)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	created := now.Add(-2 * time.Hour)
	a, err := artifact.New(plan, checks, created, created.Add(time.Hour), "revision", "test", "db", "sha256:"+strings.Repeat("a", 64), artifact.Approval{Identity: "release", ApprovedAt: created}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	pub, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Sign("release-key", private); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	cfg := applyConfig{
		EditorIdentity: "unused-editor", EditSigningKeyReference: "env://AUTOSQL_OBSERVATION_UNRESOLVED", DevelopmentURLReference: "env://AUTOSQL_OBSERVATION_UNRESOLVED", DownConfigPath: filepath.Join(dir, "absent-down-config.json"),
		DatabaseURL: "env://AUTOSQL_OBSERVATION_UNRESOLVED", Environment: "test", DatabaseIdentity: "db", SourceRevision: "revision",
		KeyID: "release-key", PublicKey: base64.RawStdEncoding.EncodeToString(pub), Issuer: "issuer", Signer: "signer",
		KeyStatus: "active", KeyPurpose: "release", KeyNotBefore: created.Add(-time.Hour), KeyNotAfter: now.Add(time.Hour),
		ExpectedPlanDigest: a.Plan.Digest, ExpectedChecksDigest: a.Checks.Digest, ExpectedGuardrailDigest: a.GuardrailDigest, ExpectedApprovalIdentity: a.Approval.Identity,
		ApprovalAuditPath: filepath.Join(dir, "approval.jsonl"), LifecycleAuditPath: filepath.Join(dir, "lifecycle.jsonl"),
	}
	configPath := filepath.Join(dir, "apply.json")
	writeConfig := func() {
		t.Helper()
		raw, err := json.Marshal(cfg)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(configPath, raw, 0600); err != nil {
			t.Fatal(err)
		}
	}
	t.Setenv("AUTOSQL_APPLY_CONFIG", configPath)
	t.Setenv("AUTOSQL_OBSERVATION_UNRESOLVED", "")
	writeConfig()
	observed, err := VerifyProductionArtifactForObservation(a, false)
	if err != nil || observed.Digest() != a.Digest {
		t.Fatalf("expired observation: %v", err)
	}
	if _, err := VerifyProductionArtifactForApply(a, false); !errors.Is(err, artifact.ErrExpired) {
		t.Fatalf("execution expiry relaxed: %v", err)
	}
	if _, err := VerifyProductionArtifactForObservation(a, true); err == nil {
		t.Fatal("mandatory no-edits policy relaxed")
	}
	cfg.KeyStatus = "revoked"
	writeConfig()
	if _, err := VerifyProductionArtifactForObservation(a, false); !errors.Is(err, artifact.ErrInvalid) {
		t.Fatalf("current production policy not consulted: %v", err)
	}
	for _, path := range []string{cfg.ApprovalAuditPath, cfg.LifecycleAuditPath} {
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("verification wrote audit evidence: %v", err)
		}
	}
}
