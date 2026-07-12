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
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"time"
)

type productionEditService struct {
	editor                                                                                           string
	policyFor                                                                                        func(artifact.Artifact) (artifact.VerifyPolicy, error)
	g                                                                                                guardrail.Guardrail
	input                                                                                            func(artifact.Artifact) (guardrail.Input, error)
	url, targetIdentity, developmentURL, developmentIdentity, revision, environment, database, keyID string
	version                                                                                          int
	private                                                                                          ed25519.PrivateKey
	approval                                                                                         artifact.Approval
	created, expires                                                                                 time.Time
	audit                                                                                            *executor.FileAudit
	schemas                                                                                          []string
	stage                                                                                            func(string) error
}

func (s *productionEditService) checkpoint(name string) error {
	if s.stage != nil {
		return s.stage(name)
	}
	return nil
}

func (s *productionEditService) TrustedEditor() string { return s.editor }
func (s *productionEditService) VerifyOriginal(a artifact.Artifact) error {
	if err := s.checkpoint("original_verify"); err != nil {
		return err
	}
	p, err := s.policyFor(a)
	if err != nil {
		return err
	}
	_, err = a.VerifyTrusted(p)
	return err
}

type editSimulator struct {
	url, id, productionID string
	from                  schema.Document
}

func (s editSimulator) Simulate(ctx context.Context, p plan.Plan) (string, error) {
	r, e := simulate.Run(ctx, simulate.PostgresFactory{}, simulate.Request{Config: simulate.Config{DevelopmentURL: s.url, DevelopmentIdentity: s.id, ProductionIdentity: s.productionID, CleanupTimeout: 20 * time.Second}, From: s.from, Plan: p})
	return r.ToFingerprint, e
}

type editSafety struct{ version int }

func evidenceDigest(v any) string {
	b, _ := json.Marshal(v)
	x := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(x[:])
}
func (s editSafety) Analyze(ctx context.Context, p plan.Plan) (artifact.SafetyAttestation, error) {
	in := safety.Input{Changes: p.Changes, Statements: p.SafetyStatements(), Target: safety.Target{Engine: "postgresql", Version: s.version}}
	builtins := safety.Builtins()
	d, err := (safety.Runner{Analyzers: builtins}).Run(ctx, in)
	if err != nil {
		return artifact.SafetyAttestation{}, err
	}
	rules := make([]string, 0, len(builtins))
	for _, analyzer := range builtins {
		rules = append(rules, fmt.Sprintf("%T", analyzer))
	}
	seen := map[string]bool{}
	suppressed := []safety.Diagnostic{}
	for _, v := range d {
		if !seen[v.Rule] {
			seen[v.Rule] = true
			rules = append(rules, v.Rule)
		}
		if v.Suppressed != nil {
			suppressed = append(suppressed, v)
		}
		if v.Severity == safety.SeverityError {
			return artifact.SafetyAttestation{}, errors.New("edited plan has blocking safety diagnostics")
		}
	}
	return artifact.SafetyAttestation{Analyzers: rules, Threshold: string(safety.SeverityError), SuppressionsDigest: evidenceDigest(suppressed), DiagnosticsDigest: evidenceDigest(d), ConfigDigest: evidenceDigest(in)}, nil
}

type editBinder struct {
	s        *productionEditService
	original artifact.Artifact
}

