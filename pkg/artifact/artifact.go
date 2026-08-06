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
	"reflect"
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
	Identity    string    `json:"identity"`
	ApprovedAt  time.Time `json:"approved_at"`
	ProofDigest string    `json:"proof_digest,omitempty"`
}
type Signature struct {
	KeyID     string `json:"key_id"`
	Algorithm string `json:"algorithm"`
	Value     string `json:"value"`
}
type EditRecord struct {
	Digest         string    `json:"digest"`
	ParentDigest   string    `json:"parent_digest"`
	SQLDigest      string    `json:"sql_digest"`
	EditorIdentity string    `json:"editor_identity"`
	EditedAt       time.Time `json:"edited_at"`
	Reason         string    `json:"reason"`
	Source         string    `json:"source"`
}
type ValidationAttestation struct {
	Stage          string                        `json:"stage"`
	Implementation string                        `json:"implementation"`
	Version        string                        `json:"version"`
	ConfigDigest   string                        `json:"config_digest"`
	ResultDigest   string                        `json:"result_digest"`
	At             time.Time                     `json:"at"`
	ExpiresAt      time.Time                     `json:"expires_at"`
	Simulation     *SimulationAttestation        `json:"simulation,omitempty"`
	Safety         *SafetyAttestation            `json:"safety,omitempty"`
	Policy         *PolicyAttestation            `json:"policy,omitempty"`
	Precheck       *PrecheckGuardrailAttestation `json:"precheck_guardrail,omitempty"`
	Editor         *EditorAttestation            `json:"editor,omitempty"`
}
type SimulationAttestation struct {
	TargetIdentity      string `json:"target_identity"`
	DevelopmentIdentity string `json:"development_identity"`
	FromFingerprint     string `json:"from_fingerprint"`
	ToFingerprint       string `json:"to_fingerprint"`
	DatabaseVersion     string `json:"database_version"`
	ConfigDigest        string `json:"config_digest"`
}
type SafetyAttestation struct {
	Analyzers          []string `json:"analyzers"`
	Threshold          string   `json:"threshold"`
	SuppressionsDigest string   `json:"suppressions_digest"`
	DiagnosticsDigest  string   `json:"diagnostics_digest"`
	ConfigDigest       string   `json:"config_digest"`
}
type PolicyAttestation struct {
	DocumentDigest  string `json:"document_digest"`
	LimitsDigest    string `json:"limits_digest"`
	ResourcesDigest string `json:"resources_digest"`
	ConfigDigest    string `json:"config_digest"`
}
type PrecheckGuardrailAttestation struct {
	ChecksDigest    string `json:"checks_digest"`
	GuardrailDigest string `json:"guardrail_digest"`
	ConfigDigest    string `json:"config_digest"`
}
type EditorAttestation struct {
	Identity     string `json:"identity"`
	ReasonDigest string `json:"reason_digest"`
	ChainDigest  string `json:"chain_digest"`
	ConfigDigest string `json:"config_digest"`
}
type EditProvenance struct {
	Version                 string                  `json:"version"`
	OriginalArtifact        []byte                  `json:"original_artifact"`
	OriginalLength          int                     `json:"original_length"`
	OriginalBytesDigest     string                  `json:"original_bytes_digest"`
	OriginalArtifactDigest  string                  `json:"original_artifact_digest"`
	OriginalPlanDigest      string                  `json:"original_plan_digest"`
	OriginalSignatureDigest string                  `json:"original_signature_digest"`
	CandidatePlanDigest     string                  `json:"candidate_plan_digest"`
	CandidateBytesDigest    string                  `json:"candidate_bytes_digest"`
	ChainDigest             string                  `json:"chain_digest"`
	Records                 []EditRecord            `json:"records"`
	Attestations            []ValidationAttestation `json:"attestations"`
}
type ArtifactOrigin struct {
	Kind          string `json:"kind"`
	PlanDigest    string `json:"plan_digest"`
	Generator     string `json:"generator"`
	GeneratorHash string `json:"generator_hash"`
	KeyID         string `json:"key_id,omitempty"`
	Purpose       string `json:"purpose,omitempty"`
	Signature     string `json:"signature,omitempty"`
}
type Artifact struct {
	Version                string                  `json:"version"`
	Plan                   plan.Plan               `json:"plan"`
	Checks                 precheck.Plan           `json:"checks"`
	CreatedAt              time.Time               `json:"created_at"`
	ExpiresAt              time.Time               `json:"expires_at"`
	SourceRevision         string                  `json:"source_revision"`
	TargetEnvironment      string                  `json:"target_environment"`
	DatabaseIdentity       string                  `json:"database_identity"`
	Approval               Approval                `json:"approval"`
	GuardrailDigest        string                  `json:"guardrail_digest"`
	Metadata               map[string]string       `json:"metadata"`
	Digest                 string                  `json:"digest"`
	Signature              Signature               `json:"signature"`
	EditProvenance         *EditProvenance         `json:"edit_provenance,omitempty"`
	ValidationAttestations []ValidationAttestation `json:"validation_attestations,omitempty"`
	Origin                 ArtifactOrigin          `json:"origin"`
}
type ExpectedBindings struct{ PlanDigest, ChecksDigest, GuardrailDigest, SourceRevision, Environment, DatabaseIdentity, ApprovalIdentity, ApprovalProofDigest, GeneratedPlanDigest string }
type KeyRecord struct {
	PublicKey                                      ed25519.PublicKey
	Issuer, Identity, Environment, Purpose, Status string
	NotBefore, NotAfter                            time.Time
}
type VerifyPolicy struct {
	Now                              func() time.Time
	Expected                         ExpectedBindings
	Keys                             map[string]KeyRecord
	Issuer, Identity, Purpose        string
	NoEdits                          bool
	GeneratorKeys                    map[string]KeyRecord
	GeneratorPurpose                 string
	ExpectedValidationContextDigests map[string]string
	ExpectedValidationAttestations   map[string]ValidationAttestation
}
type VerifiedArtifact struct {
	artifact Artifact
	marker   [32]byte
}

