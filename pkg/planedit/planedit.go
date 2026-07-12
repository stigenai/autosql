// Package planedit provides provenance-preserving controlled SQL plan editing.
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
type EditRecord struct {
	Digest, ParentDigest, SQLDigest string
	Editor                          Editor
	Source                          string
}
type EditedArtifact struct {
	Version                   string
	OriginalGeneratedArtifact []byte
	OriginalPlan              plan.Plan
	CandidatePlan             plan.Plan
	EditedSQL                 string
	Provenance                []EditRecord
	Digest                    string
}

func New(originalBytes []byte, a artifact.Artifact, sql, sourceName string, editor Editor) (EditedArtifact, error) {
	if len(originalBytes) == 0 {
		return EditedArtifact{}, fmt.Errorf("%w: original bytes required", ErrInvalid)
	}
	canonical, err := a.MarshalCanonical()
	if err != nil || !json.Valid(originalBytes) || string(canonical) != string(originalBytes) {
		return EditedArtifact{}, fmt.Errorf("%w: original artifact bytes", ErrInvalid)
	}
	return edit(EditedArtifact{Version: Version, OriginalGeneratedArtifact: append([]byte(nil), originalBytes...), OriginalPlan: a.Plan, CandidatePlan: a.Plan}, sql, sourceName, editor)
}
func (e EditedArtifact) Edit(sql, sourceName string, editor Editor) (EditedArtifact, error) {
	return edit(e, sql, sourceName, editor)
}
func edit(e EditedArtifact, sql, sourceName string, editor Editor) (EditedArtifact, error) {
	if e.Version != Version || editor.Identity == "" || editor.Reason == "" || editor.At.IsZero() || editor.At.Location() != time.UTC || sourceName == "" {
		return EditedArtifact{}, fmt.Errorf("%w: editor provenance", ErrInvalid)
	}
	statements, err := source.SplitSQL(sourceName, sql)
	if err != nil {
		return EditedArtifact{}, err
	}
	parts := make([]string, len(statements))
	for i, s := range statements {
		parts[i] = s.SQL
	}
	candidate, err := plan.EditSQL(e.CandidatePlan, parts)
	if err != nil {
		return EditedArtifact{}, err
	}
	parent := e.Digest
	if parent == "" {
		parent = e.OriginalPlan.Digest
	}
	r := EditRecord{ParentDigest: parent, SQLDigest: hash("sql", []byte(sql)), Editor: editor, Source: sourceName}
	r.Digest = hash("record", mustJSON(r))
	e.CandidatePlan = candidate
	e.EditedSQL = sql
	e.Provenance = append(append([]EditRecord(nil), e.Provenance...), r)
	e.Digest = hash("artifact", mustJSON(struct {
		Original []byte
		Plan     plan.Plan
		Records  []EditRecord
	}{e.OriginalGeneratedArtifact, e.CandidatePlan, e.Provenance}))
	return e, nil
}
func hash(domain string, b []byte) string {
	s := sha256.Sum256(append([]byte("autosql.planedit."+domain+"/v1\x00"), b...))
	return "sha256:" + hex.EncodeToString(s[:])
}
func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }

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
}

func (p Pipeline) Revalidate(ctx context.Context, e EditedArtifact) (Eligible, error) {
	var out Eligible
	if p.Simulator == nil || p.Safety == nil || p.Binder == nil {
		return out, fmt.Errorf("%w: complete pipeline required", ErrInvalid)
	}
	stmts, err := source.SplitSQL("edited.sql", e.EditedSQL)
	if err != nil {
		return out, fmt.Errorf("parse edited SQL: %w", err)
	}
	parts := make([]string, len(stmts))
	for i, s := range stmts {
		parts[i] = s.SQL
	}
	rebuilt, err := plan.EditSQL(e.CandidatePlan, parts)
	if err != nil || rebuilt.Digest != e.CandidatePlan.Digest {
		return out, fmt.Errorf("rebuild bindings: %w (%v, %s != %s)", ErrInvalid, err, rebuilt.Digest, e.CandidatePlan.Digest)
	}
	fingerprint, err := p.Simulator.Simulate(ctx, rebuilt)
	if err != nil {
		return out, fmt.Errorf("isolated simulation: %w", err)
	}
	if !strings.EqualFold(fingerprint, rebuilt.ToFingerprint) {
		return out, fmt.Errorf("isolated simulation final fingerprint: %w", ErrInvalid)
	}
	if err = p.Safety.Analyze(ctx, rebuilt); err != nil {
		return out, fmt.Errorf("safety analysis: %w", err)
	}
	checks, bundle, err := p.Binder.Bind(ctx, rebuilt)
	if err != nil {
		return out, fmt.Errorf("policy precheck guardrail binding: %w", err)
	}
	return Eligible{Edit: e, Checks: checks, GuardrailDigest: bundle, FinalFingerprint: fingerprint}, nil
}
func (e Eligible) FreshArtifact(created, expires time.Time, revision, environment, database string, approval artifact.Approval) (artifact.Artifact, error) {
	if e.GuardrailDigest == "" || e.FinalFingerprint == "" {
		return artifact.Artifact{}, ErrInvalid
	}
	metadata := map[string]string{"autosql.edited": "true", "autosql.edit_digest": e.Edit.Digest, "autosql.original_plan_digest": e.Edit.OriginalPlan.Digest}
	return artifact.New(e.Edit.CandidatePlan, e.Checks, created, expires, revision, environment, database, e.GuardrailDigest, approval, metadata)
}
func IsEdited(a artifact.Artifact) bool { return a.Metadata["autosql.edited"] == "true" }