func (b editBinder) BuildPrechecks(_ context.Context, p plan.Plan) (precheck.Plan, error) {
	cd, err := guardrail.ChangeDigest(p.Changes)
	if err != nil {
		return precheck.Plan{}, err
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
		return checks, err
	}
	for i := range checks.Assertions {
		checks.Assertions[i].PlanDigest = checks.Digest
	}
	return checks, nil
}
func (b editBinder) ValidatePolicy(_ context.Context, _ plan.Plan) (artifact.PolicyAttestation, error) {
	in, err := b.s.input(b.original)
	if err != nil {
		return artifact.PolicyAttestation{}, err
	}
	return artifact.PolicyAttestation{DocumentDigest: evidenceDigest(in.Policy), LimitsDigest: evidenceDigest(map[string]any{"identity": in.PolicyIdentity}), ResourcesDigest: evidenceDigest([]any{in.Changes, in.StatementBindings}), ConfigDigest: evidenceDigest(in.Policy)}, nil
}
func (b editBinder) BindGuardrail(_ context.Context, p plan.Plan, checks precheck.Plan) (string, error) {
	tmp, err := artifact.New(p, checks, b.s.created, b.s.expires, b.s.revision, b.s.environment, b.s.database, "sha256:"+strings.Repeat("0", 64), b.s.approval, map[string]string{})
	if err != nil {
		return "", err
	}
	in, err := b.s.input(tmp)
	if err != nil {
		return "", err
	}
	bundle, err := b.s.g.BundleDigest(in)
	return bundle, err
}
func (s *productionEditService) Revalidate(ctx context.Context, e planedit.EditedArtifact) (planedit.Eligible, error) {
	if s.audit == nil {
		return planedit.Eligible{}, errors.New("durable edit audit required")
	}
	reason := ""
	if len(e.Provenance) > 0 {
		sum := sha256.Sum256([]byte(e.Provenance[len(e.Provenance)-1].Reason))
		reason = "reason_sha256:" + hex.EncodeToString(sum[:])
	}
	event := func(kind, stage string) error {
		if err := s.checkpoint("audit_" + kind); err != nil {
			return err
		}
		return s.audit.AppendDurable(ctx, executor.LifecycleEvent{Type: kind, ExecutionID: e.Digest, Target: s.editor, PlanDigest: e.CandidatePlan.Digest, Guidance: stage + " " + reason, At: time.Now().UTC()})
	}
	if err := event("edit_requested", "requested"); err != nil {
		return planedit.Eligible{}, err
	}
	out, err := s.revalidateCore(ctx, e)
	if err != nil {
		if auditErr := event("edit_rejected", "validation"); auditErr != nil {
			return planedit.Eligible{}, fmt.Errorf("%v; rejection audit: %w", err, auditErr)
		}
		return planedit.Eligible{}, err
	}
	if err = event("edit_validated", "all_stages"); err != nil {
		return planedit.Eligible{}, err
	}
	return out, nil
}

