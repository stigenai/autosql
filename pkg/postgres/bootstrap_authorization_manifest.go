package postgres

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
	"sort"
	"time"
	"unicode/utf8"

	"autosql/pkg/artifact"
	"autosql/pkg/bootstrap"
	"autosql/pkg/plan"
	"autosql/pkg/schema"
)

const BootstrapAuthorizationManifestVersion = "autosql.bootstrap-authorization-manifest/v1"

const bootstrapAuthorizationSignatureDomain = "autosql.bootstrap-authorization.signature/v1\x00"

var ErrInvalidBootstrapAuthorization = errors.New("invalid bootstrap authorization manifest")
var ErrExpiredBootstrapAuthorization = errors.New("bootstrap authorization manifest is outside its validity window")
var ErrStaleBootstrapAuthorization = errors.New("bootstrap authorization manifest is stale for the current plan")

type BootstrapRoutineApproval struct {
	ResourceID                   string `json:"resource_id"`
	Kind                         string `json:"kind"`
	Signature                    string `json:"signature"`
	SourceDigest                 string `json:"source_digest"`
	DigestReviewed               bool   `json:"digest_reviewed"`
	UnsafeLanguageAuthorized     bool   `json:"unsafe_language_authorized"`
	PrivilegedRoutineAuthorized  bool   `json:"privileged_routine_authorized"`
	TransactionControlAuthorized bool   `json:"transaction_control_authorized"`
}

type BootstrapExtensionApproval struct {
	ResourceID          string   `json:"resource_id"`
	Name                string   `json:"name"`
	Version             string   `json:"version"`
	Schema              string   `json:"schema"`
	Requires            []string `json:"requires,omitempty"`
	Allowlisted         bool     `json:"allowlisted"`
	Trusted             bool     `json:"trusted"`
	SuperuserRequired   bool     `json:"superuser_required"`
	UntrustedAuthorized bool     `json:"untrusted_authorized"`
}

// BootstrapAuthorizationManifest is a signed authorization-only artifact. It
// deliberately contains no executable plan, SQL, credentials, or routine
// source and is bound to the non-executable inventory identity.
type BootstrapAuthorizationManifest struct {
	Version          string                       `json:"version"`
	PlanDigest       string                       `json:"plan_digest"`
	SchemaPlanDigest string                       `json:"schema_plan_digest"`
	SourceDigest     string                       `json:"source_digest"`
	Database         string                       `json:"database"`
	Issuer           string                       `json:"issuer"`
	Signer           string                       `json:"signer"`
	Purpose          string                       `json:"purpose"`
	IssuedAt         time.Time                    `json:"issued_at"`
	NotBefore        time.Time                    `json:"not_before"`
	ExpiresAt        time.Time                    `json:"expires_at"`
	Routines         []BootstrapRoutineApproval   `json:"routines"`
	Extensions       []BootstrapExtensionApproval `json:"extensions"`
	Digest           string                       `json:"digest"`
	Signature        artifact.Signature           `json:"signature"`
}

type BootstrapAuthorizationVerifyPolicy struct {
	Now                     func() time.Time
	Keys                    map[string]artifact.KeyRecord
	Issuer, Signer, Purpose string
}

type VerifiedBootstrapAuthorization struct {
	manifest BootstrapAuthorizationManifest
	marker   [32]byte
}

