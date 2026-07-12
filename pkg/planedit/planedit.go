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
	return err
}

type Simulator interface {
	Simulate(context.Context, plan.Plan) (string, error)
}
type Safety interface {
	Analyze(context.Context, plan.Plan) error
}
type Binder interface {
	Bind(context.Context, plan.Plan) (precheck.Plan, string, error)
}
type Pipeline struct {
	Simulator Simulator
	Safety    Safety
	Binder    Binder
}
type Eligible struct {
	Edit                              EditedArtifact
	Checks                            precheck.Plan
	GuardrailDigest, FinalFingerprint string
	Attestations                      []artifact.ValidationAttestation
}

func (p Pipeline) Revalidate(ctx context.Context, e EditedArtifact) (Eligible, error) {
	var out Eligible
	if err := e.Validate(); err != nil {
		return out, err
	}
	ss, err := parse("edited.sql", e.EditedSQL)
	if err != nil {
		return out, err
	}
	_ = ss
	rebuilt := e.CandidatePlan
	if err := rebuilt.Validate(); err != nil {
		return out, fmt.Errorf("rebuild bindings: %w", ErrInvalid)
	}
	fp, err := p.Simulator.Simulate(ctx, rebuilt)
	if err != nil {
		return out, fmt.Errorf("isolated simulation: %w", err)
	}
	if fp != rebuilt.ToFingerprint {
		return out, fmt.Errorf("final fingerprint: %w", ErrInvalid)
	}
	if err = p.Safety.Analyze(ctx, rebuilt); err != nil {
		return out, fmt.Errorf("safety: %w", err)
	}
	checks, bundle, err := p.Binder.Bind(ctx, rebuilt)
	if err != nil {
		return out, fmt.Errorf("policy precheck guardrail: %w", err)
	}
	now := time.Now().UTC()
	mk := func(stage, impl, result string) artifact.ValidationAttestation {
		return artifact.ValidationAttestation{Stage: stage, Implementation: impl, Version: "1", ConfigDigest: hash("config", []byte(impl)), ResultDigest: result, At: now}
	}
	return Eligible{Edit: e, Checks: checks, GuardrailDigest: bundle, FinalFingerprint: fp, Attestations: []artifact.ValidationAttestation{mk("parse_rebind", "pg_query_go/v6+autosql/plan", rebuilt.Digest), mk("simulation", fmt.Sprintf("%T", p.Simulator), fp), mk("safety", fmt.Sprintf("%T", p.Safety), hash("safety", []byte(rebuilt.Digest))), mk("policy_precheck_guardrail", fmt.Sprintf("%T", p.Binder), bundle)}}, nil
}
func (e Eligible) FreshArtifact(created, expires time.Time, revision, environment, database string, approval artifact.Approval) (artifact.Artifact, error) {
	if len(e.Attestations) != 4 || !approval.ApprovedAt.After(e.Edit.Provenance[len(e.Edit.Provenance)-1].EditedAt) {
		return artifact.Artifact{}, errors.New("fresh post-edit approval required")
	}
	a, err := artifact.New(e.Edit.CandidatePlan, e.Checks, created, expires, revision, environment, database, e.GuardrailDigest, approval, map[string]string{"autosql.edit_digest": e.Edit.Digest})
	if err != nil {
		return a, err
	}
	orig, _ := artifact.Parse(e.Edit.OriginalGeneratedArtifact)
	sig, _ := json.Marshal(orig.Signature)
	candidate, _ := e.Edit.CandidatePlan.MarshalCanonical()
	a.EditProvenance = &artifact.EditProvenance{Version: "autosql.edit-provenance/v1", OriginalArtifact: append([]byte(nil), e.Edit.OriginalGeneratedArtifact...), OriginalLength: len(e.Edit.OriginalGeneratedArtifact), OriginalBytesDigest: provHash("original-bytes", e.Edit.OriginalGeneratedArtifact), OriginalArtifactDigest: orig.Digest, OriginalPlanDigest: orig.Plan.Digest, OriginalSignatureDigest: provHash("signature", sig), CandidatePlanDigest: a.Plan.Digest, CandidateBytesDigest: provHash("candidate", candidate), ChainDigest: e.Edit.Provenance[len(e.Edit.Provenance)-1].Digest, Records: append([]artifact.EditRecord(nil), e.Edit.Provenance...), Attestations: append([]artifact.ValidationAttestation(nil), e.Attestations...)}
	if err = a.ResetAuthorization(); err != nil {
		return a, err
	}
	return a, nil
}
func IsEdited(a artifact.Artifact) bool { return a.EditProvenance != nil }
