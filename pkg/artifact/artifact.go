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
	"regexp"
	"strings"
	"time"
	"unicode/utf8"

	"autosql/pkg/guardrail"
	"autosql/pkg/plan"
	"autosql/pkg/precheck"
)

const Version = "autosql.artifact/v1"
const signatureDomain = "autosql.artifact.signature/v1\x00"

var ErrInvalid = errors.New("invalid immutable plan artifact")
var ErrExpired = errors.New("plan artifact expired")
var ErrStale = errors.New("plan artifact source state is stale")

type Error struct {
	Code string
	kind error
}

func (e *Error) Error() string           { return "artifact " + e.Code }
func (e *Error) Unwrap() error           { return e.kind }
func fail(code string, kind error) error { return &Error{Code: code, kind: kind} }

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var rawDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

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
	DatabaseIdentity  string            `json:"database_identity"`
	Approval          Approval          `json:"approval"`
	GuardrailDigest   string            `json:"guardrail_digest"`
	Metadata          map[string]string `json:"metadata"`
	Digest            string            `json:"digest"`
	Signature         Signature         `json:"signature"`
}
type ExpectedBindings struct{ PlanDigest, ChecksDigest, GuardrailDigest, SourceRevision, Environment, DatabaseIdentity, ApprovalIdentity string }
type KeyRecord struct {
	PublicKey                                      ed25519.PublicKey
	Issuer, Identity, Environment, Purpose, Status string
	NotBefore, NotAfter                            time.Time
}
type VerifyPolicy struct {
	Now                       func() time.Time
	Expected                  ExpectedBindings
	Keys                      map[string]KeyRecord
	Issuer, Identity, Purpose string
}
type VerifiedArtifact struct {
	artifact Artifact
	marker   [32]byte
}

func (v VerifiedArtifact) Digest() string { return v.artifact.Digest }
func (v VerifiedArtifact) forRegistry() (Artifact, error) {
	if v.marker != verifiedMarker(v.artifact) || v.artifact.Digest == "" {
		return Artifact{}, fail("verified_token", ErrInvalid)
	}
	if err := v.artifact.validateStored(); err != nil {
		return Artifact{}, fail("verified_token", ErrInvalid)
	}
	return v.artifact, nil
}

