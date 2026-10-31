package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"strings"
	"testing"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/migrate"
)

func TestCompletionApprovalRequiresOriginalSignedClaims(t *testing.T) {
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Add(-20 * time.Minute)
	identity := approval.Identity{ID: "automation", Roles: []string{"release"}}
	digest := "sha256:" + strings.Repeat("a", 64)
	provider := migrate.AutomationApprovalProvider{KeyID: "approval-key", Identity: identity, PrivateKey: private, TTL: 10 * time.Minute}
	items, _, err := provider.Issue(context.Background(), digest, "production", created, created.Add(time.Hour))
	if err != nil {
		t.Fatal(err)
	}
	proofDigest := sha256.Sum256([]byte(items[0].Proof))
	a := artifact.Artifact{GuardrailDigest: digest, TargetEnvironment: "production", Approval: artifact.Approval{Identity: identity.ID, ApprovedAt: created, ProofDigest: "sha256:" + hex.EncodeToString(proofDigest[:])}}
	cfg := applyConfig{AutomationApprovalKeyID: "approval-key", AutomationApprovalPublicKey: base64.RawStdEncoding.EncodeToString(public)}
	trust := migrationTrust{ApprovalEvidence: items, ApprovalIdentities: map[string]approval.Identity{a.Approval.ProofDigest: identity}}
	// An editable transport expiry cannot extend the signed ten-minute proof.
	trust.ApprovalEvidence[0].ExpiresAt = time.Now().UTC().Add(time.Hour)
	gotIdentity, signed, err := completionApproval(a, cfg, trust)
	if err != nil || !signed.ExpiresAt.Equal(created.Add(10*time.Minute)) {
		t.Fatalf("signed expiry lost: %+v %v", signed, err)
	}
	gate := approval.Gate{Authority: completionAuthority{actors: map[string]approval.Identity{"author": {ID: "author"}, "requester": {ID: "requester"}, identity.ID: gotIdentity}, proof: items[0].Proof, signed: signed}, Policy: approval.Policy{Environments: map[string]approval.EnvironmentPolicy{"production": {Allowed: true, Requirements: []approval.Requirement{{MinimumRisk: approval.RiskLow, ApproverCount: 1, Roles: []string{"release"}}}}}}}
	request := approval.Request{Plan: approval.Plan{Digest: digest, Environment: "production", Author: "author", ExpiresAt: created.Add(time.Hour)}, RequestedBy: "requester", Approvals: items}
	if _, err := gate.EvaluateForObservation(context.Background(), request); !errors.Is(err, approval.ErrDenied) {
		t.Fatalf("expired signed approval accepted: %v", err)
	}
	projectTrust := trust
	projectTrust.Project = &projectReleaseVerification{Path: "project.json"}
	if err := verifyProjectApprovalLifetime(a, cfg, projectTrust); !errors.Is(err, approval.ErrDenied) {
		t.Fatalf("project release extended an expired original approval: %v", err)
	}
	projectTrust.ApprovalEvidence = nil
	if err := verifyProjectApprovalLifetime(a, cfg, projectTrust); !errors.Is(err, approval.ErrDenied) {
		t.Fatalf("project release discarded required signed approval: %v", err)
	}
	for name, mutate := range map[string]func(*artifact.Artifact, *applyConfig, *migrationTrust){
		"legacy manifest": func(_ *artifact.Artifact, _ *applyConfig, t *migrationTrust) { t.ApprovalEvidence = nil },
		"wrong key":       func(_ *artifact.Artifact, c *applyConfig, _ *migrationTrust) { c.AutomationApprovalKeyID = "other" },
		"wrong signer": func(_ *artifact.Artifact, c *applyConfig, _ *migrationTrust) {
			p, _, _ := ed25519.GenerateKey(nil)
			c.AutomationApprovalPublicKey = base64.RawStdEncoding.EncodeToString(p)
		},
		"bundle":      func(a *artifact.Artifact, _ *applyConfig, _ *migrationTrust) { a.GuardrailDigest = "changed" },
		"environment": func(a *artifact.Artifact, _ *applyConfig, _ *migrationTrust) { a.TargetEnvironment = "other" },
		"approval time": func(a *artifact.Artifact, _ *applyConfig, _ *migrationTrust) {
			a.Approval.ApprovedAt = a.Approval.ApprovedAt.Add(time.Second)
		},
		"proof digest":       func(a *artifact.Artifact, _ *applyConfig, _ *migrationTrust) { a.Approval.ProofDigest = "changed" },
		"untrusted identity": func(_ *artifact.Artifact, _ *applyConfig, t *migrationTrust) { t.ApprovalIdentities = nil },
	} {
		t.Run(name, func(t *testing.T) {
			candidate, c, release := a, cfg, trust
			mutate(&candidate, &c, &release)
			if _, _, err := completionApproval(candidate, c, release); err == nil {
				t.Fatal("invalid approval accepted")
			}
		})
	}
}
