package ci_test

import (
	"crypto/ed25519"
	"testing"

	"autosql/pkg/ci"
)

type stage struct {
	name   string
	passed bool
}

func (s stage) Name() string { return s.name }
func (s stage) Run(ci.Changeset) ci.StageResult {
	return ci.StageResult{Stage: s.name, Passed: s.passed, Diagnostics: []ci.Diagnostic{{Code: "fixture", Severity: "warning", Message: "review", File: "migrations/001.sql", Line: 2, Column: 1}}}
}

func history() []ci.Revision {
	return []ci.Revision{{ID: "r1", File: "migrations/001.sql", Digest: "sha256:a"}, {ID: "r2", Parent: "r1", File: "migrations/002.sql", Digest: "sha256:b"}, {ID: "r3", Parent: "r2", File: "migrations/003.sql", Digest: "sha256:c"}}
}
func TestDetectOnlyAncestralMigrationChanges(t *testing.T) {
	c, err := ci.Detect(ci.Reference{Kind: ci.Branch, Value: "main", Revision: "r1"}, ci.Reference{Kind: ci.Branch, Value: "feature", Revision: "r3"}, history(), "migrations")
	if err != nil {
		t.Fatal(err)
	}
	if len(c.Revisions) != 2 || c.Digest == "" {
		t.Fatalf("changeset=%+v", c)
	}
}
func TestDetectRejectsStaleAndOutOfTree(t *testing.T) {
	_, err := ci.Detect(ci.Reference{Value: "main", Revision: "missing"}, ci.Reference{Value: "head", Revision: "r3"}, history(), "migrations")
	if err == nil {
		t.Fatal("accepted non-ancestor")
	}
	h := history()
	h[1].File = "other.sql"
	_, err = ci.Detect(ci.Reference{Value: "main", Revision: "r1"}, ci.Reference{Value: "head", Revision: "r3"}, h, "migrations")
	if err == nil {
		t.Fatal("accepted out-of-tree revision")
	}
}
func TestPipelineBindsSignedReviewAndReports(t *testing.T) {
	c, _ := ci.Detect(ci.Reference{Kind: ci.Branch, Value: "main", Revision: "r1"}, ci.Reference{Kind: ci.Branch, Value: "head", Revision: "r3"}, history(), "migrations")
	pub, priv, _ := ed25519.GenerateKey(nil)
	r, err := (ci.Pipeline{Stages: []ci.Stage{stage{"replay", true}, stage{"tests", true}}, ArtifactDigest: "sha256:artifact", PolicyVersion: "policy-4", SigningKey: priv, KeyID: "ci"}).Run(c)
	if err != nil || !r.Passed() || r.Attestation == nil || !ci.VerifyAttestation(*r.Attestation, pub) {
		t.Fatalf("review=%+v err=%v", r, err)
	}
	if len(r.Annotations()) != 2 {
		t.Fatal("missing neutral annotations")
	}
	if _, err := r.SARIF(); err != nil {
		t.Fatal(err)
	}
	if r.Terminal() == "" {
		t.Fatal("missing terminal output")
	}
}