func NewBootstrapAuthorizationManifest(inventory BootstrapAuthorizationInventory, issuedAt, notBefore, expiresAt time.Time, issuer, signer, purpose string) (BootstrapAuthorizationManifest, error) {
	if err := inventory.Validate(); err != nil {
		return BootstrapAuthorizationManifest{}, fmt.Errorf("%w: inventory", ErrInvalidBootstrapAuthorization)
	}
	manifest := BootstrapAuthorizationManifest{
		Version: BootstrapAuthorizationManifestVersion, PlanDigest: inventory.PlanDigest, SchemaPlanDigest: inventory.PlanSummary.SchemaPlanDigest,
		SourceDigest: inventory.SourceDigest, Database: inventory.Database, Issuer: issuer, Signer: signer, Purpose: purpose,
		IssuedAt: issuedAt.UTC(), NotBefore: notBefore.UTC(), ExpiresAt: expiresAt.UTC(),
		Routines: []BootstrapRoutineApproval{}, Extensions: []BootstrapExtensionApproval{},
	}
	for _, routine := range inventory.Routines {
		manifest.Routines = append(manifest.Routines, BootstrapRoutineApproval{
			ResourceID: routine.ResourceID, Kind: routine.Kind, Signature: routine.Signature, SourceDigest: routine.SourceDigest,
			DigestReviewed: true, UnsafeLanguageAuthorized: routine.UnsafeLanguageAuthorizationRequired,
			PrivilegedRoutineAuthorized:  routine.PrivilegedRoutineAuthorizationRequired,
			TransactionControlAuthorized: routine.TransactionControlAuthorizationRequired,
		})
	}
	for _, extension := range inventory.Extensions {
		manifest.Extensions = append(manifest.Extensions, BootstrapExtensionApproval{
			ResourceID: extension.ResourceID, Name: extension.Name, Version: extension.Version, Schema: extension.Schema,
			Requires: append([]string(nil), extension.Requires...), Allowlisted: true, Trusted: extension.Trusted,
			SuperuserRequired: extension.SuperuserRequired, UntrustedAuthorized: extension.UntrustedExtensionAuthorizationRequired,
		})
	}
	normalizeBootstrapAuthorizationManifest(&manifest)
	if err := manifest.validateUnsigned(); err != nil {
		return BootstrapAuthorizationManifest{}, err
	}
	return manifest, nil
}

func (m *BootstrapAuthorizationManifest) Sign(keyID string, private ed25519.PrivateKey) error {
	if keyID == "" || len(private) != ed25519.PrivateKeySize {
		return fmt.Errorf("%w: signing key", ErrInvalidBootstrapAuthorization)
	}
	m.Signature = artifact.Signature{KeyID: keyID, Algorithm: "Ed25519"}
	if err := m.validateUnsigned(); err != nil {
		return err
	}
	digest, err := bootstrapAuthorizationManifestDigest(*m)
	if err != nil {
		return err
	}
	m.Digest = digest
	m.Signature.Value = base64.RawStdEncoding.EncodeToString(ed25519.Sign(private, []byte(bootstrapAuthorizationSignatureDomain+digest)))
	return nil
}

func (m BootstrapAuthorizationManifest) MarshalCanonical() ([]byte, error) {
	if err := m.validateStored(); err != nil {
		return nil, err
	}
	raw, err := json.Marshal(m)
	if err != nil {
		return nil, err
	}
	return append(raw, '\n'), nil
}

func ParseBootstrapAuthorizationManifest(raw []byte) (BootstrapAuthorizationManifest, error) {
	if len(raw) == 0 || len(raw) > 4<<20 || !utf8.Valid(raw) || bootstrapAuthorizationJSONGuard(raw) != nil {
		return BootstrapAuthorizationManifest{}, fmt.Errorf("%w: encoding", ErrInvalidBootstrapAuthorization)
	}
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	var manifest BootstrapAuthorizationManifest
	if err := decoder.Decode(&manifest); err != nil {
		return BootstrapAuthorizationManifest{}, fmt.Errorf("%w: parse", ErrInvalidBootstrapAuthorization)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		return BootstrapAuthorizationManifest{}, fmt.Errorf("%w: trailing data", ErrInvalidBootstrapAuthorization)
	}
	if err := manifest.validateStored(); err != nil {
		return BootstrapAuthorizationManifest{}, err
	}
	canonical, err := manifest.MarshalCanonical()
	if err != nil || !bytes.Equal(canonical, raw) {
		return BootstrapAuthorizationManifest{}, fmt.Errorf("%w: canonical encoding", ErrInvalidBootstrapAuthorization)
	}
	return manifest, nil
}

