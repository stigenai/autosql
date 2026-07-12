package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/guardrail"
	"autosql/pkg/plan"
	"autosql/pkg/policy"
	"autosql/pkg/postgres"
	"autosql/pkg/safety"
	"autosql/pkg/schema"
	"autosql/pkg/secret"
	"autosql/pkg/simulate"
)

type applyConfig struct {
	DatabaseURL, Environment, DatabaseIdentity, SourceRevision, KeyID, PublicKey, Issuer, Signer, Author, Requester, ApprovalAuditPath, LifecycleAuditPath, ArtifactDirectory string
	PostgresVersion                                                                                                                                                           int
	Schemas                                                                                                                                                                   []string
	ExpectedPlanDigest, ExpectedChecksDigest, ExpectedGuardrailDigest, ExpectedApprovalIdentity, ExpectedApprovalProofDigest, KeyStatus, KeyPurpose                           string
	KeyNotBefore, KeyNotAfter                                                                                                                                                 time.Time
	NoEdits                                                                                                                                                                   bool
	GeneratorKeyID, GeneratorPublicKey, GeneratorPurpose                                                                                                                      string
	ExpectedValidationContextDigests                                                                                                                                          map[string]string
	ExpectedValidationAttestations                                                                                                                                            map[string]artifact.ValidationAttestation
	EditorIdentity, EditSigningKeyID, EditSigningKeyReference, DevelopmentURLReference, FreshApprovalIdentity, FreshApprovalProofDigest                                       string
	FreshApprovalAt, EditReleaseCreatedAt, EditReleaseExpiresAt                                                                                                               time.Time
	TrustedMigrations                                                                                                                                                         map[string]migrationTrust
}
type migrationTrust struct {
	Expected                 artifact.ExpectedBindings
	ValidationContextDigests map[string]string
	ValidationAttestations   map[string]artifact.ValidationAttestation
}
type staticAuthority struct{ actors map[string]approval.Identity }

func (a staticAuthority) ResolveActor(_ context.Context, id string) (approval.Identity, error) {
	v, ok := a.actors[id]
	if !ok {
		return v, errors.New("untrusted actor")
	}
	return v, nil
}
func (staticAuthority) VerifyApproval(context.Context, approval.Approval) (approval.VerifiedApproval, error) {
	return approval.VerifiedApproval{}, errors.New("external approvals disabled")
}

// ProductionServices loads the shipped binary apply boundary from AUTOSQL_APPLY_CONFIG.
func ProductionServices() (Services, error) {
	return productionServices(executor.PGXConnector{})
}

