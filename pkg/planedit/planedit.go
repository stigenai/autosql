package planedit

import (
	"autosql/pkg/artifact"
	"autosql/pkg/plan"
	"autosql/pkg/precheck"
	"autosql/pkg/source"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/encoding/protojson"
	"strings"
	"time"
)

const Version = "autosql.planedit/v1"

var ErrInvalid = errors.New("invalid controlled plan edit")

type Editor struct {
	Identity string    `json:"identity"`
	At       time.Time `json:"at"`
	Reason   string    `json:"reason"`
}
type EditedArtifact struct {
	Version                   string                `json:"version"`
	OriginalGeneratedArtifact []byte                `json:"original_generated_artifact"`
	OriginalPlan              plan.Plan             `json:"original_plan"`
	CandidatePlan             plan.Plan             `json:"candidate_plan"`
	EditedSQL                 string                `json:"edited_sql"`
	Provenance                []artifact.EditRecord `json:"provenance"`
	Digest                    string                `json:"digest"`
}

func hash(domain string, b []byte) string {
	s := sha256.Sum256(append([]byte("autosql.planedit."+domain+"/v1\x00"), b...))
	return "sha256:" + hex.EncodeToString(s[:])
}
func provHash(domain string, b []byte) string {
	s := sha256.Sum256(append([]byte("autosql.edit-provenance."+domain+"/v1\x00"), b...))
	return "sha256:" + hex.EncodeToString(s[:])
}
func js(v any) []byte { b, _ := json.Marshal(v); return b }
func parse(file, sql string) ([]source.Statement, error) {
	tree, err := pg_query.Parse(sql)
	if err != nil {
		return nil, fmt.Errorf("%w: PostgreSQL syntax", ErrInvalid)
	}
	ss, err := source.SplitSQL(file, sql)
	if err != nil || len(ss) == 0 || len(tree.Stmts) != len(ss) {
		return nil, fmt.Errorf("%w: exact statement coverage", ErrInvalid)
	}
	for i, raw := range tree.Stmts {
		n := raw.Stmt
		if n.GetTransactionStmt() != nil || n.GetVariableSetStmt() != nil || n.GetCopyStmt() != nil || n.GetCreateRoleStmt() != nil || n.GetAlterRoleStmt() != nil || n.GetAlterRoleSetStmt() != nil || n.GetDropRoleStmt() != nil || n.GetGrantRoleStmt() != nil || n.GetLockStmt() != nil || n.GetListenStmt() != nil || n.GetNotifyStmt() != nil {
			return nil, fmt.Errorf("%w: session, transaction, role, lock, or COPY statement", ErrInvalid)
		}
		if n.GetCreateStmt() == nil && n.GetAlterTableStmt() == nil && n.GetIndexStmt() == nil && n.GetDropStmt() == nil && n.GetRenameStmt() == nil && n.GetViewStmt() == nil && n.GetCreateSchemaStmt() == nil {
			return nil, fmt.Errorf("%w: only supported declarative DDL is editable", ErrInvalid)
		}
		pb, _ := protojson.Marshal(n)
		lower := strings.ToLower(string(pb) + " " + ss[i].SQL)
		for _, bad := range []string{"pg_advisory", "autosql_migration_history", "search_path", "session_authorization", "autosql_audit"} {
			if strings.Contains(lower, bad) {
				return nil, fmt.Errorf("%w: protected execution control", ErrInvalid)
			}
		}
	}
	return ss, nil
}

