// Package down plans fail-closed, append-only reversal migrations.
package down

import (
	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/guardrail"
	"autosql/pkg/migrate"
	"autosql/pkg/migrate/revision"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/precheck"
	"autosql/pkg/schema"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

var ErrRefused = errors.New("controlled down migration refused")
var ErrStale = errors.New("controlled down plan is stale")
var ErrIrreversible = errors.New("migration requires trusted irreversible override")

type Original struct {
	Version, ArtifactDigest, PlanDigest string `json:",omitempty"`
}
type ReverseStatement struct {
	OriginalVersion, OriginalArtifactDigest, Scope, SQL, Digest string
	Transactional                                               bool
}
type Impact struct {
	ChangeID, Operation, Object string
	Destructive                 bool
	Preconditions               []string
}
type Override struct {
	Actor, Reason    string
	Scopes           []string
	ExpiresAt        time.Time
	KeyID, Signature string
}
type DownPlan struct {
	Version                                                                            string
	ManifestDigest, ManifestGeneration, HeadVersion, HeadArtifactDigest, TargetVersion string
	LiveFingerprint, PriorFingerprint                                                  string
	Originals                                                                          []Original
	Plan                                                                               plan.Plan
	Checks                                                                             precheck.Plan
	Reverse                                                                            []ReverseStatement
	Impacts                                                                            []Impact
	Override                                                                           *Override `json:",omitempty"`
	CreatedAt, ExpiresAt                                                               time.Time
	SignerKeyID, Digest, Signature                                                     string
	ArtifactPath, ArtifactDigest, GuardrailDigest                                      string
}

func (p DownPlan) BindArtifact(path, digestValue, bundle string, key ed25519.PrivateKey) (DownPlan, error) {
	if path == "" || digestValue == "" || bundle == "" || len(key) != ed25519.PrivateKeySize {
		return DownPlan{}, ErrRefused
	}
	p.ArtifactPath, p.ArtifactDigest, p.GuardrailDigest = path, digestValue, bundle
	p.Digest = digest(p)
	p.Signature = base64.RawStdEncoding.EncodeToString(ed25519.Sign(key, []byte("autosql.down-plan.signature/v1\x00"+p.Digest)))
	return p, nil
}

type Request struct {
	Snapshot                  migrate.Snapshot
	Revisions                 []revision.Revision
	TargetVersion             string
	LockedLive, ReplayedPrior schema.Document
	Reverse                   []ReverseStatement
	Checks                    []precheck.Assertion
	Override                  *Override
	OverrideKeys              map[string]ed25519.PublicKey
	Now, ExpiresAt            time.Time
	SignerKeyID               string
	Signer                    ed25519.PrivateKey
	VerifyOriginal            func(artifact.Artifact) (artifact.VerifiedArtifact, error)
	Executor                  []revision.ExecutorRecord
	TargetIdentity            string
}
type Reload func(context.Context) (migrate.Snapshot, revision.Revision, error)
type Authorize func(context.Context, DownPlan) (artifact.VerifiedArtifact, error)

// ExecuteReversal owns the canonical target lock, executor transaction and
// append-only revision/event write. Success means DDL, executor evidence, a
// reversal revision, and reversal events linking every Original committed
// together; implementations must never update/delete prior revision rows.
type ExecuteReversal func(context.Context, DownPlan, artifact.VerifiedArtifact) error
type Engine struct {
	Reload    Reload
	Authorize Authorize
	Execute   ExecuteReversal
	PlanKey   ed25519.PublicKey
}

func (e Engine) Apply(ctx context.Context, p DownPlan) error {
	if e.Reload == nil || e.Authorize == nil || e.Execute == nil || len(e.PlanKey) != ed25519.PublicKeySize {
		return ErrRefused
	}
	snap, head, err := e.Reload(ctx)
	if err != nil {
		return ErrRefused
	}
	if err = p.Verify(e.PlanKey, time.Now().UTC(), snap.Manifest, head); err != nil {
		return err
	}
	v, err := e.Authorize(ctx, p)
	if err != nil {
		return ErrRefused
	}
	a, err := v.Payload()
	if err != nil || a.Plan.Digest != p.Plan.Digest || a.Checks.Digest != p.Checks.Digest || a.SourceRevision != "down:"+p.HeadVersion+":"+p.TargetVersion {
		return ErrRefused
	}
	if err = e.Execute(ctx, p, v); err != nil {
		return err
	}
	return nil
}

func Build(ctx context.Context, r Request) (DownPlan, error) {
	var out DownPlan
	if r.Snapshot.Manifest.Digest == "" || r.TargetVersion == "" || len(r.Revisions) == 0 || r.VerifyOriginal == nil || r.Now.IsZero() || !r.ExpiresAt.After(r.Now) || r.SignerKeyID == "" || len(r.Signer) != ed25519.PrivateKeySize {
		return out, ErrRefused
	}
	targetVersion, versionErr := migrate.ParseVersion(r.TargetVersion)
	if versionErr != nil {
		return out, ErrRefused
	}
	r.TargetVersion = targetVersion.String()
	head := r.Revisions[len(r.Revisions)-1]
	if head.State != "applied" && head.State != "baseline" && head.State != "checkpoint" {
		return out, fmt.Errorf("%w: current revision is partial, failed, or uncertain; reconcile status before down", ErrRefused)
	}
	target, headIndex := -1, -1
	logicalHead := head.Version
	if head.Kind == "reversal" && head.ToVersion != "" {
		logicalHead = head.ToVersion
	}
	for i, e := range r.Snapshot.Manifest.Entries {
		if e.Version == r.TargetVersion {
			target = i
		}
		if e.Version == logicalHead {
			headIndex = i
		}
	}
	if target < 0 || headIndex < 0 || target >= headIndex {
		return out, ErrRefused
	}
	rows := map[string]revision.Revision{}
	for _, row := range r.Revisions {
		if row.Kind == "reversal" {
			continue
		}
		if _, exists := rows[row.Version]; exists {
			return out, fmt.Errorf("%w: duplicate revision evidence", ErrRefused)
		}
		rows[row.Version] = row
	}
	for i := target + 1; i <= headIndex; i++ {
		entry := r.Snapshot.Manifest.Entries[i]
		row, ok := rows[entry.Version]
		if !ok || row.State != "applied" || row.Kind == "baseline" || row.FileName != entry.File || row.FileDigest != entry.SQLDigest || row.ArtifactDigest != entry.ArtifactDigest || row.PlanDigest != entry.Directives.PlanDigest || row.ChecksDigest != entry.Directives.CheckDigest || row.BundleDigest != entry.Directives.BundleDigest {
			return out, fmt.Errorf("%w: revision evidence gap, baseline ambiguity, or digest mismatch", ErrRefused)
		}
	}
	originals := []Original{}
	originalByVersion := map[string]string{}
	originalPlans := map[string]artifact.Artifact{}
	requiredReverse := map[string]bool{}
	for i := target + 1; i <= headIndex; i++ {
		e := r.Snapshot.Manifest.Entries[i]
		if e.ArtifactFile == "" {
			return out, ErrRefused
		}
		a, err := artifact.Parse(r.Snapshot.Files[e.ArtifactFile])
		if err != nil {
			return out, ErrRefused
		}
		v, err := r.VerifyOriginal(a)
		if err != nil {
			return out, ErrRefused
		}
		p, err := v.Payload()
		if err != nil {
			return out, ErrRefused
		}
		originals = append(originals, Original{e.Version, p.Digest, p.Plan.Digest})
		originalByVersion[e.Version] = p.Digest
		originalPlans[p.Digest] = p
		for _, c := range p.Plan.Changes.Changes {
			if c.Before != nil && c.Before.Kind == schema.KindReferenceData || c.After != nil && c.After.Kind == schema.KindReferenceData {
				requiredReverse[e.Version] = true
			}
		}
	}
	if len(originals) == 0 {
		return out, ErrRefused
	}
	byArtifact := map[string][]revision.ExecutorRecord{}
	seenHistory := map[string]bool{}
	for _, x := range r.Executor {
		k := x.ArtifactDigest + "\x00" + x.StepID + fmt.Sprint(x.Attempt)
		if seenHistory[k] {
			return out, fmt.Errorf("%w: duplicate executor evidence", ErrRefused)
		}
		seenHistory[k] = true
		byArtifact[x.ArtifactDigest] = append(byArtifact[x.ArtifactDigest], x)
	}
	for _, o := range originals {
		a := originalPlans[o.ArtifactDigest]
		expected := map[string]plan.Phase{}
		for _, phase := range a.Plan.Phases {
			for _, id := range phase.StepIDs {
				for _, step := range a.Plan.Steps {
					if step.ID == id && step.Kind == plan.StepExecutable {
						expected[id] = phase
					}
				}
			}
		}
		historyRows := byArtifact[o.ArtifactDigest]
		if len(historyRows) != len(expected) {
			return out, fmt.Errorf("%w: executor evidence gap", ErrRefused)
		}
		for _, x := range historyRows {
			phase, ok := expected[x.StepID]
			var step plan.Step
			for _, candidate := range a.Plan.Steps {
				if candidate.ID == x.StepID {
					step = candidate
				}
			}
			if !ok || x.State != "confirmed" || x.Attempt != 1 || x.StepHash != executor.StepHash(step) || x.PhaseID != phase.ID || x.PhaseMode != string(phase.Transaction) || x.ExecutionID != a.Digest || x.PlanDigest != a.Plan.Digest || x.BundleDigest != a.GuardrailDigest || r.TargetIdentity != "" && x.TargetIdentity != r.TargetIdentity {
				return out, fmt.Errorf("%w: executor evidence binding mismatch", ErrRefused)
			}
		}
	}
	liveFP, e := schema.SemanticFingerprint(r.LockedLive)
	if e != nil {
		return out, ErrRefused
	}
	priorFP, e := schema.SemanticFingerprint(r.ReplayedPrior)
	if e != nil {
		return out, ErrRefused
	}
	p, e := plan.Build(ctx, postgres.New(), r.LockedLive, r.ReplayedPrior, plan.Options{})
	if e != nil {
		return out, e
	}
	if p.FromFingerprint != liveFP || p.ToFingerprint != priorFP {
		return out, ErrRefused
	}
	for _, phase := range p.Phases {
		if phase.Transaction == plan.TransactionProhibited && !validOverride(r.Override, r.OverrideKeys, r.Now, "nontransactional") {
			return out, fmt.Errorf("%w: nontransactional reversal has ambiguous recovery semantics", ErrIrreversible)
		}
	}
	reverse := append([]ReverseStatement(nil), r.Reverse...)
	seen := map[string]bool{}
	for i := range reverse {
		x := &reverse[i]
		if strings.TrimSpace(x.SQL) == "" || x.OriginalArtifactDigest == "" || x.Scope == "" || originalByVersion[x.OriginalVersion] != x.OriginalArtifactDigest || seen[x.OriginalVersion+"\x00"+x.Scope] {
			return out, ErrRefused
		}
		x.Digest = hash("reverse", []byte(strings.Join([]string{x.OriginalVersion, x.OriginalArtifactDigest, x.Scope, x.SQL, fmt.Sprint(x.Transactional)}, "\x00")))
		seen[x.OriginalVersion] = true
		seen[x.OriginalVersion+"\x00"+x.Scope] = true
	}
	for version := range requiredReverse {
		if !seen[version] {
			if !validOverride(r.Override, r.OverrideKeys, r.Now, "data:"+version) {
				return out, ErrIrreversible
			}
		}
	}
	author := make([]plan.AuthorSQL, len(reverse))
	for i, x := range reverse {
		author[i] = plan.AuthorSQL{ID: x.OriginalVersion + ":" + x.Scope, SQL: x.SQL, Transactional: x.Transactional}
	}
	p, e = plan.AppendAuthorSQL(p, author)
	if e != nil {
		return out, e
	}
	for _, phase := range p.Phases {
		if phase.Transaction == plan.TransactionProhibited && !validOverride(r.Override, r.OverrideKeys, r.Now, "nontransactional") {
			return out, fmt.Errorf("%w: nontransactional reversal has ambiguous recovery semantics", ErrIrreversible)
		}
	}
	impacts := impacts(p)
	destructive := false
	for _, x := range impacts {
		destructive = destructive || x.Destructive
	}
	if destructive && !validOverride(r.Override, r.OverrideKeys, r.Now, "destructive") { /* ordinary approval still occurs later; destructive DDL remains reviewable */
	}
	checks := precheck.Plan{ID: "down:" + head.Version + ":" + r.TargetVersion, Statements: statements(p), Assertions: append([]precheck.Assertion(nil), r.Checks...)}
	cd, err := guardrail.ChangeDigest(p.Changes)
	if err != nil {
		return out, err
	}
	checks.ChangeDigest = cd
	for i := range checks.Assertions {
		checks.Assertions[i].ChangeDigest = cd
		checks.Assertions[i].PlanDigest = ""
	}
	checks.Digest, e = precheck.Digest(checks)
	if e != nil {
		return out, e
	}
	for i := range checks.Assertions {
		checks.Assertions[i].PlanDigest = checks.Digest
	}
	out = DownPlan{Version: "autosql.down-plan/v1", ManifestDigest: r.Snapshot.Manifest.Digest, ManifestGeneration: r.Snapshot.Manifest.Generation, HeadVersion: head.Version, HeadArtifactDigest: head.ArtifactDigest, TargetVersion: r.TargetVersion, LiveFingerprint: liveFP, PriorFingerprint: priorFP, Originals: originals, Plan: p, Checks: checks, Reverse: reverse, Impacts: impacts, Override: r.Override, CreatedAt: r.Now.UTC(), ExpiresAt: r.ExpiresAt.UTC(), SignerKeyID: r.SignerKeyID}
	out.Digest = digest(out)
	out.Signature = base64.RawStdEncoding.EncodeToString(ed25519.Sign(r.Signer, []byte("autosql.down-plan.signature/v1\x00"+out.Digest)))
	return out, nil
}
func (p DownPlan) Verify(pub ed25519.PublicKey, now time.Time, manifest migrate.Manifest, head revision.Revision) error {
	if p.Version != "autosql.down-plan/v1" || p.Digest != digest(p) || now.Before(p.CreatedAt) || !now.Before(p.ExpiresAt) || manifest.Digest != p.ManifestDigest || manifest.Generation != p.ManifestGeneration || head.Version != p.HeadVersion || head.ArtifactDigest != p.HeadArtifactDigest {
		return ErrStale
	}
	sig, e := base64.RawStdEncoding.Strict().DecodeString(p.Signature)
	if e != nil || !ed25519.Verify(pub, []byte("autosql.down-plan.signature/v1\x00"+p.Digest), sig) {
		return ErrRefused
	}
	return nil
}
func impacts(p plan.Plan) []Impact {
	out := []Impact{}
	for _, c := range p.Changes.Changes {
		x := Impact{ChangeID: c.ID, Operation: string(c.Operation), Object: c.ResourceID}
		if c.Operation == schema.OperationDrop {
			x.Destructive = true
			x.Preconditions = []string{"object contents and dependents must be safe to remove"}
		}
		out = append(out, x)
	}
	return out
}
func statements(p plan.Plan) []string {
	var out []string
	for _, x := range p.Steps {
		if x.Kind == plan.StepExecutable {
			out = append(out, x.SQL)
		}
	}
	return out
}
func validOverride(o *Override, keys map[string]ed25519.PublicKey, now time.Time, scope string) bool {
	if o == nil || o.Actor == "" || o.Reason == "" || !o.ExpiresAt.After(now) {
		return false
	}
	scopes := append([]string(nil), o.Scopes...)
	sort.Strings(scopes)
	found := false
	for _, x := range scopes {
		found = found || x == scope
	}
	pub := keys[o.KeyID]
	sig, e := base64.RawStdEncoding.Strict().DecodeString(o.Signature)
	copy := *o
	copy.Signature = ""
	raw, _ := json.Marshal(copy)
	return found && e == nil && ed25519.Verify(pub, append([]byte("autosql.down.override/v1\x00"), raw...), sig)
}
func digest(p DownPlan) string {
	p.Digest = ""
	p.Signature = ""
	raw, _ := json.Marshal(p)
	return hash("plan", raw)
}
func hash(domain string, b []byte) string {
	x := sha256.Sum256(append([]byte("autosql.down."+domain+"/v1\x00"), b...))
	return "sha256:" + hex.EncodeToString(x[:])
}
