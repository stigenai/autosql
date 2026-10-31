package artifact

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

var projectDigestFixture = "sha256:" + strings.Repeat("d", 64)

func projectArtifactFixture(t *testing.T) (Artifact, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	legacy, public, private := fixture(t)
	a, err := NewGenerated(legacy.Plan, legacy.Checks, legacy.CreatedAt, legacy.ExpiresAt, legacy.SourceRevision, legacy.TargetEnvironment, legacy.DatabaseIdentity, legacy.GuardrailDigest, legacy.Approval, legacy.Metadata, "generator", "migration", private)
	if err != nil {
		t.Fatal(err)
	}
	sim, safety, policy := "sha256:"+strings.Repeat("a", 64), "sha256:"+strings.Repeat("b", 64), "sha256:"+strings.Repeat("c", 64)
	atts := []ValidationAttestation{
		{Stage: "replay_simulation", ConfigDigest: sim, Simulation: &SimulationAttestation{TargetIdentity: "db-1", DevelopmentIdentity: "development", FromFingerprint: a.Plan.FromFingerprint, ToFingerprint: a.Plan.ToFingerprint, DatabaseVersion: "17", ConfigDigest: sim}},
		{Stage: "safety", ConfigDigest: safety, Safety: &SafetyAttestation{Analyzers: []string{"compatibility", "postgresql-operational"}, Threshold: "error", ConfigDigest: safety}},
		{Stage: "policy_precheck_guardrail", ConfigDigest: policy, Policy: &PolicyAttestation{DocumentDigest: policy, ConfigDigest: policy}, Precheck: &PrecheckGuardrailAttestation{ChecksDigest: a.Checks.Digest, GuardrailDigest: a.GuardrailDigest, ConfigDigest: policy}},
	}
	for index := range atts {
		atts[index].At, atts[index].ExpiresAt = a.CreatedAt, a.ExpiresAt
		atts[index].Implementation, atts[index].Version, atts[index].ResultDigest = "fixture", "1", sim
	}
	if err := a.SetValidationAttestations(atts); err != nil {
		t.Fatal(err)
	}
	return a, public, private
}

func projectArtifactPolicy(a Artifact, pub ed25519.PublicKey) VerifyPolicy {
	p := trustedPolicy(a, pub, a.CreatedAt)
	p.ExpectedProjectInputDigest = projectDigestFixture
	p.ExpectedValidationAttestations = map[string]ValidationAttestation{}
	p.ExpectedValidationContextDigests = map[string]string{}
	for _, att := range a.ValidationAttestations {
		p.ExpectedValidationAttestations[att.Stage] = att
		p.ExpectedValidationContextDigests[att.Stage] = att.ConfigDigest
	}
	return p
}

func TestProjectBindingComposesEveryStageAndPreservesWireVersion(t *testing.T) {
	a, public, private := projectArtifactFixture(t)
	before := cloneArtifact(t, a)
	if err := a.BindProjectInputs(projectDigestFixture); err != nil {
		t.Fatal(err)
	}
	for index, att := range a.ValidationAttestations {
		old := before.ValidationAttestations[index]
		want, err := ProjectStageConfigDigest(att.Stage, old.ConfigDigest, projectDigestFixture)
		if err != nil || att.ConfigDigest != want || att.ConfigDigest == old.ConfigDigest {
			t.Fatalf("stage did not bind inputs: %+v %v", att, err)
		}
	}
	if a.Version != Version || a.Plan.Digest != before.Plan.Digest {
		t.Fatal("project binding changed wire or plan identity")
	}
	if err := a.Sign("key-1", private); err != nil {
		t.Fatal(err)
	}
	raw, err := a.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	policy := projectArtifactPolicy(a, public)
	if _, err := parsed.VerifyTrusted(policy); err != nil {
		t.Fatal(err)
	}
	if _, err := parsed.VerifyForObservation(policy); err != nil {
		t.Fatal(err)
	}
	if digest, exists, err := parsed.ProjectInputDigest(); err != nil || !exists || digest != projectDigestFixture {
		t.Fatalf("lost signed project claim: %s %t %v", digest, exists, err)
	}
}

