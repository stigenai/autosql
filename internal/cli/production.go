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
	"sync"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/guardrail"
	"autosql/pkg/migrate/repair"
	"autosql/pkg/migrate/revision"
	"autosql/pkg/plan"
	"autosql/pkg/policy"
	"autosql/pkg/postgres"
	"autosql/pkg/safety"
	"autosql/pkg/schema"
	"autosql/pkg/secret"
	"autosql/pkg/simulate"
	"autosql/pkg/workloadidentity"
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
	DownConfigPath                                                                                                                                                            string
	FreshApprovalAt, EditReleaseCreatedAt, EditReleaseExpiresAt                                                                                                               time.Time
	TrustedMigrations                                                                                                                                                         map[string]migrationTrust
	ApprovalPolicy                                                                                                                                                            approval.Policy
	RepairPolicyDigest, RepairApprovalDigest, RepairDestructiveApprovalDigest                                                                                                 string
	WorkloadIdentity                                                                                                                                                          *workloadidentity.Binding
}
type migrationTrust struct {
	Expected                 artifact.ExpectedBindings
	ValidationContextDigests map[string]string
	ValidationAttestations   map[string]artifact.ValidationAttestation
	Schemas                  []string
	Policy                   policy.Document
	PolicyIdentity           string
	SchemaPolicyResources    []policy.Resource
	MigrationPolicyResources []policy.Resource
	ApprovalIdentities       map[string]approval.Identity
}
type staticAuthority struct {
	actors   map[string]approval.Identity
	verified map[string]approval.Identity
}

func (a staticAuthority) ResolveActor(_ context.Context, id string) (approval.Identity, error) {
	v, ok := a.actors[id]
	if !ok {
		return v, errors.New("untrusted actor")
	}
	return v, nil
}
func (a staticAuthority) VerifyApproval(_ context.Context, item approval.Approval) (approval.VerifiedApproval, error) {
	identity, ok := a.verified[item.Proof]
	if !ok || identity.ID != item.Approver {
		return approval.VerifiedApproval{}, errors.New("external approval is not release-bound")
	}
	return approval.VerifiedApproval{Identity: identity, PlanDigest: item.PlanDigest, Environment: item.Environment, ApprovedAt: item.ApprovedAt.UTC(), ExpiresAt: item.ExpiresAt.UTC()}, nil
}

// ProductionServices loads the shipped binary apply boundary from AUTOSQL_APPLY_CONFIG.
func ProductionServices() (Services, error) {
	return productionServices(executor.PGXConnector{})
}

// ProductionServicesForURL uses the same signed-artifact guardrail and
// lifecycle audit configuration as ProductionServices, while targeting a
// controller-resolved database URL for this invocation.
func ProductionServicesForURL(databaseURL string) (Services, error) {
	if strings.TrimSpace(databaseURL) == "" {
		return Services{}, errors.New("database URL is required")
	}
	return productionServicesWithURL(executor.PGXConnector{}, databaseURL)
}

// VerifyProductionArtifactForApply verifies a release artifact through the
// exact production service policy without resolving or opening a database
// credential. This is used by controllers that must authenticate immutable
// release input before reading runtime Secrets.
func VerifyProductionArtifactForApply(a artifact.Artifact, mandatoryNoEdits bool) (artifact.VerifiedArtifact, error) {
	services, err := ProductionServicesForURL("postgres://autosql-verification.invalid/postgres")
	if err != nil {
		return artifact.VerifiedArtifact{}, err
	}
	verifier, ok := services.Apply.(interface {
		VerifyArtifactForApply(artifact.Artifact, bool) (artifact.VerifiedArtifact, error)
	})
	if !ok {
		return artifact.VerifiedArtifact{}, errors.New("production artifact verification service is unavailable")
	}
	return verifier.VerifyArtifactForApply(a, mandatoryNoEdits)
}

func productionServices(connector executor.Connector) (Services, error) {
	return productionServicesWithURL(connector, "")
}

var resolveProductionWorkloadIdentity = func(ctx context.Context, binding workloadidentity.Binding) (string, error) {
	var source *workloadidentity.Source
	var err error
	switch binding.Provider {
	case workloadidentity.AWSRDS:
		source, err = workloadidentity.NewAWS(ctx, binding)
	case workloadidentity.GCPCloud:
		source, err = workloadidentity.NewGCP(ctx, binding)
	case workloadidentity.AzurePG:
		source, err = workloadidentity.NewAzure(binding)
	default:
		err = workloadidentity.ErrIdentity
	}
	if err != nil {
		return "", workloadidentity.ErrIdentity
	}
	url, _, err := source.ConnectionURL(ctx)
	if err != nil {
		return "", workloadidentity.ErrIdentity
	}
	return url, nil
}

