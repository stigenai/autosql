// Package ci contains deterministic contracts for CI changeset review.
package ci

import (
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

const ContractVersion = "autosql.ci/v1"

type ReferenceKind string

const (
	Branch             ReferenceKind = "branch"
	Tag                ReferenceKind = "tag"
	Registry           ReferenceKind = "registry"
	MigrationDirectory ReferenceKind = "migration_directory"
)

type Reference struct {
	Kind     ReferenceKind `json:"kind"`
	Value    string        `json:"value"`
	Revision string        `json:"revision,omitempty"`
	Digest   string        `json:"digest,omitempty"`
}
type Revision struct {
	ID     string `json:"id"`
	Parent string `json:"parent,omitempty"`
	File   string `json:"file"`
	Digest string `json:"digest"`
}
type Changeset struct {
	Version   string     `json:"version"`
	Base      Reference  `json:"base"`
	Head      Reference  `json:"head"`
	Revisions []Revision `json:"revisions"`
	Digest    string     `json:"digest"`
}

var ErrStaleHistory = errors.New("stale or non-linear migration history")

// Detect selects exactly the revisions after base and before head. It rejects
// missing parents, duplicate IDs, divergent ancestry, and revisions outside a
// migration directory, preventing a CI job from silently analyzing the wrong
// changeset.
func Detect(base, head Reference, history []Revision, migrationDir string) (Changeset, error) {
	if !validReference(base) || !validReference(head) || len(history) == 0 {
		return Changeset{}, fmt.Errorf("invalid changeset references")
	}
	byID := map[string]Revision{}
	for _, r := range history {
		if r.ID == "" || r.File == "" || r.Digest == "" || byID[r.ID].ID != "" {
			return Changeset{}, stale("invalid or duplicate revision")
		}
		byID[r.ID] = r
	}
	if _, ok := byID[head.Revision]; !ok {
		return Changeset{}, stale("head %q missing", head.Revision)
	}
	seen := map[string]bool{}
	selected := []Revision{}
	cur := head.Revision
	for cur != "" && cur != base.Revision {
		if seen[cur] {
			return Changeset{}, stale("cycle at %q", cur)
		}
		seen[cur] = true
		r := byID[cur]
		if r.ID == "" {
			return Changeset{}, stale("missing ancestry at %q", cur)
		}
		if migrationDir != "" && !strings.HasPrefix(r.File, strings.TrimSuffix(migrationDir, "/")+"/") {
			return Changeset{}, stale("revision %q outside migration directory", r.ID)
		}
		selected = append(selected, r)
		cur = r.Parent
	}
	if cur != base.Revision {
		return Changeset{}, stale("base %q is not an ancestor", base.Revision)
	}
	sort.SliceStable(selected, func(i, j int) bool { return selected[i].ID < selected[j].ID })
	out := Changeset{Version: ContractVersion, Base: base, Head: head, Revisions: selected}
	raw, _ := json.Marshal(out)
	h := sha256.Sum256(raw)
	out.Digest = "sha256:" + hex.EncodeToString(h[:])
	return out, nil
}

func validReference(r Reference) bool {
	if r.Value == "" || r.Revision == "" {
		return false
	}
	switch r.Kind {
	case Branch, Tag, Registry, MigrationDirectory:
		return true
	default:
		return false
	}
}

func stale(format string, args ...any) error {
	return fmt.Errorf("%w: %s; rebase onto the selected base and retry", ErrStaleHistory, fmt.Sprintf(format, args...))
}

type Diagnostic struct {
	Code     string `json:"code"`
	Severity string `json:"severity"`
	Message  string `json:"message"`
	File     string `json:"file,omitempty"`
	Line     int    `json:"line,omitempty"`
	Column   int    `json:"column,omitempty"`
}
type StageResult struct {
	Stage        string       `json:"stage"`
	Passed       bool         `json:"passed"`
	Diagnostics  []Diagnostic `json:"diagnostics,omitempty"`
	ResultDigest string       `json:"result_digest"`
}
type Review struct {
	Version        string        `json:"version"`
	Changeset      Changeset     `json:"changeset"`
	ArtifactDigest string        `json:"artifact_digest"`
	PolicyVersion  string        `json:"policy_version"`
	Stages         []StageResult `json:"stages"`
	Attestation    *Attestation  `json:"attestation,omitempty"`
}
type Attestation struct {
	SourceRevision  string `json:"source_revision"`
	ChangesetDigest string `json:"changeset_digest"`
	ArtifactDigest  string `json:"artifact_digest"`
	PolicyVersion   string `json:"policy_version"`
	TestsDigest     string `json:"tests_digest"`
	Algorithm       string `json:"algorithm"`
	KeyID           string `json:"key_id"`
	Signature       string `json:"signature"`
}

type Stage interface {
	Name() string
	Run(Changeset) StageResult
}
type Pipeline struct {
	Stages                        []Stage
	PolicyVersion, ArtifactDigest string
	SigningKey                    ed25519.PrivateKey
	KeyID                         string
}

func (p Pipeline) Run(c Changeset) (Review, error) {
	if c.Version != ContractVersion || c.Digest == "" {
		return Review{}, fmt.Errorf("invalid changeset")
	}
	if p.ArtifactDigest == "" || p.PolicyVersion == "" {
		return Review{}, fmt.Errorf("artifact digest and policy version are required")
	}
	out := Review{Version: ContractVersion, Changeset: c, ArtifactDigest: p.ArtifactDigest, PolicyVersion: p.PolicyVersion}
	seen := map[string]bool{}
	for _, s := range p.Stages {
		if s == nil || s.Name() == "" || seen[s.Name()] {
			return Review{}, fmt.Errorf("invalid or duplicate review stage")
		}
		seen[s.Name()] = true
		r := s.Run(c)
		if r.Stage == "" {
			r.Stage = s.Name()
		}
		if r.ResultDigest == "" {
			raw, _ := json.Marshal(r.Diagnostics)
			h := sha256.Sum256(raw)
			r.ResultDigest = "sha256:" + hex.EncodeToString(h[:])
		}
		out.Stages = append(out.Stages, r)
	}
	if p.SigningKey != nil {
		tests := []string{}
		for _, s := range out.Stages {
			if strings.Contains(strings.ToLower(s.Stage), "test") {
				tests = append(tests, s.ResultDigest)
			}
		}
		raw, _ := json.Marshal(tests)
		th := sha256.Sum256(raw)
		a := Attestation{SourceRevision: c.Head.Revision, ChangesetDigest: c.Digest, ArtifactDigest: p.ArtifactDigest, PolicyVersion: p.PolicyVersion, TestsDigest: "sha256:" + hex.EncodeToString(th[:]), Algorithm: "ed25519", KeyID: p.KeyID}
		payload, _ := json.Marshal(a)
		sig := ed25519.Sign(p.SigningKey, payload)
		a.Signature = hex.EncodeToString(sig)
		out.Attestation = &a
	}
	return out, nil
}

func (r Review) Passed() bool {
	if len(r.Stages) == 0 {
		return false
	}
	for _, s := range r.Stages {
		if !s.Passed {
			return false
		}
	}
	return true
}
func (r Review) JSON() ([]byte, error) { return json.MarshalIndent(r, "", "  ") }
func (r Review) Terminal() string {
	var b strings.Builder
	fmt.Fprintf(&b, "changeset %s (%d revisions)\n", r.Changeset.Digest, len(r.Changeset.Revisions))
	for _, s := range r.Stages {
		state := "FAIL"
		if s.Passed {
			state = "PASS"
		}
		fmt.Fprintf(&b, "%s %s\n", state, s.Stage)
		for _, d := range s.Diagnostics {
			fmt.Fprintf(&b, "  %s: %s", d.Severity, d.Message)
			if d.File != "" {
				fmt.Fprintf(&b, " (%s:%d:%d)", d.File, d.Line, d.Column)
			}
			b.WriteByte('\n')
		}
	}
	return b.String()
}

type PRAnnotation struct {
	Level       string `json:"level"`
	Message     string `json:"message"`
	Path        string `json:"path,omitempty"`
	StartLine   int    `json:"start_line,omitempty"`
	StartColumn int    `json:"start_column,omitempty"`
	Code        string `json:"code,omitempty"`
}

func (r Review) Annotations() []PRAnnotation {
	var out []PRAnnotation
	for _, s := range r.Stages {
		for _, d := range s.Diagnostics {
			out = append(out, PRAnnotation{Level: d.Severity, Message: d.Message, Path: d.File, StartLine: d.Line, StartColumn: d.Column, Code: d.Code})
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Path+out[i].Message < out[j].Path+out[j].Message })
	return out
}
func (r Review) SARIF() ([]byte, error) {
	results := []map[string]any{}
	for _, a := range r.Annotations() {
		x := map[string]any{"ruleId": a.Code, "level": a.Level, "message": map[string]string{"text": a.Message}}
		if a.Path != "" {
			x["locations"] = []any{map[string]any{"physicalLocation": map[string]any{"artifactLocation": map[string]string{"uri": a.Path}, "region": map[string]int{"startLine": a.StartLine, "startColumn": a.StartColumn}}}}
		}
		results = append(results, x)
	}
	return json.MarshalIndent(map[string]any{"version": "2.1.0", "runs": []any{map[string]any{"tool": map[string]any{"driver": map[string]string{"name": "AutoSQL CI"}}, "results": results}}}, "", "  ")
}

// VerifyAttestation verifies the signature over all binding fields. Callers
// should additionally compare ArtifactDigest and PolicyVersion to trusted CI
// configuration before accepting a merge.
func VerifyAttestation(a Attestation, public ed25519.PublicKey) bool {
	if a.Algorithm != "ed25519" || a.Signature == "" {
		return false
	}
	sig, err := hex.DecodeString(a.Signature)
	if err != nil {
		return false
	}
	copyA := a
	copyA.Signature = ""
	raw, _ := json.Marshal(copyA)
	return ed25519.Verify(public, raw, sig)
}
