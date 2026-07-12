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
	"github.com/jackc/pgx/v5"
)

type applyConfig struct {
	DatabaseURL, Environment, DatabaseIdentity, SourceRevision, KeyID, PublicKey, Issuer, Signer, Author, Requester, ApprovalAuditPath, LifecycleAuditPath, ArtifactDirectory string
	PostgresVersion                                                                                                                                                           int
	Schemas                                                                                                                                                                   []string
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
		now := time.Now().UTC()
		return artifact.VerifyPolicy{Now: time.Now, Expected: artifact.ExpectedBindings{PlanDigest: a.Plan.Digest, ChecksDigest: a.Checks.Digest, GuardrailDigest: a.GuardrailDigest, SourceRevision: c.SourceRevision, Environment: c.Environment, DatabaseIdentity: c.DatabaseIdentity, ApprovalIdentity: a.Approval.Identity}, Keys: map[string]artifact.KeyRecord{c.KeyID: {PublicKey: ed25519.PublicKey(pub), Issuer: c.Issuer, Identity: c.Signer, Environment: c.Environment, Purpose: "plan-artifact", Status: "active", NotBefore: now.Add(-365 * 24 * time.Hour), NotAfter: now.Add(365 * 24 * time.Hour)}}, Issuer: c.Issuer, Identity: c.Signer, Purpose: "plan-artifact"}, nil
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
	mutation := func(v artifact.VerifiedArtifact) (guardrail.AuthorizedMutation, error) {
		a, _ := v.Payload()
		state := func(ctx context.Context) (executor.RuntimeState, error) {
			doc, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: c.Schemas})
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
		return executor.NewPostgreSQL(executor.Config{URL: url, Audit: &executor.FileAudit{Path: c.LifecycleAuditPath}, Reauthorize: func(ctx context.Context, a artifact.Artifact) error {
			fresh, _ := policyFor(a)
			_, err := a.VerifyTrusted(fresh)
			return err
		}, State: func(ctx context.Context, _ *pgx.Conn) (executor.RuntimeState, error) { return state(ctx) }, Confirm: func(ctx context.Context, _ *pgx.Conn, s plan.Step) error {
			if s.ID != last {
				return nil
			}
			got, err := state(ctx)
			if err != nil {
				return err
			}
			if !strings.EqualFold(got.Fingerprint, a.Plan.ToFingerprint) {
				return errors.New("target postcondition mismatch")
			}
			return nil
		}}, v)
	}
	verified := VerifiedArtifactApplyService{PolicyFor: policyFor, Guardrail: g, Input: input, Mutation: mutation}
	return Services{ReadPlan: DefaultReadPlan{}, Apply: resolvingApply{verified: verified, directory: c.ArtifactDirectory}}, nil
}

type resolvingApply struct {
	verified  VerifiedArtifactApplyService
	directory string
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
