package cli

import (
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/guardrail"
	"autosql/pkg/precheck"
)

func TestApplyRevalidatesVerifiedSnapshotWithoutReopeningArtifact(t *testing.T) {
	_, plan := workflowFixture(t)
	change, err := guardrail.ChangeDigest(plan.Changes)
	if err != nil {
		t.Fatal(err)
	}
	checks := precheck.Plan{ID: "snapshot", ChangeDigest: change}
	for _, step := range plan.Steps {
		checks.Statements = append(checks.Statements, step.SQL)
	}
	checks.Digest, err = precheck.Digest(checks)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	a, err := artifact.New(plan, checks, now.Add(-time.Minute), now.Add(time.Hour), "revision", "test", "db", "sha256:"+strings.Repeat("a", 64), artifact.Approval{Identity: "reviewer", ApprovedAt: now.Add(-time.Minute)}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	pub, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Sign("release", private); err != nil {
		t.Fatal(err)
	}
	policy := artifact.VerifyPolicy{Now: func() time.Time { return now }, Expected: artifact.ExpectedBindings{PlanDigest: a.Plan.Digest, ChecksDigest: a.Checks.Digest, GuardrailDigest: a.GuardrailDigest, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: a.DatabaseIdentity, ApprovalIdentity: a.Approval.Identity}, Keys: map[string]artifact.KeyRecord{"release": {PublicKey: pub, Issuer: "issuer", Identity: "signer", Environment: "test", Purpose: "release", Status: "active", NotBefore: now.Add(-time.Hour), NotAfter: now.Add(2 * time.Hour)}}, Issuer: "issuer", Identity: "signer", Purpose: "release"}
	verified, err := a.VerifyTrusted(policy)
	if err != nil {
		t.Fatal(err)
	}
	stop := errors.New("stop after immutable input")
	inputs := 0
	service := VerifiedArtifactApplyService{Policy: policy, Input: func(got artifact.Artifact) (guardrail.Input, error) {
		inputs++
		if got.Digest != a.Digest {
			t.Fatal("artifact snapshot changed")
		}
		return guardrail.Input{}, stop
	}, Mutation: func(artifact.VerifiedArtifact) (guardrail.AuthorizedMutation, error) {
		t.Fatal("refused request constructed mutation")
		return nil, nil
	}}
	request := ApplyRequest{ApprovalMode: "artifact", ArtifactPath: filepath.Join(t.TempDir(), "artifact-was-removed.json"), VerifiedArtifact: verified}
	if _, err := service.Apply(context.Background(), request); !errors.Is(err, stop) || inputs != 1 {
		t.Fatalf("reopened file instead of snapshot: inputs=%d err=%v", inputs, err)
	}
	request.NoEdits = true
	if _, err := service.Apply(context.Background(), request); err == nil || inputs != 1 {
		t.Fatal("snapshot bypassed mandatory no-edits")
	}
	request.NoEdits = false
	key := service.Policy.Keys["release"]
	key.Status = "revoked"
	service.Policy.Keys["release"] = key
	if _, err := service.Apply(context.Background(), request); err == nil || inputs != 1 {
		t.Fatal("snapshot bypassed current key policy")
	}
	key.Status = "active"
	service.Policy.Keys["release"] = key
	now = a.ExpiresAt
	if _, err := service.Apply(context.Background(), request); !errors.Is(err, artifact.ErrExpired) || inputs != 1 {
		t.Fatalf("snapshot bypassed expiry: %v", err)
	}
}