func bootstrapAuthorizationJSONGuard(raw []byte) error {
	decoder := json.NewDecoder(bytes.NewReader(raw))
	if err := bootstrapAuthorizationJSONValue(decoder, 0); err != nil {
		return err
	}
	if _, err := decoder.Token(); err != io.EOF {
		return ErrInvalidBootstrapAuthorization
	}
	return nil
}

func bootstrapAuthorizationJSONValue(decoder *json.Decoder, depth int) error {
	token, err := decoder.Token()
	if err != nil {
		return ErrInvalidBootstrapAuthorization
	}
	delimiter, ok := token.(json.Delim)
	if !ok {
		return nil
	}
	if delimiter != '{' && delimiter != '[' {
		return ErrInvalidBootstrapAuthorization
	}
	depth++
	if depth > 64 {
		return ErrInvalidBootstrapAuthorization
	}
	if delimiter == '{' {
		keys := map[string]bool{}
		for decoder.More() {
			keyToken, err := decoder.Token()
			key, ok := keyToken.(string)
			if err != nil || !ok || keys[key] {
				return ErrInvalidBootstrapAuthorization
			}
			keys[key] = true
			if err := bootstrapAuthorizationJSONValue(decoder, depth); err != nil {
				return err
			}
		}
	} else {
		for decoder.More() {
			if err := bootstrapAuthorizationJSONValue(decoder, depth); err != nil {
				return err
			}
		}
	}
	closing, err := decoder.Token()
	if err != nil {
		return ErrInvalidBootstrapAuthorization
	}
	want := json.Delim('}')
	if delimiter == '[' {
		want = ']'
	}
	if closing != want {
		return ErrInvalidBootstrapAuthorization
	}
	return nil
}

func VerifyBootstrapAuthorizationManifest(manifest BootstrapAuthorizationManifest, inventory BootstrapAuthorizationInventory, policy BootstrapAuthorizationVerifyPolicy) (VerifiedBootstrapAuthorization, error) {
	if policy.Now == nil || policy.Issuer == "" || policy.Signer == "" || policy.Purpose == "" {
		return VerifiedBootstrapAuthorization{}, fmt.Errorf("%w: verification policy", ErrInvalidBootstrapAuthorization)
	}
	if err := inventory.Validate(); err != nil {
		return VerifiedBootstrapAuthorization{}, fmt.Errorf("%w: inventory", ErrInvalidBootstrapAuthorization)
	}
	if err := manifest.validateStored(); err != nil {
		return VerifiedBootstrapAuthorization{}, err
	}
	want, err := NewBootstrapAuthorizationManifest(inventory, manifest.IssuedAt, manifest.NotBefore, manifest.ExpiresAt, policy.Issuer, policy.Signer, policy.Purpose)
	if err != nil {
		return VerifiedBootstrapAuthorization{}, fmt.Errorf("%w: inventory", ErrInvalidBootstrapAuthorization)
	}
	if manifest.Issuer != policy.Issuer || manifest.Signer != policy.Signer || manifest.Purpose != policy.Purpose {
		return VerifiedBootstrapAuthorization{}, fmt.Errorf("%w: trusted identity policy", ErrInvalidBootstrapAuthorization)
	}
	if manifest.PlanDigest != inventory.PlanDigest || manifest.SchemaPlanDigest != inventory.PlanSummary.SchemaPlanDigest || manifest.SourceDigest != inventory.SourceDigest || manifest.Database != inventory.Database || !reflect.DeepEqual(manifest.Routines, want.Routines) || !reflect.DeepEqual(manifest.Extensions, want.Extensions) {
		return VerifiedBootstrapAuthorization{}, fmt.Errorf("%w: inventory binding", ErrStaleBootstrapAuthorization)
	}
	now := policy.Now().UTC()
	if now.Before(manifest.IssuedAt) || now.Before(manifest.NotBefore) || !now.Before(manifest.ExpiresAt) {
		return VerifiedBootstrapAuthorization{}, ErrExpiredBootstrapAuthorization
	}
	record, ok := policy.Keys[manifest.Signature.KeyID]
	if !ok || len(record.PublicKey) != ed25519.PublicKeySize || record.Status != "active" || record.Issuer != policy.Issuer || record.Identity != policy.Signer || record.Purpose != policy.Purpose || record.NotBefore.Location() != time.UTC || record.NotAfter.Location() != time.UTC || !record.NotAfter.After(record.NotBefore) || now.Before(record.NotBefore) || !now.Before(record.NotAfter) {
		return VerifiedBootstrapAuthorization{}, fmt.Errorf("%w: verification key", ErrInvalidBootstrapAuthorization)
	}
	signature, err := base64.RawStdEncoding.Strict().DecodeString(manifest.Signature.Value)
	if err != nil || len(signature) != ed25519.SignatureSize || !ed25519.Verify(record.PublicKey, []byte(bootstrapAuthorizationSignatureDomain+manifest.Digest), signature) {
		return VerifiedBootstrapAuthorization{}, fmt.Errorf("%w: signature", ErrInvalidBootstrapAuthorization)
	}
	verified := VerifiedBootstrapAuthorization{manifest: manifest}
	verified.marker = bootstrapAuthorizationMarker(manifest)
	return verified, nil
}

