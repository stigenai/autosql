package artifact

import (
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestObservationPermitsOnlyElapsedArtifactLifetime(t *testing.T) {
	a, pub, private := fixture(t)
	for _, now := range []time.Time{a.CreatedAt, a.ExpiresAt, a.ExpiresAt.Add(time.Hour)} {
		policy := trustedPolicy(a, pub, now)
		observed, err := a.VerifyForObservation(policy)
		if err != nil || observed.Digest() != a.Digest {
			t.Fatalf("observation at %s: %v", now, err)
		}
		payload, err := observed.Payload()
		if err != nil || !reflect.DeepEqual(payload, a) {
			t.Fatalf("observation changed signed payload: %v", err)
		}
		_, err = payload.VerifyTrusted(policy)
		if now.Before(a.ExpiresAt) && err != nil || !now.Before(a.ExpiresAt) && !errors.Is(err, ErrExpired) {
			t.Fatalf("execution lifetime changed at %s: %v", now, err)
		}
	}
	// Keep approval valid while making creation future-dated, so this tests the
	// creation boundary independently of the approval-time boundary.
	a.Approval.ApprovedAt = a.CreatedAt.Add(-time.Minute)
	if err := a.Sign("key-1", private); err != nil {
		t.Fatal(err)
	}
	if _, err := a.VerifyForObservation(trustedPolicy(a, pub, a.CreatedAt.Add(-time.Second))); err == nil {
		t.Fatal("future-created artifact accepted for observation")
	}
}

func TestObservationRetainsTrustAndBindings(t *testing.T) {
	a, pub, _ := fixture(t)
	cases := map[string]func(*Artifact, *VerifyPolicy){
		"signature":         func(a *Artifact, _ *VerifyPolicy) { a.Signature.Value = "AAAA" },
		"payload":           func(a *Artifact, _ *VerifyPolicy) { a.Metadata["ticket"] = "tampered" },
		"database":          func(_ *Artifact, p *VerifyPolicy) { p.Expected.DatabaseIdentity = "other" },
		"environment":       func(_ *Artifact, p *VerifyPolicy) { p.Expected.Environment = "other" },
		"approval identity": func(_ *Artifact, p *VerifyPolicy) { p.Expected.ApprovalIdentity = "other" },
		"approval proof":    func(_ *Artifact, p *VerifyPolicy) { p.Expected.ApprovalProofDigest = "other" },
		"revocation": func(_ *Artifact, p *VerifyPolicy) {
			key := p.Keys["key-1"]
			key.Status = "revoked"
			p.Keys["key-1"] = key
		},
		"key expiry": func(a *Artifact, p *VerifyPolicy) {
			key := p.Keys["key-1"]
			key.NotAfter = a.ExpiresAt
			p.Keys["key-1"] = key
		},
		"future approval": func(a *Artifact, p *VerifyPolicy) {
			p.Now = func() time.Time { return a.Approval.ApprovedAt.Add(-time.Second) }
		},
		"no edits":      func(_ *Artifact, p *VerifyPolicy) { p.NoEdits = true },
		"missing clock": func(_ *Artifact, p *VerifyPolicy) { p.Now = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := cloneArtifact(t, a)
			policy := trustedPolicy(a, pub, a.ExpiresAt)
			mutate(&candidate, &policy)
			observed, err := candidate.VerifyForObservation(policy)
			if err == nil || observed.Digest() != "" {
				t.Fatalf("invalid observation accepted: %v", err)
			}
		})
	}
}

func TestObservationCannotBecomeExecutionAuthority(t *testing.T) {
	observationType, executionType := reflect.TypeFor[VerifiedObservation](), reflect.TypeFor[VerifiedArtifact]()
	if observationType.ConvertibleTo(executionType) || executionType.ConvertibleTo(observationType) {
		t.Fatal("purpose-specific verification tokens are convertible")
	}
	if _, err := (VerifiedObservation{}).Payload(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("zero observation accepted: %v", err)
	}
	a, pub, _ := fixture(t)
	observed, err := a.VerifyForObservation(trustedPolicy(a, pub, a.ExpiresAt))
	if err != nil {
		t.Fatal(err)
	}
	// Even package-internal token confusion must fail at the marker boundary.
	forged := VerifiedArtifact{artifact: observed.artifact, marker: observed.observationMarker}
	if _, err := forged.Payload(); !errors.Is(err, ErrInvalid) {
		t.Fatalf("observation marker authorized execution payload: %v", err)
	}
	a.Metadata["ticket"] = "changed source"
	first, err := observed.Payload()
	if err != nil || first.Metadata["ticket"] != "DB-1" {
		t.Fatalf("source mutation changed observation: %v", err)
	}
	first.Metadata["ticket"] = "changed payload"
	first.Plan.Steps[0].SQL = "changed SQL"
	again, err := observed.Payload()
	if err != nil || again.Metadata["ticket"] != "DB-1" || again.Plan.Steps[0].SQL == "changed SQL" {
		t.Fatalf("returned payload mutation changed observation: %v", err)
	}
}

func TestObservationDoesNotExtendValidationAttestations(t *testing.T) {
	for _, timing := range []string{"current", "expired", "future"} {
		t.Run(timing, func(t *testing.T) {
			a, pub, private := fixture(t)
			at, expires := a.CreatedAt, a.ExpiresAt.Add(time.Hour)
			if timing == "expired" {
				expires = a.ExpiresAt
			}
			if timing == "future" {
				at = a.ExpiresAt.Add(time.Second)
			}
			attestation := ValidationAttestation{Stage: "safety", Implementation: "contract", Version: "1", ConfigDigest: a.GuardrailDigest, ResultDigest: a.GuardrailDigest, At: at, ExpiresAt: expires}
			a.ValidationAttestations = []ValidationAttestation{attestation}
			if err := a.Sign("key-1", private); err != nil {
				t.Fatal(err)
			}
			policy := trustedPolicy(a, pub, a.ExpiresAt)
			policy.ExpectedValidationAttestations = map[string]ValidationAttestation{"safety": attestation}
			policy.ExpectedValidationContextDigests = map[string]string{"safety": attestation.ConfigDigest}
			_, err := a.VerifyForObservation(policy)
			if (err == nil) != (timing == "current") {
				t.Fatalf("attestation timing %s: %v", timing, err)
			}
		})
	}
}
