package cli

import (
	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/guardrail"
	"autosql/pkg/plan"
	"autosql/pkg/planedit"
	"autosql/pkg/postgres"
	"autosql/pkg/precheck"
	"autosql/pkg/safety"
	"autosql/pkg/schema"
	"autosql/pkg/simulate"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"strings"
	"time"
)

type productionEditService struct {
	editor                                                      string
	policyFor                                                   func(artifact.Artifact) (artifact.VerifyPolicy, error)
	g                                                           guardrail.Guardrail
	input                                                       func(artifact.Artifact) (guardrail.Input, error)
	url, developmentURL, revision, environment, database, keyID string
	version                                                     int
	private                                                     ed25519.PrivateKey
	approval                                                    artifact.Approval
	created, expires                                            time.Time
	audit                                                       *executor.FileAudit
	schemas                                                     []string
}

func (s *productionEditService) TrustedEditor() string { return s.editor }
func (s *productionEditService) VerifyOriginal(a artifact.Artifact) error {
	p, err := s.policyFor(a)
	if err != nil {
		return err
	}
	_, err = a.VerifyTrusted(p)
	return err
}

type editSimulator struct {
	url, id string
	from    schema.Document
}

func (s editSimulator) Simulate(ctx context.Context, p plan.Plan) (string, error) {
	r, e := simulate.Run(ctx, simulate.PostgresFactory{}, simulate.Request{Config: simulate.Config{DevelopmentURL: s.url, DevelopmentIdentity: s.id, ProductionIdentity: "production.invalid:5432/prod", CleanupTimeout: 20 * time.Second}, From: s.from, Plan: p})
	return r.ToFingerprint, e
}

type editSafety struct{ version int }

func (s editSafety) Analyze(ctx context.Context, p plan.Plan) error {
	d, err := (safety.Runner{Analyzers: safety.Builtins()}).Run(ctx, safety.Input{Changes: p.Changes, Statements: p.SafetyStatements(), Target: safety.Target{Engine: "postgresql", Version: s.version}})
	if err != nil {
		return err
	}
	for _, v := range d {
		if v.Severity == safety.SeverityError {
			return errors.New("edited plan has blocking safety diagnostics")
		}
	}
	return nil
}

type editBinder struct {
	s        *productionEditService
	original artifact.Artifact
}

func (b editBinder) Bind(ctx context.Context, p plan.Plan) (precheck.Plan, string, error) {
	cd, err := guardrail.ChangeDigest(p.Changes)
	if err != nil {
		return precheck.Plan{}, "", err
	}
	checks := b.original.Checks
	checks.ChangeDigest = cd
	checks.Statements = nil
	for _, st := range p.Steps {
		if st.Kind == plan.StepExecutable {
			checks.Statements = append(checks.Statements, st.SQL)
		}
	}
	for i := range checks.Assertions {
		checks.Assertions[i].ChangeDigest = cd
		checks.Assertions[i].PlanDigest = ""
	}
	checks.Digest, err = precheck.Digest(checks)
	if err != nil {
		return checks, "", err
	}
	for i := range checks.Assertions {
		checks.Assertions[i].PlanDigest = checks.Digest
	}
	tmp, err := artifact.New(p, checks, b.s.created, b.s.expires, b.s.revision, b.s.environment, b.s.database, "sha256:"+strings.Repeat("0", 64), b.s.approval, map[string]string{})
	if err != nil {
		return checks, "", err
	}
	in, err := b.s.input(tmp)
	if err != nil {
		return checks, "", err
	}
	bundle, err := b.s.g.BundleDigest(in)
	return checks, bundle, err
}
func (s *productionEditService) Revalidate(ctx context.Context, e planedit.EditedArtifact) (planedit.Eligible, error) {
	orig, err := artifact.Parse(e.OriginalGeneratedArtifact)
	if err != nil {
		return planedit.Eligible{}, err
	}
	if err = s.VerifyOriginal(orig); err != nil {
		return planedit.Eligible{}, err
	}
	from, err := postgres.InspectURL(ctx, s.url, postgres.Options{Schemas: s.schemas})
	if err != nil {
		return planedit.Eligible{}, err
	}
	fp, err := schema.SemanticFingerprint(from)
	if err != nil || fp != e.CandidatePlan.FromFingerprint {
		return planedit.Eligible{}, errors.New("edited plan source is stale")
	}
	id, err := simulate.ResolvePostgresIdentity(ctx, s.developmentURL)
	if err != nil {
		return planedit.Eligible{}, err
	}
	eligible, err := (planedit.Pipeline{Simulator: editSimulator{url: s.developmentURL, id: id, from: from}, Safety: editSafety{version: s.version}, Binder: editBinder{s: s, original: orig}}).Revalidate(ctx, e)
	if err == nil && s.audit != nil {
		err = s.audit.AppendDurable(ctx, executor.LifecycleEvent{Type: "edit_revalidated", ExecutionID: e.Digest, ArtifactDigest: orig.Digest, PlanDigest: e.CandidatePlan.Digest, BundleDigest: eligible.GuardrailDigest, At: time.Now().UTC()})
	}
	return eligible, err
}
func (s *productionEditService) Publish(ctx context.Context, e planedit.Eligible) (artifact.Artifact, error) {
	if e.Edit.Validate() != nil || len(e.Attestations) != 4 {
		return artifact.Artifact{}, errors.New("invalid or stale edit attestation")
	}
	for _, a := range e.Attestations {
		if a.At.Before(e.Edit.Provenance[len(e.Edit.Provenance)-1].EditedAt) || time.Since(a.At) > time.Hour {
			return artifact.Artifact{}, errors.New("stale edit attestation")
		}
	}
	out, err := e.FreshArtifact(s.created, s.expires, s.revision, s.environment, s.database, s.approval)
	if err != nil {
		return out, err
	}
	if err = out.Sign(s.keyID, s.private); err != nil {
		return out, err
	}
	if s.audit != nil {
		err = s.audit.AppendDurable(ctx, executor.LifecycleEvent{Type: "edit_published", ExecutionID: e.Edit.Digest, ArtifactDigest: out.Digest, PlanDigest: out.Plan.Digest, BundleDigest: out.GuardrailDigest, At: time.Now().UTC()})
	}
	return out, err
}
func decodePrivate(value string) (ed25519.PrivateKey, error) {
	raw, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid edit signing key")
	}
	return ed25519.PrivateKey(raw), nil
}
