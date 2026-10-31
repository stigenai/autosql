package artifact

import (
	"crypto/sha256"
	"time"
)

// VerifiedObservation authenticates an artifact for completion inspection only.
// It cannot be converted to VerifiedArtifact or passed to an execution API.
// It does not prove execution, current database state, or current authorization
// under an external approval policy; those are separate observation predicates.
type VerifiedObservation struct {
	artifact          Artifact
	observationMarker [32]byte
}

// VerifyForObservation applies the trusted verification policy, permitting only
// an elapsed artifact lifetime. Future creation and invalid trust, signature,
// approval bindings, attestations, or target bindings still fail.
func (a Artifact) VerifyForObservation(policy VerifyPolicy) (VerifiedObservation, error) {
	immutable, err := a.verifyTrusted(policy, verifyObservation)
	if err != nil {
		return VerifiedObservation{}, err
	}
	return VerifiedObservation{artifact: immutable, observationMarker: observationMarker(immutable)}, nil
}

func (v VerifiedObservation) Digest() string { return v.artifact.Digest }

// Payload returns an independent snapshot. Obtaining the payload does not grant
// execution authority; VerifyTrusted must succeed under the execution policy.
func (v VerifiedObservation) Payload() (Artifact, error) {
	if v.artifact.Digest == "" || v.observationMarker != observationMarker(v.artifact) || v.artifact.validateStored() != nil {
		return Artifact{}, fail("observation_token", ErrInvalid)
	}
	raw, err := v.artifact.MarshalCanonical()
	if err != nil {
		return Artifact{}, fail("observation_token", ErrInvalid)
	}
	return Parse(raw)
}

func observationMarker(a Artifact) [32]byte {
	return sha256.Sum256([]byte("autosql.artifact.observation/v1\x00" + a.Digest + "\x00" + a.Signature.Value))
}

func observationAttestationsCurrent(attestations []ValidationAttestation, now time.Time) bool {
	for _, attestation := range attestations {
		if attestation.At.IsZero() || now.Before(attestation.At) || !now.Before(attestation.ExpiresAt) {
			return false
		}
	}
	return true
}