func productionServices(connector executor.Connector) (Services, error) {
	path := os.Getenv("AUTOSQL_APPLY_CONFIG")
	if path == "" {
		return Services{ReadPlan: DefaultReadPlan{}, Apply: refusingApply{}}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Services{}, errors.New("read apply configuration")
	}
	var c applyConfig
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return Services{}, errors.New("parse apply configuration")
	}
	pub, err := base64.RawStdEncoding.DecodeString(c.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return Services{}, errors.New("invalid apply public key")
	}
	ref := secret.Reference(c.DatabaseURL)
	if err = ref.Validate(); err != nil {
		return Services{}, errors.New("invalid database URL reference")
	}
	resolver := secret.NewResolver()
	url, err := resolver.Resolve(context.Background(), ref)
	if err != nil {
		return Services{}, errors.New("resolve database URL")
	}
	authority := staticAuthority{actors: map[string]approval.Identity{c.Author: {ID: c.Author}, c.Requester: {ID: c.Requester}}}
	ap := approval.Policy{Environments: map[string]approval.EnvironmentPolicy{c.Environment: {Allowed: true}}}
	g := guardrail.Guardrail{Config: guardrail.Config{Environment: c.Environment, FailOn: safety.SeverityError, Risk: guardrail.RiskConfig{Baseline: approval.RiskLow}}, Safety: safety.Runner{Analyzers: safety.Builtins()}, Policy: policy.Evaluator{}, Approval: approval.Gate{Policy: ap, Authority: authority, Audit: &approval.Chain{Sink: &approval.FileSink{Path: c.ApprovalAuditPath}}}}
	policyFor := func(a artifact.Artifact) (artifact.VerifyPolicy, error) {
		expected := artifact.ExpectedBindings{PlanDigest: c.ExpectedPlanDigest, GeneratedPlanDigest: c.ExpectedPlanDigest, ChecksDigest: c.ExpectedChecksDigest, GuardrailDigest: c.ExpectedGuardrailDigest, SourceRevision: c.SourceRevision, Environment: c.Environment, DatabaseIdentity: c.DatabaseIdentity, ApprovalIdentity: c.ExpectedApprovalIdentity, ApprovalProofDigest: c.ExpectedApprovalProofDigest}
		contexts, attestations := c.ExpectedValidationContextDigests, c.ExpectedValidationAttestations
		if len(c.TrustedMigrations) > 0 {
			trusted, ok := c.TrustedMigrations[a.Digest]
			if !ok {
				return artifact.VerifyPolicy{}, errors.New("artifact absent from trusted migration release manifest")
			}
			expected = trusted.Expected
			contexts, attestations = trusted.ValidationContextDigests, trusted.ValidationAttestations
		}
		if expected.PlanDigest == "" || expected.ChecksDigest == "" || expected.GuardrailDigest == "" || expected.ApprovalIdentity == "" || expected.SourceRevision == "" || expected.Environment == "" || expected.DatabaseIdentity == "" || c.KeyStatus == "" || c.KeyPurpose == "" || c.KeyNotBefore.IsZero() || c.KeyNotAfter.IsZero() {
			return artifact.VerifyPolicy{}, errors.New("trusted release manifest bindings required")
		}
		vp := artifact.VerifyPolicy{Now: time.Now, NoEdits: c.NoEdits, Expected: expected, Keys: map[string]artifact.KeyRecord{c.KeyID: {PublicKey: ed25519.PublicKey(pub), Issuer: c.Issuer, Identity: c.Signer, Environment: expected.Environment, Purpose: c.KeyPurpose, Status: c.KeyStatus, NotBefore: c.KeyNotBefore.UTC(), NotAfter: c.KeyNotAfter.UTC()}}, Issuer: c.Issuer, Identity: c.Signer, Purpose: c.KeyPurpose}
		vp.ExpectedValidationContextDigests = contexts
		vp.ExpectedValidationAttestations = attestations
		if c.GeneratorKeyID != "" || c.GeneratorPublicKey != "" || c.GeneratorPurpose != "" {
			generatorPub, decodeErr := base64.RawStdEncoding.Strict().DecodeString(c.GeneratorPublicKey)
			if decodeErr != nil || len(generatorPub) != ed25519.PublicKeySize || c.GeneratorKeyID == "" || c.GeneratorPurpose == "" {
				return artifact.VerifyPolicy{}, errors.New("trusted generator manifest required")
			}
			vp.GeneratorPurpose = c.GeneratorPurpose
			vp.GeneratorKeys = map[string]artifact.KeyRecord{c.GeneratorKeyID: {PublicKey: ed25519.PublicKey(generatorPub), Purpose: c.GeneratorPurpose}}
		}
		return vp, nil
	}
	input := func(a artifact.Artifact) (guardrail.Input, error) {
		doc := policy.Document{Version: policy.LanguageVersion, Rules: []policy.Rule{{Name: "configured apply", Target: "all", Assert: policy.Expression{Eq: []any{true, true}}, Message: "apply allowed"}}}
		si := safety.Input{Statements: a.Plan.SafetyStatements(), Target: safety.Target{Engine: "postgresql", Version: c.PostgresVersion}}
		bindings, err := guardrail.BuildStatementBindings(a.Plan.Changes, si.Statements)
		if err != nil {
			return guardrail.Input{}, err
		}
		return guardrail.Input{Changes: a.Plan.Changes, Safety: si, Policy: doc, PolicyIdentity: "production-config/v1", Precheck: a.Checks, Approval: approval.Request{Plan: approval.Plan{Digest: a.GuardrailDigest, Environment: c.Environment, Author: c.Author, ExpiresAt: a.ExpiresAt}, RequestedBy: c.Requester}, StatementBindings: bindings}, nil
	}
	mutationFor := func(v artifact.VerifiedArtifact, locked executor.Session, tx executor.Tx) (guardrail.AuthorizedMutation, error) {
		a, _ := v.Payload()
		state := func(ctx context.Context, conn executor.Session) (executor.RuntimeState, error) {
			doc, err := postgres.InspectConn(ctx, conn.Raw(), postgres.Options{Schemas: c.Schemas})
			if err != nil {
				return executor.RuntimeState{}, err
			}
			fp, err := schema.SemanticFingerprint(doc)
			return executor.RuntimeState{Fingerprint: fp, SourceRevision: c.SourceRevision, Environment: c.Environment, DatabaseIdentity: c.DatabaseIdentity}, err
		}
		last := ""
		for _, s := range a.Plan.Steps {
			if s.Kind == plan.StepExecutable {
				last = s.ID
			}
		}
		return executor.NewPostgreSQL(executor.Config{URL: url, Connector: connector, LockedSession: locked, LockAlreadyHeld: locked != nil, Transaction: tx, NoEdits: c.NoEdits, Audit: &executor.FileAudit{Path: c.LifecycleAuditPath}, Reauthorize: func(ctx context.Context, a artifact.Artifact) error {
			fresh, _ := policyFor(a)
			_, err := a.VerifyTrusted(fresh)
			return err
		}, State: func(ctx context.Context, conn executor.Session) (executor.RuntimeState, error) {
			return state(ctx, conn)
		}, Confirm: func(ctx context.Context, conn executor.Session, s plan.Step) error {
			if s.ID != last {
				return nil
			}
			got, err := state(ctx, conn)
			if err != nil {
				return err
			}
			if !strings.EqualFold(got.Fingerprint, a.Plan.ToFingerprint) {
				return errors.New("target postcondition mismatch")
			}
			return nil
		}}, v)
	}
	mutation := func(v artifact.VerifiedArtifact) (guardrail.AuthorizedMutation, error) {
		return mutationFor(v, nil, nil)
	}
	verified := VerifiedArtifactApplyService{PolicyFor: policyFor, Guardrail: g, Input: input, Mutation: mutation, MutationLocked: mutationFor, NoEdits: c.NoEdits}
	var editService PlanEditService
	if c.EditorIdentity != "" {
		if c.EditorIdentity == c.Author || c.EditorIdentity == c.Requester {
			return Services{}, errors.New("editor must be separated from author and requester")
		}
		keyText, resolveErr := resolver.Resolve(context.Background(), secret.Reference(c.EditSigningKeyReference))
		if resolveErr != nil {
			return Services{}, errors.New("resolve edit signing key")
		}
		private, decodeErr := decodePrivate(keyText)
		if decodeErr != nil {
			return Services{}, decodeErr
		}
		devURL, resolveErr := resolver.Resolve(context.Background(), secret.Reference(c.DevelopmentURLReference))
		if resolveErr != nil {
			return Services{}, errors.New("resolve edit development URL")
		}
		targetID, identityErr := simulate.ResolvePostgresIdentity(context.Background(), url)
		if identityErr != nil {
			return Services{}, errors.New("resolve edit target identity")
		}
		devID, identityErr := simulate.ResolvePostgresIdentity(context.Background(), devURL)
		if identityErr != nil {
			return Services{}, errors.New("resolve edit development identity")
		}
		if targetID == devID {
			return Services{}, errors.New("edit development database must be distinct from target")
		}
		editService = &productionEditService{editor: c.EditorIdentity, policyFor: policyFor, g: g, input: input, url: url, targetIdentity: targetID, developmentURL: devURL, developmentIdentity: devID, revision: c.SourceRevision, environment: c.Environment, database: c.DatabaseIdentity, keyID: c.EditSigningKeyID, version: c.PostgresVersion, private: private, approval: artifact.Approval{Identity: c.FreshApprovalIdentity, ApprovedAt: c.FreshApprovalAt.UTC(), ProofDigest: c.FreshApprovalProofDigest}, created: c.EditReleaseCreatedAt.UTC(), expires: c.EditReleaseExpiresAt.UTC(), audit: &executor.FileAudit{Path: c.LifecycleAuditPath}, schemas: append([]string(nil), c.Schemas...)}
	}
	return Services{ReadPlan: DefaultReadPlan{}, Apply: resolvingApply{verified: verified, directory: c.ArtifactDirectory}, PlanEdit: editService}, nil
}

type resolvingApply struct {
	verified  VerifiedArtifactApplyService
	directory string
}

func (s resolvingApply) VerifyArtifact(a artifact.Artifact) (artifact.VerifiedArtifact, error) {
	return s.verified.VerifyArtifact(a)
}
func (s resolvingApply) ApplyVersioned(ctx context.Context, v artifact.VerifiedArtifact, session executor.Session, tx executor.Tx) (executor.Result, error) {
	return s.verified.ApplyVersioned(ctx, v, session, tx)
}

func (s resolvingApply) Apply(ctx context.Context, r ApplyRequest) (ApplyResult, error) {
	if r.ApprovalMode == "digest" {
		if s.directory == "" || r.AssertedDigest == "" {
			return ApplyResult{Status: "refused"}, errors.New("signed artifact directory required")
		}
		r.ArtifactPath = filepath.Join(s.directory, r.AssertedDigest+".json")
		r.ApprovalMode = "artifact"
	}
	return s.verified.Apply(ctx, r)
}

type refusingApply struct{}

func (refusingApply) Apply(context.Context, ApplyRequest) (ApplyResult, error) {
	return ApplyResult{Status: "refused"}, errors.New("production apply configuration is not installed")
}