func New(p plan.Plan, checks precheck.Plan, created, expires time.Time, revision, environment, databaseIdentity, guardrailDigest string, approval Approval, metadata map[string]string) (Artifact, error) {
	a := Artifact{Version: Version, Plan: p, Checks: checks, CreatedAt: created.UTC(), ExpiresAt: expires.UTC(), SourceRevision: revision, TargetEnvironment: environment, DatabaseIdentity: databaseIdentity, Approval: approval, GuardrailDigest: guardrailDigest, Metadata: clone(metadata)}
	d, err := digest(a)
	a.Digest = d
	return a, err
}
func (a *Artifact) Sign(keyID string, private ed25519.PrivateKey) error {
	if keyID == "" || len(private) != ed25519.PrivateKeySize {
		return fail("signing_key", ErrInvalid)
	}
	if err := a.validateUnsigned(); err != nil {
		return err
	}
	a.Signature = Signature{KeyID: keyID, Algorithm: "Ed25519"}
	d, err := digest(*a)
	if err != nil {
		return err
	}
	a.Digest = d
	a.Signature.Value = base64.RawStdEncoding.EncodeToString(ed25519.Sign(private, []byte(signatureDomain+d)))
	return nil
}
func (a Artifact) Verify(keys map[string]ed25519.PublicKey, now time.Time) error {
	return fail("trusted_policy_required", ErrInvalid)
}
func (a Artifact) VerifyTrusted(policy VerifyPolicy) (VerifiedArtifact, error) {
	if policy.Now == nil {
		return VerifiedArtifact{}, fail("policy", ErrInvalid)
	}
	now := policy.Now().UTC()
	if err := a.validateUnsigned(); err != nil {
		return VerifiedArtifact{}, fail("structure", ErrInvalid)
	}
	want, err := digest(a)
	if err != nil || want != a.Digest {
		return VerifiedArtifact{}, fail("digest", ErrInvalid)
	}
	if now.Before(a.CreatedAt) || !now.Before(a.ExpiresAt) {
		return VerifiedArtifact{}, fail("time", ErrExpired)
	}
	expected := policy.Expected
	if expected.PlanDigest == "" || expected.ChecksDigest == "" || expected.GuardrailDigest == "" || expected.SourceRevision == "" || expected.Environment == "" || expected.DatabaseIdentity == "" || expected.ApprovalIdentity == "" || a.Plan.Digest != expected.PlanDigest || a.Checks.Digest != expected.ChecksDigest || a.GuardrailDigest != expected.GuardrailDigest || a.SourceRevision != expected.SourceRevision || a.TargetEnvironment != expected.Environment || a.DatabaseIdentity != expected.DatabaseIdentity || a.Approval.Identity != expected.ApprovalIdentity {
		return VerifiedArtifact{}, fail("binding", ErrInvalid)
	}
	changeDigest, ce := guardrail.ChangeDigest(a.Plan.Changes)
	if ce != nil || changeDigest != a.Checks.ChangeDigest {
		return VerifiedArtifact{}, fail("changes", ErrInvalid)
	}
	var sql []string
	for _, s := range a.Plan.Steps {
		if s.Kind == plan.StepExecutable {
			sql = append(sql, strings.TrimSuffix(s.SQL, ";"))
		}
	}
	if len(sql) != len(a.Checks.Statements) {
		return VerifiedArtifact{}, fail("statements", ErrInvalid)
	}
	for i := range sql {
		if strings.TrimSuffix(a.Checks.Statements[i], ";") != sql[i] {
			return VerifiedArtifact{}, fail("statements", ErrInvalid)
		}
	}
	if a.Signature.KeyID == "" || policy.Issuer == "" || policy.Identity == "" || policy.Purpose == "" {
		return VerifiedArtifact{}, fail("identity", ErrInvalid)
	}
	record, ok := policy.Keys[a.Signature.KeyID]
	if !ok || len(record.PublicKey) != ed25519.PublicKeySize || record.Status != "active" || record.Issuer != policy.Issuer || record.Identity != policy.Identity || record.Environment != a.TargetEnvironment || record.Purpose != policy.Purpose || record.NotBefore.Location() != time.UTC || record.NotAfter.Location() != time.UTC || !record.NotAfter.After(record.NotBefore) || now.Before(record.NotBefore) || !now.Before(record.NotAfter) {
		return VerifiedArtifact{}, fail("key", ErrInvalid)
	}
	if a.Approval.ApprovedAt.After(a.CreatedAt) || a.Approval.ApprovedAt.After(now) {
		return VerifiedArtifact{}, fail("approval_time", ErrInvalid)
	}
	if a.Signature.Algorithm != "Ed25519" {
		return VerifiedArtifact{}, fail("algorithm", ErrInvalid)
	}
	sig, e := base64.RawStdEncoding.Strict().DecodeString(a.Signature.Value)
	if e != nil || !ed25519.Verify(record.PublicKey, []byte(signatureDomain+a.Digest), sig) {
		return VerifiedArtifact{}, fail("signature", ErrInvalid)
	}
	b, marshalErr := a.MarshalCanonical()
	if marshalErr != nil {
		return VerifiedArtifact{}, fail("clone", ErrInvalid)
	}
	immutable, parseErr := Parse(b)
	if parseErr != nil || immutable.validateStored() != nil {
		return VerifiedArtifact{}, fail("clone", ErrInvalid)
	}
	v := VerifiedArtifact{artifact: immutable}
	v.marker = verifiedMarker(immutable)
	return v, nil
}
func (a Artifact) validateUnsigned() error {
	if a.Version != Version || a.SourceRevision == "" || a.TargetEnvironment == "" || a.DatabaseIdentity == "" || a.GuardrailDigest == "" || a.Approval.Identity == "" || a.Metadata == nil || a.CreatedAt.IsZero() || !a.ExpiresAt.After(a.CreatedAt) || a.Approval.ApprovedAt.IsZero() || a.Approval.ApprovedAt.After(a.CreatedAt) || a.CreatedAt.Location() != time.UTC || a.ExpiresAt.Location() != time.UTC || a.Approval.ApprovedAt.Location() != time.UTC {
		return fmt.Errorf("%w: required metadata", ErrInvalid)
	}
	if err := a.Plan.Validate(); err != nil {
		return fmt.Errorf("%w: plan: %v", ErrInvalid, err)
	}
	wantChecks, checkErr := precheck.Digest(a.Checks)
	if a.Checks.Digest == "" || checkErr != nil || wantChecks != a.Checks.Digest {
		return fmt.Errorf("%w: checks", ErrInvalid)
	}
	if !digestPattern.MatchString(a.Plan.Digest) || !rawDigestPattern.MatchString(a.Checks.Digest) || !digestPattern.MatchString(a.GuardrailDigest) || a.Digest != "" && !digestPattern.MatchString(a.Digest) {
		return fail("digest_format", ErrInvalid)
	}
	return nil
}
func digest(a Artifact) (string, error) {
	a.Digest = ""
	a.Signature.Value = ""
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
func (a Artifact) validateStored() error {
	if err := a.validateUnsigned(); err != nil {
		return fail("structure", ErrInvalid)
	}
	want, err := digest(a)
	if err != nil || want != a.Digest {
		return fail("digest", ErrInvalid)
	}
	if a.Signature.KeyID == "" || a.Signature.Algorithm != "Ed25519" {
		return fail("signature", ErrInvalid)
	}
	sig, err := base64.RawStdEncoding.Strict().DecodeString(a.Signature.Value)
	if err != nil || len(sig) != ed25519.SignatureSize {
		return fail("signature", ErrInvalid)
	}
	return nil
}
func Parse(data []byte) (Artifact, error) {
	if len(data) > 4<<20 || !utf8.Valid(data) || jsonDepthAndDuplicates(data) != nil {
		return Artifact{}, fail("encoding", ErrInvalid)
	}
	var a Artifact
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(&a); err != nil {
		return Artifact{}, fail("json", ErrInvalid)
	}
	var trailing any
	if err := d.Decode(&trailing); err != io.EOF {
		return Artifact{}, fail("json", ErrInvalid)
	}
	canonical, _ := json.Marshal(a)
	if !bytes.Equal(canonical, data) {
		return Artifact{}, fail("canonical", ErrInvalid)
	}
	for i := range a.Plan.Steps {
		if a.Plan.Steps[i].DependsOn == nil {
			a.Plan.Steps[i].DependsOn = []string{}
		}
	}
	return a, nil
}
func jsonDepthAndDuplicates(data []byte) error {
	d := json.NewDecoder(bytes.NewReader(data))
	depth := 0
	for {
		tok, e := d.Token()
		if e == io.EOF {
			return nil
		}
		if e != nil {
			return ErrInvalid
		}
		if v, ok := tok.(json.Delim); ok {
			if v == '{' || v == '[' {
				depth++
				if depth > 64 {
					return ErrInvalid
				}
			} else {
				depth--
			}
		}
	}
}
func clone(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}

// LockedState exposes only a lock-scoped fingerprint read; it has no SQL mutation method.
type RuntimeState struct{ Fingerprint, SourceRevision, Environment, DatabaseIdentity string }
type FingerprintReader interface {
	State(context.Context) (RuntimeState, error)
}
type LockedState interface {
	WithLock(context.Context, func(context.Context, FingerprintReader) error) error
}

func CheckStale(ctx context.Context, state LockedState, v VerifiedArtifact) error {
	if v.marker != verifiedMarker(v.artifact) || v.artifact.Digest == "" {
		return fail("verified_token", ErrInvalid)
	}
	return state.WithLock(ctx, func(ctx context.Context, r FingerprintReader) error {
		got, err := r.State(ctx)
		if err != nil {
			return err
		}
		a := v.artifact
		if !strings.EqualFold(got.Fingerprint, a.Plan.FromFingerprint) || got.SourceRevision != a.SourceRevision || got.Environment != a.TargetEnvironment || got.DatabaseIdentity != a.DatabaseIdentity {
			return ErrStale
		}
		return nil
	})
}
func verifiedMarker(a Artifact) [32]byte {
	return sha256.Sum256([]byte("autosql.artifact.verified/v1\x00" + a.Digest + "\x00" + a.Signature.Value))
}