func bindDDL(e EditedArtifact) error {
	original := make([]plan.Step, 0)
	candidate := make([]plan.Step, 0)
	for _, s := range e.OriginalPlan.Steps {
		if s.Kind == plan.StepExecutable {
			original = append(original, s)
		}
	}
	for _, s := range e.CandidatePlan.Steps {
		if s.Kind == plan.StepExecutable {
			candidate = append(candidate, s)
		}
	}
	if len(original) != len(candidate) {
		return fmt.Errorf("%w: statement/change cardinality", ErrInvalid)
	}
	for i := range original {
		if original[i].ChangeID != candidate[i].ChangeID {
			return fmt.Errorf("%w: change binding mismatch", ErrInvalid)
		}
		want, err := pg_query.Fingerprint(original[i].SQL)
		if err != nil {
			return ErrInvalid
		}
		got, err := pg_query.Fingerprint(candidate[i].SQL)
		if err != nil || got != want {
			return fmt.Errorf("%w: edited AST differs from generated statement", ErrInvalid)
		}
	}
	return nil
}
func New(raw []byte, a artifact.Artifact, sql, file string, editor Editor) (EditedArtifact, error) {
	if len(raw) == 0 {
		return EditedArtifact{}, ErrInvalid
	}
	canonical, err := a.MarshalCanonical()
	if err != nil || string(canonical) != string(raw) {
		return EditedArtifact{}, fmt.Errorf("%w: byte-exact original", ErrInvalid)
	}
	return edit(EditedArtifact{Version: Version, OriginalGeneratedArtifact: append([]byte(nil), raw...), OriginalPlan: a.Plan, CandidatePlan: a.Plan}, sql, file, editor)
}
func (e EditedArtifact) Edit(sql, file string, editor Editor) (EditedArtifact, error) {
	return edit(e, sql, file, editor)
}
func edit(e EditedArtifact, sql, file string, editor Editor) (EditedArtifact, error) {
	if editor.Identity == "" || len(editor.Reason) > 4096 || strings.TrimSpace(editor.Reason) == "" || strings.IndexFunc(editor.Reason, func(r rune) bool { return r < 32 && r != '\n' && r != '\t' }) >= 0 || editor.At.IsZero() || editor.At.Location() != time.UTC || editor.At.After(time.Now().UTC().Add(time.Minute)) || file == "" {
		return EditedArtifact{}, fmt.Errorf("%w: editor provenance", ErrInvalid)
	}
	if len(e.Provenance) > 0 && !editor.At.After(e.Provenance[len(e.Provenance)-1].EditedAt) {
		return EditedArtifact{}, fmt.Errorf("%w: edit time is not monotonic", ErrInvalid)
	}
	ss, err := parse(file, sql)
	if err != nil {
		return EditedArtifact{}, err
	}
	parts := make([]string, len(ss))
	for i := range ss {
		parts[i] = ss[i].SQL
	}
	candidate, err := plan.EditSQL(e.CandidatePlan, parts)
	if err != nil {
		return EditedArtifact{}, err
	}
	parent := e.OriginalPlan.Digest
	if e.Digest != "" {
		parent = e.Digest
	}
	r := artifact.EditRecord{ParentDigest: parent, SQLDigest: hash("sql", []byte(sql)), EditorIdentity: editor.Identity, EditedAt: editor.At, Reason: editor.Reason, Source: file}
	r.Digest = provHash("record", js(r))
	e.CandidatePlan = candidate
	e.EditedSQL = sql
	if err := bindDDL(e); err != nil {
		return EditedArtifact{}, err
	}
	e.Provenance = append(append([]artifact.EditRecord(nil), e.Provenance...), r)
	e.Digest = hash("draft", js(struct {
		Original []byte
		Plan     plan.Plan
		Records  []artifact.EditRecord
	}{e.OriginalGeneratedArtifact, e.CandidatePlan, e.Provenance}))
	return e, nil
}
func (e EditedArtifact) Validate() error {
	if e.Version != Version || e.Digest == "" || len(e.OriginalGeneratedArtifact) == 0 || len(e.Provenance) == 0 {
		return ErrInvalid
	}
	a, err := artifact.Parse(e.OriginalGeneratedArtifact)
	if err != nil || a.Plan.Digest != e.OriginalPlan.Digest {
		return ErrInvalid
	}
	parent := e.OriginalPlan.Digest
	for _, r := range e.Provenance {
		copy := r
		copy.Digest = ""
		if r.ParentDigest != parent || r.Digest != provHash("record", js(copy)) {
			return ErrInvalid
		}
		parent = r.Digest
	}
	want := hash("draft", js(struct {
		Original []byte
		Plan     plan.Plan
		Records  []artifact.EditRecord
	}{e.OriginalGeneratedArtifact, e.CandidatePlan, e.Provenance}))
	if want != e.Digest || parent != e.Provenance[len(e.Provenance)-1].Digest {
		return ErrInvalid
	}
	if e.Provenance[len(e.Provenance)-1].SQLDigest != hash("sql", []byte(e.EditedSQL)) {
		return ErrInvalid
	}
	_, err = parse("edited.sql", e.EditedSQL)
	if err != nil {
		return err
	}
	return bindDDL(e)
}