func (v VerifiedArtifact) Digest() string { return v.artifact.Digest }

// Payload returns a defensive copy only for a valid verified token.
func (v VerifiedArtifact) Payload() (Artifact, error) { return v.forRegistry() }
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
	a := Artifact{Version: Version, Plan: p, Checks: checks, CreatedAt: created.UTC(), ExpiresAt: expires.UTC(), SourceRevision: revision, TargetEnvironment: environment, DatabaseIdentity: databaseIdentity, Approval: approval, GuardrailDigest: guardrailDigest, Metadata: clone(metadata), Origin: ArtifactOrigin{Kind: "unattested", PlanDigest: p.Digest, Generator: "autosql/" + plan.PlannerVersion}}
	a.Origin.GeneratorHash = originHash(a.Origin)
	d, err := digest(a)
	a.Digest = d
	return a, err
}

func NewGenerated(p plan.Plan, checks precheck.Plan, created, expires time.Time, revision, environment, databaseIdentity, guardrailDigest string, approval Approval, metadata map[string]string, keyID, purpose string, private ed25519.PrivateKey) (Artifact, error) {
	a, err := New(p, checks, created, expires, revision, environment, databaseIdentity, guardrailDigest, approval, metadata)
	if err != nil || keyID == "" || purpose == "" || len(private) != ed25519.PrivateKeySize {
		return Artifact{}, fail("generator_key", ErrInvalid)
	}
	a.Origin.Kind, a.Origin.KeyID, a.Origin.Purpose = "generated", keyID, purpose
	a.Origin.GeneratorHash = originHash(a.Origin)
	a.Origin.Signature = base64.RawStdEncoding.EncodeToString(ed25519.Sign(private, []byte("autosql.artifact.generator/v1\x00"+a.Origin.GeneratorHash)))
	return a, nil
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
func (a *Artifact) SetValidationAttestations(v []ValidationAttestation) error {
	if a.Signature.Value != "" || a.EditProvenance != nil {
		return fail("validation_attestations", ErrInvalid)
	}
	a.ValidationAttestations = append([]ValidationAttestation(nil), v...)
	d, e := digest(*a)
	a.Digest = d
	return e
}

// ResetAuthorization clears signing material and recomputes the artifact digest.
func (a *Artifact) ResetAuthorization() error {
	a.Signature = Signature{}
	d, err := digest(*a)
	a.Digest = d
	return err
}
func (a *Artifact) MarkEditedOrigin(generator string) {
	a.Origin = ArtifactOrigin{Kind: "edited", PlanDigest: a.Plan.Digest, Generator: generator}
	a.Origin.GeneratorHash = originHash(a.Origin)
}
func (a Artifact) Verify(keys map[string]ed25519.PublicKey, now time.Time) error {
	return fail("trusted_policy_required", ErrInvalid)
}
func (a Artifact) VerifyTrusted(policy VerifyPolicy) (VerifiedArtifact, error) {
	if policy.Now == nil {
		return VerifiedArtifact{}, fail("policy", ErrInvalid)
	}
	now := policy.Now().UTC()
	if policy.NoEdits {
		generated := policy.Expected.GeneratedPlanDigest
		record, ok := policy.GeneratorKeys[a.Origin.KeyID]
		sig, decodeErr := base64.RawStdEncoding.Strict().DecodeString(a.Origin.Signature)
		if a.EditProvenance != nil || a.Origin.Kind != "generated" || generated == "" || a.Origin.PlanDigest != generated || a.Plan.Digest != generated || policy.GeneratorPurpose == "" || a.Origin.Purpose != policy.GeneratorPurpose || !ok || record.Purpose != policy.GeneratorPurpose || len(record.PublicKey) != ed25519.PublicKeySize || decodeErr != nil || !ed25519.Verify(record.PublicKey, []byte("autosql.artifact.generator/v1\x00"+a.Origin.GeneratorHash), sig) {
			return VerifiedArtifact{}, fail("edits_forbidden", ErrInvalid)
		}
	}
	attestations := a.ValidationAttestations
	if a.EditProvenance != nil {
		attestations = a.EditProvenance.Attestations
	}
	if len(attestations) > 0 && len(policy.ExpectedValidationContextDigests) > 0 {
		seen := map[string]bool{}
		for _, att := range attestations {
			expected, ok := policy.ExpectedValidationContextDigests[att.Stage]
			if !ok || expected != att.ConfigDigest {
				return VerifiedArtifact{}, fail("validation_context", ErrInvalid)
			}
			seen[att.Stage] = true
		}
		if len(seen) != len(policy.ExpectedValidationContextDigests) {
			return VerifiedArtifact{}, fail("validation_context", ErrInvalid)
		}
	}
	if len(attestations) > 0 && len(policy.ExpectedValidationAttestations) == 0 {
		return VerifiedArtifact{}, fail("validation_attestation_manifest", ErrInvalid)
	}
	if len(attestations) > 0 {
		seen := map[string]bool{}
		for _, got := range attestations {
			want, ok := policy.ExpectedValidationAttestations[got.Stage]
			if !ok || !reflect.DeepEqual(got.Simulation, want.Simulation) || !reflect.DeepEqual(got.Safety, want.Safety) || !reflect.DeepEqual(got.Policy, want.Policy) || !reflect.DeepEqual(got.Precheck, want.Precheck) || !reflect.DeepEqual(got.Editor, want.Editor) {
				return VerifiedArtifact{}, fail("validation_attestation", ErrInvalid)
			}
			seen[got.Stage] = true
		}
		if len(seen) != len(policy.ExpectedValidationAttestations) {
			return VerifiedArtifact{}, fail("validation_attestation", ErrInvalid)
		}
	}
	if err := a.validateUnsigned(); err != nil {
		return VerifiedArtifact{}, fail("structure", ErrInvalid)
	}
	want, err := digest(a)
	if err != nil || want != a.Digest {
		return VerifiedArtifact{}, fail("digest", ErrInvalid)
	}
	expected := policy.Expected
	if expected.PlanDigest == "" || expected.ChecksDigest == "" || expected.GuardrailDigest == "" || expected.SourceRevision == "" || expected.Environment == "" || expected.DatabaseIdentity == "" || expected.ApprovalIdentity == "" || a.Plan.Digest != expected.PlanDigest || a.Checks.Digest != expected.ChecksDigest || a.GuardrailDigest != expected.GuardrailDigest || a.SourceRevision != expected.SourceRevision || a.TargetEnvironment != expected.Environment || a.DatabaseIdentity != expected.DatabaseIdentity || a.Approval.Identity != expected.ApprovalIdentity {
		return VerifiedArtifact{}, fail("binding", ErrInvalid)
	}
	if expected.ApprovalProofDigest != "" && a.Approval.ProofDigest != expected.ApprovalProofDigest {
		return VerifiedArtifact{}, fail("approval_proof", ErrInvalid)
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
	// Freshness is validated last, after every authenticity check (bindings,
	// signing-key record/status/validity, and signature). An ErrExpired result
	// therefore proves the artifact is otherwise fully authentic against the
	// current policy — a revoked key or tampered signature returns ErrInvalid
	// here, never ErrExpired — so callers may treat a lapsed lifetime, and only a
	// lapsed lifetime, as non-fatal for an already-applied no-op.
	if now.Before(a.CreatedAt) || !now.Before(a.ExpiresAt) {
		return VerifiedArtifact{}, fail("time", ErrExpired)
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
	if a.Version != Version || a.SourceRevision == "" || a.TargetEnvironment == "" || a.DatabaseIdentity == "" || a.GuardrailDigest == "" || a.Approval.Identity == "" || a.Metadata == nil || a.CreatedAt.IsZero() || !a.ExpiresAt.After(a.CreatedAt) || a.Approval.ApprovedAt.IsZero() || a.Approval.ApprovedAt.After(a.CreatedAt) || a.CreatedAt.Location() != time.UTC || a.ExpiresAt.Location() != time.UTC || a.Approval.ApprovedAt.Location() != time.UTC || (a.Origin.Kind != "unattested" && a.Origin.Kind != "generated" && a.Origin.Kind != "edited") || a.Origin.PlanDigest != a.Plan.Digest || a.Origin.Generator == "" || a.Origin.GeneratorHash != originHash(a.Origin) {
		return fmt.Errorf("%w: required metadata", ErrInvalid)
	}
	if err := a.Plan.Validate(); err != nil {
		return fmt.Errorf("%w: plan: %v", ErrInvalid, err)
	}
	if a.EditProvenance != nil {
		if err := validateEditProvenance(a); err != nil {
			return err
		}
	}
	if a.Origin.Kind == "generated" && len(a.ValidationAttestations) > 0 {
		seen := map[string]bool{}
		for _, v := range a.ValidationAttestations {
			if seen[v.Stage] || v.Implementation == "" || v.Version == "" || !digestPattern.MatchString(v.ConfigDigest) || !digestPattern.MatchString(v.ResultDigest) || v.At.IsZero() || v.At.Location() != time.UTC || !v.ExpiresAt.After(v.At) {
				return fail("generated_attestation", ErrInvalid)
			}
			seen[v.Stage] = true
			switch v.Stage {
			case "replay_simulation":
				if v.Simulation == nil || v.Simulation.TargetIdentity == v.Simulation.DevelopmentIdentity || v.Simulation.FromFingerprint == "" || v.Simulation.ToFingerprint == "" || v.Simulation.ConfigDigest != v.ConfigDigest {
					return fail("generated_attestation", ErrInvalid)
				}
			case "safety":
				if v.Safety == nil || len(v.Safety.Analyzers) == 0 || v.Safety.ConfigDigest != v.ConfigDigest {
					return fail("generated_attestation", ErrInvalid)
				}
			case "policy_precheck_guardrail":
				if v.Policy == nil || v.Precheck == nil || v.Policy.ConfigDigest != v.ConfigDigest || v.Precheck.ConfigDigest != v.ConfigDigest {
					return fail("generated_attestation", ErrInvalid)
				}
			default:
				return fail("generated_attestation", ErrInvalid)
			}
		}
		if len(seen) != 3 {
			return fail("generated_attestation", ErrInvalid)
		}
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

func originHash(o ArtifactOrigin) string {
	o.GeneratorHash = ""
	o.Signature = ""
	b, _ := json.Marshal(o)
	s := sha256.Sum256(append([]byte("autosql.artifact.origin/v1\x00"), b...))
	return "sha256:" + hex.EncodeToString(s[:])
}
func editHash(domain string, b []byte) string {
	s := sha256.Sum256(append([]byte("autosql.edit-provenance."+domain+"/v1\x00"), b...))
	return "sha256:" + hex.EncodeToString(s[:])
}
func validateEditProvenance(a Artifact) error {
	p := a.EditProvenance
	if p.Version != "autosql.edit-provenance/v1" || p.OriginalLength != len(p.OriginalArtifact) || p.OriginalBytesDigest != editHash("original-bytes", p.OriginalArtifact) || p.CandidatePlanDigest != a.Plan.Digest {
		return fail("edit_provenance", ErrInvalid)
	}
	original, err := Parse(p.OriginalArtifact)
	if err != nil || original.Digest != p.OriginalArtifactDigest || original.Plan.Digest != p.OriginalPlanDigest {
		return fail("edit_original", ErrInvalid)
	}
	sigRaw, _ := json.Marshal(original.Signature)
	if p.OriginalSignatureDigest != editHash("signature", sigRaw) {
		return fail("edit_signature", ErrInvalid)
	}
	planRaw, _ := a.Plan.MarshalCanonical()
	if p.CandidateBytesDigest != editHash("candidate", planRaw) {
		return fail("edit_candidate", ErrInvalid)
	}
	parent := p.OriginalPlanDigest
	for _, r := range p.Records {
		copy := r
		copy.Digest = ""
		raw, _ := json.Marshal(copy)
		if r.ParentDigest != parent || r.Digest != editHash("record", raw) || r.EditorIdentity == "" || r.Reason == "" || r.Source == "" || r.EditedAt.IsZero() || r.EditedAt.Location() != time.UTC {
			return fail("edit_chain", ErrInvalid)
		}
		parent = r.Digest
	}
	if len(p.Records) == 0 || p.ChainDigest != parent {
		return fail("edit_chain", ErrInvalid)
	}
	for _, v := range p.Attestations {
		if v.Stage == "" || v.Implementation == "" || v.Version == "" || !digestPattern.MatchString(v.ConfigDigest) || !digestPattern.MatchString(v.ResultDigest) || v.At.IsZero() || v.At.Location() != time.UTC || !v.ExpiresAt.After(v.At) || v.ExpiresAt.Location() != time.UTC {
			return fail("edit_attestation", ErrInvalid)
		}
		switch v.Stage {
		case "parse_rebind":
			if v.Editor == nil || v.Editor.Identity == "" || !digestPattern.MatchString(v.Editor.ReasonDigest) || v.Editor.ChainDigest != p.ChainDigest || v.Editor.ConfigDigest != v.ConfigDigest {
				return fail("edit_attestation", ErrInvalid)
			}
		case "simulation":
			if v.Simulation == nil || v.Simulation.TargetIdentity == "" || v.Simulation.DevelopmentIdentity == "" || v.Simulation.TargetIdentity == v.Simulation.DevelopmentIdentity || v.Simulation.FromFingerprint == "" || v.Simulation.ToFingerprint == "" || v.Simulation.ConfigDigest != v.ConfigDigest {
				return fail("edit_attestation", ErrInvalid)
			}
		case "safety":
			if v.Safety == nil || len(v.Safety.Analyzers) == 0 || v.Safety.Threshold == "" || v.Safety.ConfigDigest != v.ConfigDigest {
				return fail("edit_attestation", ErrInvalid)
			}
		case "policy_precheck_guardrail":
			if v.Policy == nil || v.Precheck == nil || v.Policy.ConfigDigest != v.ConfigDigest || v.Precheck.ConfigDigest != v.ConfigDigest || (!digestPattern.MatchString(v.Precheck.ChecksDigest) && !rawDigestPattern.MatchString(v.Precheck.ChecksDigest)) || !digestPattern.MatchString(v.Precheck.GuardrailDigest) {
				return fail("edit_attestation", ErrInvalid)
			}
		default:
			return fail("edit_attestation", ErrInvalid)
		}
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

// maxEncodedArtifactSize bounds parser allocation while leaving room for
// complete database bootstrap plans. Those plans intentionally carry the SQL,
// checks, resource specifications, comments, and dependency metadata needed to
// verify the exact generated artifact before mutation.
const maxEncodedArtifactSize = 8 << 20

func Parse(data []byte) (Artifact, error) {
	if len(data) > maxEncodedArtifactSize || !utf8.Valid(data) || jsonDepthAndDuplicates(data) != nil {
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