func TestProjectVerificationRequiresCurrentIndependentInputDigest(t *testing.T) {
	a, public, private := projectArtifactFixture(t)
	if err := a.BindProjectInputs(projectDigestFixture); err != nil {
		t.Fatal(err)
	}
	if err := a.Sign("key-1", private); err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"", "unsigned-report", "sha256:" + strings.Repeat("e", 64)} {
		policy := projectArtifactPolicy(a, public)
		policy.ExpectedProjectInputDigest = expected
		if _, err := a.VerifyTrusted(policy); !errors.Is(err, ErrInvalid) {
			t.Fatalf("stale/missing input context verified: %v", err)
		}
		if _, err := a.VerifyForObservation(policy); !errors.Is(err, ErrInvalid) {
			t.Fatalf("observation bypassed current project inputs: %v", err)
		}
	}
	policy := projectArtifactPolicy(a, public)
	delete(a.Metadata, ProjectInputsMetadata)
	if err := a.Sign("key-1", private); err != nil {
		t.Fatal(err)
	}
	if _, err := a.VerifyTrusted(policy); err == nil {
		t.Fatal("removing and re-signing marker bypassed required project binding")
	}
}

func TestProjectBindingRejectsTamperedStagesAndMetadata(t *testing.T) {
	a, public, private := projectArtifactFixture(t)
	if err := a.BindProjectInputs(projectDigestFixture); err != nil {
		t.Fatal(err)
	}
	if err := a.Sign("key-1", private); err != nil {
		t.Fatal(err)
	}
	for _, mutate := range []func(*Artifact){
		func(a *Artifact) { a.Metadata[ProjectInputsMetadata] = "{}" },
		func(a *Artifact) { a.Metadata[ProjectInputsMetadata] += " " },
		func(a *Artifact) {
			a.Metadata[ProjectInputsMetadata] = strings.Replace(a.Metadata[ProjectInputsMetadata], projectDigestFixture, "sha256:"+strings.Repeat("e", 64), 1)
		},
		func(a *Artifact) { a.ValidationAttestations[0].ConfigDigest = "sha256:" + strings.Repeat("a", 64) },
		func(a *Artifact) {
			a.ValidationAttestations[1].Safety.ConfigDigest = "sha256:" + strings.Repeat("b", 64)
		},
		func(a *Artifact) { a.ValidationAttestations = a.ValidationAttestations[:2] },
		func(a *Artifact) { a.MarkEditedOrigin("editor") },
	} {
		candidate := cloneArtifact(t, a)
		mutate(&candidate)
		if _, err := candidate.VerifyTrusted(projectArtifactPolicy(a, public)); err == nil {
			t.Fatal("tampered project claim verified")
		}
		if err := candidate.Sign("key-1", private); err == nil {
			t.Fatal("malformed project claim could be re-signed")
		}
	}
}

func TestProjectBindingRefusesPartialOrRepeatedMutation(t *testing.T) {
	a, _, _ := projectArtifactFixture(t)
	before, _ := a.MarshalCanonical()
	err := a.BindProjectInputs("bad-input")
	after, _ := a.MarshalCanonical()
	if err == nil || !bytes.Equal(before, after) {
		t.Fatal("failed binding mutated the artifact")
	}
	if err := a.BindProjectInputs(projectDigestFixture); err != nil {
		t.Fatal(err)
	}
	before, _ = a.MarshalCanonical()
	err = a.BindProjectInputs(projectDigestFixture)
	after, _ = a.MarshalCanonical()
	if err == nil || !bytes.Equal(before, after) {
		t.Fatal("repeated binding compounded or mutated attestations")
	}
	legacy, public, _ := fixture(t)
	if _, err := legacy.VerifyTrusted(trustedPolicy(legacy, public, legacy.CreatedAt)); err != nil {
		t.Fatalf("legacy signed path changed: %v", err)
	}
	if err := legacy.BindProjectInputs(projectDigestFixture); err == nil {
		t.Fatal("signed legacy artifact gained a project binding")
	}
	raw, _ := json.Marshal(legacy)
	if strings.Contains(string(raw), ProjectInputsMetadata) {
		t.Fatal("legacy artifact gained new metadata")
	}
}