type Simulator interface {
	Simulate(context.Context, plan.Plan) (string, error)
}
type Safety interface {
	Analyze(context.Context, plan.Plan) (artifact.SafetyAttestation, error)
}
type Binder interface {
	Bind(context.Context, plan.Plan) (precheck.Plan, string, error)
}
type PolicyValidator interface {
	ValidatePolicy(context.Context, plan.Plan) (artifact.PolicyAttestation, error)
}
type PrecheckBuilder interface {
	BuildPrechecks(context.Context, plan.Plan) (precheck.Plan, error)
}
type GuardrailBinder interface {
	BindGuardrail(context.Context, plan.Plan, precheck.Plan) (string, error)
}
type Pipeline struct {
	Simulator     Simulator
	Safety        Safety
	Binder        Binder
	Policy        PolicyValidator
	Prechecks     PrecheckBuilder
	Guardrails    GuardrailBinder
	ContextDigest string
	Context       ValidationContext
	Stage         func(string) error
}
type ValidationContext struct{ TargetIdentity, DevelopmentIdentity, DatabaseVersion, EditorIdentity, ReasonDigest, ChainDigest string }
type Eligible struct {
	Edit                              EditedArtifact
	Checks                            precheck.Plan
	GuardrailDigest, FinalFingerprint string
	Attestations                      []artifact.ValidationAttestation
}