func resolveApplyDatabaseURL(ctx context.Context, c applyConfig, override string, resolver *secret.Resolver) (string, error) {
	if strings.TrimSpace(override) != "" {
		return override, nil
	}
	if (strings.TrimSpace(c.DatabaseURL) == "") == (c.WorkloadIdentity == nil) {
		return "", errors.New("exactly one database credential source is required")
	}
	if c.WorkloadIdentity != nil {
		url, err := resolveProductionWorkloadIdentity(ctx, *c.WorkloadIdentity)
		if err != nil {
			return "", errors.New("resolve database workload identity")
		}
		if resolver != nil && resolver.Redactor != nil {
			resolver.Redactor.Add(url)
		}
		return url, nil
	}
	ref := secret.Reference(c.DatabaseURL)
	if err := ref.Validate(); err != nil {
		return "", errors.New("invalid database URL reference")
	}
	url, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return "", errors.New("resolve database URL")
	}
	return url, nil
}

func productionServicesWithURL(connector executor.Connector, databaseURLOverride string) (Services, error) {
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
	d.UseNumber()
	if err = d.Decode(&c); err != nil {
		return Services{}, errors.New("parse apply configuration")
	}
	pub, err := base64.RawStdEncoding.DecodeString(c.PublicKey)
	if err != nil || len(pub) != ed25519.PublicKeySize {
		return Services{}, errors.New("invalid apply public key")
	}
	resolver := secret.NewResolver()
	url, err := resolveApplyDatabaseURL(context.Background(), c, databaseURLOverride, resolver)
	if err != nil {
		return Services{}, err
	}
	authority := staticAuthority{actors: map[string]approval.Identity{c.Author: {ID: c.Author}, c.Requester: {ID: c.Requester}}, verified: map[string]approval.Identity{}}
	for _, trusted := range c.TrustedMigrations {
		for proof, identity := range trusted.ApprovalIdentities {
			authority.verified[proof] = identity
			authority.actors[identity.ID] = identity
		}
	}
	ap := c.ApprovalPolicy
	if len(ap.Environments) == 0 {
		ap = approval.Policy{Environments: map[string]approval.EnvironmentPolicy{c.Environment: {Allowed: true}}}
	}
	g := guardrail.Guardrail{Config: guardrail.Config{Environment: c.Environment, FailOn: safety.SeverityError, Risk: guardrail.RiskConfig{Baseline: approval.RiskLow}}, Safety: safety.Runner{Analyzers: safety.Builtins()}, Policy: policy.Evaluator{}, Approval: approval.Gate{Policy: ap, Authority: authority, Audit: &approval.Chain{Sink: &approval.FileSink{Path: c.ApprovalAuditPath}}}}
	dynamicPolicies := map[string]artifact.VerifyPolicy{}
	var dynamicPolicyMu sync.RWMutex
	policyFor := func(a artifact.Artifact) (artifact.VerifyPolicy, error) {
		dynamicPolicyMu.RLock()
		dynamic, dynamicOK := dynamicPolicies[a.Digest]
		dynamicPolicyMu.RUnlock()
		if dynamicOK {
			return dynamic, nil
		}
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
		policyIdentity := "production-config/v1"
		var schemaResources, migrationResources []policy.Resource
		if trusted, ok := c.TrustedMigrations[a.Digest]; ok && len(trusted.Policy.Rules) > 0 {
			doc = trusted.Policy
			policyIdentity = trusted.PolicyIdentity
			schemaResources = append([]policy.Resource(nil), trusted.SchemaPolicyResources...)
			migrationResources = append([]policy.Resource(nil), trusted.MigrationPolicyResources...)
		}
		approvals := []approval.Approval{}
		for _, identity := range strings.Split(a.Approval.Identity, ",") {
			identity = strings.TrimSpace(identity)
			if identity != "" {
				approvals = append(approvals, approval.Approval{Approver: identity, ApprovedAt: a.Approval.ApprovedAt, ExpiresAt: a.ExpiresAt, Proof: a.Approval.ProofDigest, PlanDigest: a.GuardrailDigest, Environment: a.TargetEnvironment})
			}
		}
		si := safety.Input{Statements: a.Plan.SafetyStatements(), Target: safety.Target{Engine: "postgresql", Version: c.PostgresVersion}}
		bindings, err := guardrail.BuildStatementBindings(a.Plan.Changes, si.Statements)
		if err != nil {
			return guardrail.Input{}, err
		}
		return guardrail.Input{Changes: a.Plan.Changes, Safety: si, Policy: doc, PolicyIdentity: policyIdentity, SchemaResources: schemaResources, MigrationResources: migrationResources, Precheck: a.Checks, Approval: approval.Request{Plan: approval.Plan{Digest: a.GuardrailDigest, Environment: a.TargetEnvironment, Author: c.Author, ExpiresAt: a.ExpiresAt}, Approvals: approvals, RequestedBy: c.Requester}, StatementBindings: bindings}, nil
	}
	lifecycle := &executor.FileAudit{Path: c.LifecycleAuditPath}
	mutationForAttempt := func(v artifact.VerifiedArtifact, locked executor.Session, tx executor.Tx, attempt int) (guardrail.AuthorizedMutation, error) {
		a, _ := v.Payload()
		schemas := append([]string(nil), c.Schemas...)
		if trusted, ok := c.TrustedMigrations[a.Digest]; ok && len(trusted.Schemas) > 0 {
			schemas = append([]string(nil), trusted.Schemas...)
		}
		state := func(ctx context.Context, conn executor.Session) (executor.RuntimeState, error) {
			var doc schema.Document
			var err error
			if rawTx := executor.RawPGXTx(tx); rawTx != nil {
				doc, err = postgres.InspectTx(ctx, rawTx, postgres.Options{Schemas: schemas})
			} else {
				doc, err = postgres.InspectConn(ctx, conn.Raw(), postgres.Options{Schemas: schemas})
			}
			if err != nil {
				return executor.RuntimeState{}, err
			}
			doc, err = postgres.New().Normalize(ctx, doc)
			if err != nil {
				return executor.RuntimeState{}, err
			}
			// The executor's migration-history relation is written into the
			// target during apply; it is never part of the desired schema, so
			// exclude it or the postcondition fingerprint can never match the
			// artifact's ToFingerprint once one apply has run.
			doc = executor.ExcludeBookkeeping(doc)
			fp, err := schema.SemanticFingerprint(doc)
			return executor.RuntimeState{Fingerprint: fp, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: a.DatabaseIdentity}, err
		}
		last := ""
		for _, s := range a.Plan.Steps {
			if s.Kind == plan.StepExecutable {
				last = s.ID
			}
		}
		return executor.NewPostgreSQL(executor.Config{URL: url, Connector: connector, LockedSession: locked, LockAlreadyHeld: locked != nil, Transaction: tx, Attempt: attempt, NoEdits: c.NoEdits, Audit: lifecycle, Reauthorize: func(ctx context.Context, a artifact.Artifact) error {
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
	mutationFor := func(v artifact.VerifiedArtifact, locked executor.Session, tx executor.Tx) (guardrail.AuthorizedMutation, error) {
		return mutationForAttempt(v, locked, tx, 1)
	}
	mutation := func(v artifact.VerifiedArtifact) (guardrail.AuthorizedMutation, error) {
		return mutationFor(v, nil, nil)
	}
	repairAuthorization := func(_ context.Context, p repair.Proposal, r revision.Revision) error {
		if p.DatabaseIdentity != c.DatabaseIdentity || p.Environment != c.Environment || p.GuardrailDigest != r.BundleDigest || c.RepairPolicyDigest == "" || p.PolicyDigest != c.RepairPolicyDigest {
			return errors.New("repair target or guardrail policy denied")
		}
		want := c.RepairApprovalDigest
		if p.Action == "remove" {
			want = c.RepairDestructiveApprovalDigest
		}
		if want == "" || p.ApprovalDigest != want {
			return errors.New("repair approval policy denied")
		}
		return nil
	}
	verified := VerifiedArtifactApplyService{PolicyFor: policyFor, InstallPolicy: func(digest string, p artifact.VerifyPolicy) {
		dynamicPolicyMu.Lock()
		dynamicPolicies[digest] = p
		dynamicPolicyMu.Unlock()
	}, Guardrail: g, Input: input, Mutation: mutation, MutationLocked: mutationFor, MutationLockedAttempt: mutationForAttempt, NoEdits: c.NoEdits, LifecycleAudit: lifecycle, RepairAuthorization: repairAuthorization}
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
	downService, err := newProductionDownService(c.DownConfigPath, resolver, verified, url, c)
	if err != nil {
		return Services{}, err
	}
	return Services{ReadPlan: DefaultReadPlan{}, Apply: resolvingApply{verified: verified, directory: c.ArtifactDirectory}, PlanEdit: editService, Down: downService}, nil
}

type resolvingApply struct {
	verified  VerifiedArtifactApplyService
	directory string
}

func (s resolvingApply) VerifyArtifact(a artifact.Artifact) (artifact.VerifiedArtifact, error) {
	return s.verified.VerifyArtifact(a)
}
func (s resolvingApply) VerifyArtifactForApply(a artifact.Artifact, mandatoryNoEdits bool) (artifact.VerifiedArtifact, error) {
	return s.verified.VerifyArtifactForApply(a, mandatoryNoEdits)
}
func (s resolvingApply) ApplyVersioned(ctx context.Context, v artifact.VerifiedArtifact, session executor.Session, tx executor.Tx) (executor.ExternalExecution, error) {
	return s.verified.ApplyVersioned(ctx, v, session, tx)
}
func (s resolvingApply) ApplyVersionedAttempt(ctx context.Context, v artifact.VerifiedArtifact, session executor.Session, tx executor.Tx, attempt int) (executor.ExternalExecution, error) {
	return s.verified.ApplyVersionedAttempt(ctx, v, session, tx, attempt)
}
func (s resolvingApply) DrainLifecycle(ctx context.Context, e executor.LifecycleEvent) error {
	return s.verified.DrainLifecycle(ctx, e)
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