func (s *productionEditService) revalidateCore(ctx context.Context, e planedit.EditedArtifact) (planedit.Eligible, error) {
	orig, err := artifact.Parse(e.OriginalGeneratedArtifact)
	if err != nil {
		return planedit.Eligible{}, err
	}
	if err = s.VerifyOriginal(orig); err != nil {
		return planedit.Eligible{}, err
	}
	if err = s.checkpoint("target_inspect_stale"); err != nil {
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
	if s.developmentIdentity == "" || s.targetIdentity == "" || s.developmentIdentity == s.targetIdentity {
		return planedit.Eligible{}, errors.New("isolated development database must be distinct from target")
	}
	if err = s.checkpoint("identity_isolation"); err != nil {
		return planedit.Eligible{}, err
	}
	contextRaw := strings.Join([]string{s.targetIdentity, s.developmentIdentity, s.revision, s.environment, s.database, s.editor, fmt.Sprint(s.version), orig.Checks.Digest, orig.GuardrailDigest}, "\x00")
	contextSum := sha256.Sum256([]byte(contextRaw))
	contextDigest := "sha256:" + hex.EncodeToString(contextSum[:])
	reasonDigest, chainDigest, editor := "", "", ""
	if len(e.Provenance) > 0 {
		x := sha256.Sum256([]byte(e.Provenance[len(e.Provenance)-1].Reason))
		reasonDigest = "sha256:" + hex.EncodeToString(x[:])
		chainDigest = e.Provenance[len(e.Provenance)-1].Digest
		editor = e.Provenance[len(e.Provenance)-1].EditorIdentity
	}
	binder := editBinder{s: s, original: orig}
	eligible, err := (planedit.Pipeline{Simulator: editSimulator{url: s.developmentURL, id: s.developmentIdentity, productionID: s.targetIdentity, from: from}, Safety: editSafety{version: s.version}, Policy: binder, Prechecks: binder, Guardrails: binder, ContextDigest: contextDigest, Stage: s.stage, Context: planedit.ValidationContext{TargetIdentity: s.targetIdentity, DevelopmentIdentity: s.developmentIdentity, DatabaseVersion: fmt.Sprint(s.version), EditorIdentity: editor, ReasonDigest: reasonDigest, ChainDigest: chainDigest}}).Revalidate(ctx, e)
	return eligible, err
}
func (s *productionEditService) Publish(ctx context.Context, e planedit.Eligible) (out artifact.Artifact, err error) {
	if s.audit == nil {
		return out, errors.New("durable edit audit required")
	}
	if err = s.checkpoint("audit_edit_publish_requested"); err != nil {
		return out, err
	}
	if err = s.audit.AppendDurable(ctx, executor.LifecycleEvent{Type: "edit_publish_requested", ExecutionID: e.Edit.Digest, Target: s.editor, PlanDigest: e.Edit.CandidatePlan.Digest, At: time.Now().UTC()}); err != nil {
		return out, err
	}
	published := false
	defer func() {
		if !published && err != nil {
			if checkpointErr := s.checkpoint("audit_edit_publish_rejected"); checkpointErr != nil {
				err = fmt.Errorf("%v; rejection audit: %w", err, checkpointErr)
			} else if ae := s.audit.AppendDurable(ctx, executor.LifecycleEvent{Type: "edit_rejected", ExecutionID: e.Edit.Digest, Target: s.editor, PlanDigest: e.Edit.CandidatePlan.Digest, Guidance: "publish", At: time.Now().UTC()}); ae != nil {
				err = fmt.Errorf("%v; rejection audit: %w", err, ae)
			}
		}
	}()
	// Review attestations are not authorization. Re-run the complete trusted
	// pipeline from the embedded draft immediately before signing.
	if err = s.checkpoint("immediate_rerun"); err != nil {
		return out, err
	}
	fresh, err := s.Revalidate(ctx, e.Edit)
	if err != nil {
		return artifact.Artifact{}, err
	}
	if !reflect.DeepEqual(e.Edit, fresh.Edit) || !reflect.DeepEqual(e.Checks, fresh.Checks) || e.GuardrailDigest != fresh.GuardrailDigest || e.FinalFingerprint != fresh.FinalFingerprint || len(e.Attestations) != len(fresh.Attestations) {
		return artifact.Artifact{}, errors.New("supplied eligibility does not match trusted revalidation")
	}
	for i := range fresh.Attestations {
		got, want := e.Attestations[i], fresh.Attestations[i]
		if got.Stage != want.Stage || got.Implementation != want.Implementation || got.Version != want.Version || got.ConfigDigest != want.ConfigDigest || got.ResultDigest != want.ResultDigest || !reflect.DeepEqual(got.Simulation, want.Simulation) || !reflect.DeepEqual(got.Safety, want.Safety) || !reflect.DeepEqual(got.Policy, want.Policy) || !reflect.DeepEqual(got.Precheck, want.Precheck) || !reflect.DeepEqual(got.Editor, want.Editor) || got.At.Before(e.Edit.Provenance[len(e.Edit.Provenance)-1].EditedAt) || !got.ExpiresAt.After(time.Now().UTC()) {
			return artifact.Artifact{}, errors.New("supplied eligibility attestation mismatch or expiry")
		}
	}
	if err = s.checkpoint("approval_freshness_proof"); err != nil {
		return out, err
	}
	out, err = fresh.FreshArtifact(s.created, s.expires, s.revision, s.environment, s.database, s.approval)
	if err != nil {
		return out, err
	}
	if err = s.checkpoint("sign"); err != nil {
		return artifact.Artifact{}, err
	}
	if err = out.Sign(s.keyID, s.private); err != nil {
		return out, err
	}
	published = true
	return out, err
}

// AtomicCreate is used by the shipped edit CLI after all trusted work has
// completed. Keeping the output boundary on the production service makes it
// injectable in ordered fail-closed tests without replacing the real writer.
func (s *productionEditService) AtomicCreate(path string, data []byte) error {
	if err := s.checkpoint("atomic_create"); err != nil {
		return err
	}
	return atomicCreate(path, data)
}

// PublishOutput makes the signed file durable before recording publication.
// If the final audit cannot be made durable, the file is removed so callers
// never observe an unaudited release.
func (s *productionEditService) PublishOutput(ctx context.Context, eligible planedit.Eligible, a artifact.Artifact, path string, data []byte) (err error) {
	if err = s.AtomicCreate(path, data); err != nil {
		return err
	}
	committed := false
	defer func() {
		if !committed {
			_ = os.Remove(path)
			if s.audit != nil {
				if checkpointErr := s.checkpoint("audit_edit_publish_rejected"); checkpointErr != nil {
					err = fmt.Errorf("%v; rejection audit: %w", err, checkpointErr)
				} else if auditErr := s.audit.AppendDurable(ctx, executor.LifecycleEvent{Type: "edit_rejected", ExecutionID: eligible.Edit.Digest, Target: s.editor, PlanDigest: eligible.Edit.CandidatePlan.Digest, Guidance: "output", At: time.Now().UTC()}); auditErr != nil {
					err = fmt.Errorf("%v; rejection audit: %w", err, auditErr)
				}
			}
		}
	}()
	if s.audit == nil {
		return errors.New("durable edit audit required")
	}
	if err = s.checkpoint("audit_edit_published"); err != nil {
		return err
	}
	if err = s.audit.AppendDurable(ctx, executor.LifecycleEvent{Type: "edit_published", ExecutionID: eligible.Edit.Digest, ArtifactDigest: a.Digest, PlanDigest: a.Plan.Digest, BundleDigest: a.GuardrailDigest, At: time.Now().UTC()}); err != nil {
		return err
	}
	committed = true
	return nil
}
func decodePrivate(value string) (ed25519.PrivateKey, error) {
	raw, err := base64.RawStdEncoding.DecodeString(value)
	if err != nil || len(raw) != ed25519.PrivateKeySize {
		return nil, fmt.Errorf("invalid edit signing key")
	}
	return ed25519.PrivateKey(raw), nil
}