func (p Pipeline) Revalidate(ctx context.Context, e EditedArtifact) (Eligible, error) {
	var out Eligible
	stage := func(name string) error {
		if p.Stage != nil {
			return p.Stage(name)
		}
		return nil
	}
	if err := stage("parse"); err != nil {
		return out, err
	}
	if err := e.Validate(); err != nil {
		return out, err
	}
	ss, err := parse("edited.sql", e.EditedSQL)
	if err != nil {
		return out, err
	}
	_ = ss
	if err = stage("ast_bind"); err != nil {
		return out, err
	}
	if err = bindDDL(e); err != nil {
		return out, err
	}
	rebuilt := e.CandidatePlan
	if err = stage("rebind"); err != nil {
		return out, err
	}
	if err := rebuilt.Validate(); err != nil {
		return out, fmt.Errorf("rebuild bindings: %w", ErrInvalid)
	}
	if err = stage("simulation"); err != nil {
		return out, err
	}
	fp, err := p.Simulator.Simulate(ctx, rebuilt)
	if err != nil {
		return out, fmt.Errorf("isolated simulation: %w", err)
	}
	if err = stage("fingerprint"); err != nil {
		return out, err
	}
	if fp != rebuilt.ToFingerprint {
		return out, fmt.Errorf("final fingerprint: %w", ErrInvalid)
	}
	if err = stage("safety"); err != nil {
		return out, err
	}
	safetyEvidence, err := p.Safety.Analyze(ctx, rebuilt)
	if err != nil {
		return out, fmt.Errorf("safety: %w", err)
	}
	if err = stage("policy"); err != nil {
		return out, err
	}
	var policyEvidence artifact.PolicyAttestation
	if p.Policy != nil {
		if policyEvidence, err = p.Policy.ValidatePolicy(ctx, rebuilt); err != nil {
			return out, fmt.Errorf("policy: %w", err)
		}
	}
	if err = stage("precheck"); err != nil {
		return out, err
	}
	var checks precheck.Plan
	if p.Prechecks != nil {
		checks, err = p.Prechecks.BuildPrechecks(ctx, rebuilt)
		if err != nil {
			return out, fmt.Errorf("precheck: %w", err)
		}
	}
	if err = stage("guardrail"); err != nil {
		return out, err
	}
	var bundle string
	if p.Guardrails != nil {
		bundle, err = p.Guardrails.BindGuardrail(ctx, rebuilt, checks)
	} else {
		checks, bundle, err = p.Binder.Bind(ctx, rebuilt)
	}
	if err != nil {
		return out, fmt.Errorf("policy precheck guardrail: %w", err)
	}
	if policyEvidence.ConfigDigest == "" {
		policyEvidence = artifact.PolicyAttestation{DocumentDigest: hash("legacy-policy", js(rebuilt.Changes)), LimitsDigest: hash("legacy-limits", nil), ResourcesDigest: hash("legacy-resources", js(checks)), ConfigDigest: hash("legacy-policy-config", js(rebuilt.Changes))}
	}
	now := time.Now().UTC()
	vc := p.Context
	if vc.EditorIdentity == "" {
		vc.EditorIdentity = e.Provenance[len(e.Provenance)-1].EditorIdentity
		vc.ChainDigest = e.Provenance[len(e.Provenance)-1].Digest
		vc.ReasonDigest = hash("reason", []byte(e.Provenance[len(e.Provenance)-1].Reason))
	}
	if vc.TargetIdentity == "" {
		vc.TargetIdentity = "target/test"
	}
	if vc.DevelopmentIdentity == "" {
		vc.DevelopmentIdentity = "development/test"
	}
	if vc.DatabaseVersion == "" {
		vc.DatabaseVersion = "test"
	}
	mk := func(stage, impl, result string) artifact.ValidationAttestation {
		a := artifact.ValidationAttestation{Stage: stage, Implementation: impl, Version: "1", ConfigDigest: hash("config", []byte(impl+"\x00"+p.ContextDigest)), ResultDigest: result, At: now, ExpiresAt: now.Add(time.Hour)}
		switch stage {
		case "parse_rebind":
			a.Editor = &artifact.EditorAttestation{Identity: vc.EditorIdentity, ReasonDigest: vc.ReasonDigest, ChainDigest: vc.ChainDigest, ConfigDigest: a.ConfigDigest}
		case "simulation":
			a.Simulation = &artifact.SimulationAttestation{TargetIdentity: vc.TargetIdentity, DevelopmentIdentity: vc.DevelopmentIdentity, FromFingerprint: rebuilt.FromFingerprint, ToFingerprint: rebuilt.ToFingerprint, DatabaseVersion: vc.DatabaseVersion, ConfigDigest: a.ConfigDigest}
		case "safety":
			a.ConfigDigest = safetyEvidence.ConfigDigest
			a.Safety = &safetyEvidence
		case "policy_precheck_guardrail":
			a.ConfigDigest = policyEvidence.ConfigDigest
			a.Policy = &policyEvidence
			a.Precheck = &artifact.PrecheckGuardrailAttestation{ChecksDigest: checks.Digest, GuardrailDigest: bundle, ConfigDigest: a.ConfigDigest}
		}
		return a
	}
	return Eligible{Edit: e, Checks: checks, GuardrailDigest: bundle, FinalFingerprint: fp, Attestations: []artifact.ValidationAttestation{mk("parse_rebind", "pg_query_go/v6+autosql/plan", rebuilt.Digest), mk("simulation", fmt.Sprintf("%T", p.Simulator), fp), mk("safety", fmt.Sprintf("%T", p.Safety), hash("safety", []byte(rebuilt.Digest))), mk("policy_precheck_guardrail", fmt.Sprintf("%T", p.Binder), bundle)}}, nil
}
func (e Eligible) FreshArtifact(created, expires time.Time, revision, environment, database string, approval artifact.Approval) (artifact.Artifact, error) {
	original, parseErr := artifact.Parse(e.Edit.OriginalGeneratedArtifact)
	if parseErr != nil || len(e.Attestations) != 4 || !approval.ApprovedAt.After(e.Edit.Provenance[len(e.Edit.Provenance)-1].EditedAt) || approval.Identity == original.Approval.Identity || !strings.HasPrefix(approval.ProofDigest, "sha256:") || approval.ProofDigest == original.Approval.ProofDigest {
		return artifact.Artifact{}, errors.New("fresh post-edit approval required")
	}
	a, err := artifact.New(e.Edit.CandidatePlan, e.Checks, created, expires, revision, environment, database, e.GuardrailDigest, approval, map[string]string{"autosql.edit_digest": e.Edit.Digest})
	if err != nil {
		return a, err
	}
	orig := original
	sig, _ := json.Marshal(orig.Signature)
	candidate, _ := e.Edit.CandidatePlan.MarshalCanonical()
	a.EditProvenance = &artifact.EditProvenance{Version: "autosql.edit-provenance/v1", OriginalArtifact: append([]byte(nil), e.Edit.OriginalGeneratedArtifact...), OriginalLength: len(e.Edit.OriginalGeneratedArtifact), OriginalBytesDigest: provHash("original-bytes", e.Edit.OriginalGeneratedArtifact), OriginalArtifactDigest: orig.Digest, OriginalPlanDigest: orig.Plan.Digest, OriginalSignatureDigest: provHash("signature", sig), CandidatePlanDigest: a.Plan.Digest, CandidateBytesDigest: provHash("candidate", candidate), ChainDigest: e.Edit.Provenance[len(e.Edit.Provenance)-1].Digest, Records: append([]artifact.EditRecord(nil), e.Edit.Provenance...), Attestations: append([]artifact.ValidationAttestation(nil), e.Attestations...)}
	a.MarkEditedOrigin("autosql/controlled-plan-editor/v1")
	if err = a.ResetAuthorization(); err != nil {
		return a, err
	}
	return a, nil
}
func IsEdited(a artifact.Artifact) bool { return a.EditProvenance != nil }
