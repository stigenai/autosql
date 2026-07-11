// Package artifact defines signed, immutable migration plan artifacts.
package artifact

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"autosql/pkg/plan"
	"autosql/pkg/precheck"
)

const Version = "autosql.artifact/v1"
const signatureDomain = "autosql.artifact.signature/v1\x00"

var ErrInvalid = errors.New("invalid immutable plan artifact")
var ErrExpired = errors.New("plan artifact expired")
var ErrStale = errors.New("plan artifact source state is stale")

type Approval struct {
	Identity   string    `json:"identity"`
	ApprovedAt time.Time `json:"approved_at"`
}
type Signature struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}
type Artifact struct {
	Version           string            `json:"version"`
	Plan              plan.Plan         `json:"plan"`
	Checks            precheck.Plan     `json:"checks"`
	CreatedAt         time.Time         `json:"created_at"`
	ExpiresAt         time.Time         `json:"expires_at"`
	SourceRevision    string            `json:"source_revision"`
	TargetEnvironment string            `json:"target_environment"`
	Approval          Approval          `json:"approval"`
	GuardrailDigest   string            `json:"guardrail_digest"`
	Metadata          map[string]string `json:"metadata"`
	Digest            string            `json:"digest"`
	Signature         Signature         `json:"signature"`
}

func New(p plan.Plan, checks precheck.Plan, created, expires time.Time, revision, environment, guardrailDigest string, approval Approval, metadata map[string]string) (Artifact, error) {
	a := Artifact{Version: Version, Plan: p, Checks: checks, CreatedAt: created.UTC(), ExpiresAt: expires.UTC(), SourceRevision: revision, TargetEnvironment: environment, Approval: approval, GuardrailDigest: guardrailDigest, Metadata: clone(metadata)}
	d, err := digest(a)
	a.Digest = d
	return a, err
}
func (a *Artifact) Sign(keyID string, private ed25519.PrivateKey) error {
	if err := a.validateUnsigned(); err != nil {
		return err
	}
	d, err := digest(*a)
	if err != nil {
		return err
	}
	a.Digest = d
	a.Signature = Signature{KeyID: keyID, Algorithm: "Ed25519", Value: base64.StdEncoding.EncodeToString(ed25519.Sign(private, []byte(signatureDomain+d)))}
	return nil
}
func (a Artifact) Verify(keys map[string]ed25519.PublicKey, now time.Time) error {
	if err := a.validateUnsigned(); err != nil {
		return err
	}
	want, err := digest(a)
	if err != nil || want != a.Digest {
		return fmt.Errorf("%w: digest mismatch", ErrInvalid)
	}
	if !now.Before(a.ExpiresAt) {
		return ErrExpired
	}
	if a.Signature.Algorithm != "Ed25519" {
		return fmt.Errorf("%w: signature algorithm", ErrInvalid)
	}
	key, ok := keys[a.Signature.KeyID]
	if !ok {
		return fmt.Errorf("%w: unknown key", ErrInvalid)
	}
	sig, e := base64.StdEncoding.DecodeString(a.Signature.Value)
	if e != nil || !ed25519.Verify(key, []byte(signatureDomain+a.Digest), sig) {
		return fmt.Errorf("%w: signature", ErrInvalid)
	}
	return nil
}
func (a Artifact) validateUnsigned() error {
	if a.Version != Version || a.SourceRevision == "" || a.TargetEnvironment == "" || a.GuardrailDigest == "" || a.Approval.Identity == "" || a.CreatedAt.IsZero() || !a.ExpiresAt.After(a.CreatedAt) || a.Approval.ApprovedAt.IsZero() {
		return fmt.Errorf("%w: required metadata", ErrInvalid)
	}
	if err := a.Plan.Validate(); err != nil {
		return fmt.Errorf("%w: plan: %v", ErrInvalid, err)
	}
	wantChecks, checkErr := precheck.Digest(a.Checks)
	if a.Checks.Digest == "" || checkErr != nil || wantChecks != a.Checks.Digest {
		return fmt.Errorf("%w: checks", ErrInvalid)
	}
	return nil
}
func digest(a Artifact) (string, error) {
	a.Digest = ""
	a.Signature = Signature{}
	b, e := json.Marshal(a)
	if e != nil {
		return "", e
	}
	s := sha256.Sum256(append([]byte("autosql.artifact.digest/v1\x00"), b...))
	return "sha256:" + hex.EncodeToString(s[:]), nil
}
func (a Artifact) MarshalCanonical() ([]byte, error) {
	if _, err := digest(a); err != nil {
		return nil, err
	}
	return json.Marshal(a)
}
func Parse(data []byte) (Artifact, error) {
	var a Artifact
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&a); err != nil {
		return Artifact{}, fmt.Errorf("%w: %v", ErrInvalid, err)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return Artifact{}, ErrInvalid
	}
	return a, nil
}
func clone(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// LockedState exposes only a lock-scoped fingerprint read; it has no SQL mutation method.
type FingerprintReader interface {
	Fingerprint(context.Context) (string, error)
}
type LockedState interface {
	WithLock(context.Context, func(context.Context, FingerprintReader) error) error
}

func CheckStale(ctx context.Context, state LockedState, a Artifact) error {
	return state.WithLock(ctx, func(ctx context.Context, r FingerprintReader) error {
		got, err := r.Fingerprint(ctx)
		if err != nil {
			return err
		}
		if !strings.EqualFold(got, a.Plan.FromFingerprint) {
			return ErrStale
		}
		return nil
	})
}