// PlanDatabaseBootstrapAuthorized is the only public bridge from a verified
// authorization token to an executable plan. It materializes gate options,
// builds the plan, and rechecks both plan digests before returning, so callers
// cannot reuse a token's options for a different target or source graph.
func PlanDatabaseBootstrapAuthorized(ctx context.Context, target bootstrap.DatabaseTarget, desired schema.Document, options plan.Options, authorization VerifiedBootstrapAuthorization) (bootstrap.Plan, error) {
	render, err := authorization.renderOptions(options.Render)
	if err != nil {
		return bootstrap.Plan{}, err
	}
	options.Render = render
	whole, err := PlanDatabaseBootstrap(ctx, target, desired, options)
	if err != nil {
		return bootstrap.Plan{}, err
	}
	if whole.Digest != authorization.manifest.PlanDigest || whole.SchemaPlan.Digest != authorization.manifest.SchemaPlanDigest {
		return bootstrap.Plan{}, fmt.Errorf("%w: authorized plan binding", ErrStaleBootstrapAuthorization)
	}
	// Replace the global render-time gate with exact per-resource execution
	// capabilities. A signed trusted assertion for one extension cannot borrow
	// another extension's untrusted approval when live control metadata differs.
	whole = whole.WithRuntimeAuthorization(nil)
	for _, extension := range authorization.manifest.Extensions {
		if extension.UntrustedAuthorized {
			whole = whole.AddRuntimeAuthorization(sealBootstrapExtensionAuthorization(whole.Digest, extension.ResourceID))
		}
	}
	return whole, nil
}

func (v VerifiedBootstrapAuthorization) renderOptions(base map[string]string) (map[string]string, error) {
	if v.marker != bootstrapAuthorizationMarker(v.manifest) || v.manifest.Digest == "" {
		return nil, fmt.Errorf("%w: verified token", ErrInvalidBootstrapAuthorization)
	}
	render := cloneRenderOptions(base)
	var digests, extensionNames []string
	for _, routine := range v.manifest.Routines {
		digests = append(digests, routine.SourceDigest)
		if routine.UnsafeLanguageAuthorized {
			render["allow_unsafe_routine_languages"] = "true"
		}
		if routine.PrivilegedRoutineAuthorized {
			render["allow_privileged_routines"] = "true"
		}
		if routine.TransactionControlAuthorized {
			render["allow_transaction_control_procedures"] = "true"
		}
	}
	for _, extension := range v.manifest.Extensions {
		extensionNames = append(extensionNames, extension.Name)
		render["extension_version."+extension.Name] = extension.Version
		render["extension_schemas."+extension.Name] = extension.Schema
		if extension.UntrustedAuthorized {
			render["allow_untrusted_extensions"] = "true"
		}
	}
	render["reviewed_routine_digests"] = joinCanonicalValues(digests)
	render["extension_allowlist"] = joinCanonicalValues(extensionNames)
	return render, nil
}

func (m BootstrapAuthorizationManifest) validateUnsigned() error {
	if m.Version != BootstrapAuthorizationManifestVersion || !canonicalSHA256Digest(m.PlanDigest) || !canonicalSHA256Digest(m.SchemaPlanDigest) || !canonicalSHA256Digest(m.SourceDigest) || m.Database == "" || m.Issuer == "" || m.Signer == "" || m.Purpose == "" || m.IssuedAt.IsZero() || m.NotBefore.IsZero() || m.ExpiresAt.IsZero() || m.IssuedAt.Location() != time.UTC || m.NotBefore.Location() != time.UTC || m.ExpiresAt.Location() != time.UTC || m.IssuedAt.After(m.ExpiresAt) || !m.ExpiresAt.After(m.NotBefore) {
		return fmt.Errorf("%w: required metadata", ErrInvalidBootstrapAuthorization)
	}
	seenRoutines := map[string]bool{}
	for _, routine := range m.Routines {
		if routine.ResourceID == "" || routine.Kind == "" || routine.Signature == "" || !canonicalSHA256Digest(routine.SourceDigest) || !routine.DigestReviewed || seenRoutines[routine.ResourceID] {
			return fmt.Errorf("%w: routine approval", ErrInvalidBootstrapAuthorization)
		}
		seenRoutines[routine.ResourceID] = true
	}
	seenExtensions := map[string]bool{}
	for _, extension := range m.Extensions {
		if extension.ResourceID == "" || extension.Name == "" || extension.Version == "" || extension.Schema == "" || !extension.Allowlisted || seenExtensions[extension.ResourceID] {
			return fmt.Errorf("%w: extension approval", ErrInvalidBootstrapAuthorization)
		}
		seenExtensions[extension.ResourceID] = true
	}
	return nil
}

func (m BootstrapAuthorizationManifest) validateStored() error {
	if err := m.validateUnsigned(); err != nil {
		return err
	}
	if m.Signature.KeyID == "" || m.Signature.Algorithm != "Ed25519" || m.Signature.Value == "" || !canonicalSHA256Digest(m.Digest) {
		return fmt.Errorf("%w: signature metadata", ErrInvalidBootstrapAuthorization)
	}
	want, err := bootstrapAuthorizationManifestDigest(m)
	if err != nil || want != m.Digest {
		return fmt.Errorf("%w: digest", ErrInvalidBootstrapAuthorization)
	}
	return nil
}

func normalizeBootstrapAuthorizationManifest(m *BootstrapAuthorizationManifest) {
	sort.Slice(m.Routines, func(i, j int) bool { return m.Routines[i].ResourceID < m.Routines[j].ResourceID })
	for i := range m.Extensions {
		m.Extensions[i].Requires = uniqueNonEmptySorted(m.Extensions[i].Requires)
	}
	sort.Slice(m.Extensions, func(i, j int) bool { return m.Extensions[i].ResourceID < m.Extensions[j].ResourceID })
}

func bootstrapAuthorizationManifestDigest(manifest BootstrapAuthorizationManifest) (string, error) {
	copy := manifest
	copy.Digest = ""
	copy.Signature.Value = ""
	raw, err := json.Marshal(copy)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte("autosql.bootstrap-authorization.manifest/v1\x00"), raw...))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func bootstrapAuthorizationMarker(manifest BootstrapAuthorizationManifest) [32]byte {
	raw, _ := manifest.MarshalCanonical()
	return sha256.Sum256(append([]byte("autosql.bootstrap-authorization.verified/v1\x00"), raw...))
}

func joinCanonicalValues(values []string) string {
	values = uniqueNonEmptySorted(values)
	if len(values) == 0 {
		return ""
	}
	result := values[0]
	for _, value := range values[1:] {
		result += "," + value
	}
	return result
}
