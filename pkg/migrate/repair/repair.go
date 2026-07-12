// Package repair provides signed, compare-and-swap repair of migration evidence.
package repair

import (
	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/migrate"
	"autosql/pkg/migrate/revision"
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"
)

var ErrRefused = errors.New("migration repair refused")

type Proposal struct {
	Version, Action, TargetVersion, Reason, Operator, DatabaseIdentity, Environment, ExpectedBeforeDigest, ExpectedBeforeState, ExpectedAfterState, ManifestDigest, GuardrailDigest, ApprovalDigest, ApprovalLevel, Digest string
	CreatedAt, ExpiresAt                                                                                                                                                                                                   time.Time
	Signature                                                                                                                                                                                                              artifact.Signature
}

func (p Proposal) canonical() ([]byte, error) {
	x := p
	x.Digest = ""
	x.Signature = artifact.Signature{}
	return json.Marshal(x)
}
func (p *Proposal) Sign(keyID string, key ed25519.PrivateKey) error {
	if err := p.validate(); err != nil {
		return err
	}
	raw, _ := p.canonical()
	sum := sha256.Sum256(append([]byte("autosql.repair.proposal/v1\x00"), raw...))
	p.Digest = "sha256:" + hex.EncodeToString(sum[:])
	p.Signature = artifact.Signature{KeyID: keyID, Algorithm: "Ed25519", Value: base64.RawStdEncoding.EncodeToString(ed25519.Sign(key, []byte(p.Digest)))}
	return nil
}
func (p Proposal) Verify(keys map[string]ed25519.PublicKey, now time.Time) error {
	if err := p.validate(); err != nil {
		return err
	}
	raw, _ := p.canonical()
	sum := sha256.Sum256(append([]byte("autosql.repair.proposal/v1\x00"), raw...))
	want := "sha256:" + hex.EncodeToString(sum[:])
	key, ok := keys[p.Signature.KeyID]
	sig, e := base64.RawStdEncoding.Strict().DecodeString(p.Signature.Value)
	if !ok || p.Digest != want || p.Signature.Algorithm != "Ed25519" || e != nil || !ed25519.Verify(key, []byte(p.Digest), sig) || now.Before(p.CreatedAt) || !now.Before(p.ExpiresAt) {
		return ErrRefused
	}
	return nil
}
func (p Proposal) validate() error {
	if p.Version != "autosql.repair-proposal/v1" || p.TargetVersion == "" || p.Operator == "" || p.DatabaseIdentity == "" || p.Environment == "" || len(p.Reason) < 8 || len(p.Reason) > 500 || p.ExpectedBeforeDigest == "" || p.ExpectedBeforeState == "" || p.ExpectedAfterState == "" || p.ManifestDigest == "" || p.GuardrailDigest == "" || p.ApprovalDigest == "" || p.CreatedAt.IsZero() || !p.ExpiresAt.After(p.CreatedAt) {
		return ErrRefused
	}
	switch p.Action {
	case "mark", "remove", "reconcile":
	default:
		return ErrRefused
	}
	return nil
}

type AuditRecord struct {
	Type, ProposalDigest, Action, TargetVersion, Operator string
	At                                                    time.Time
}
type Audit interface {
	AppendDurable(context.Context, AuditRecord) error
}
type VerifyArtifact func(artifact.Artifact) (artifact.VerifiedArtifact, error)
type Fingerprint func(context.Context) (string, error)
type Service struct {
	Store           *revision.Store
	Verify          VerifyArtifact
	LiveFingerprint Fingerprint
	Audit           Audit
	Keys            map[string]ed25519.PublicKey
	Now             func() time.Time
	LockIdentity    string
}

type Divergence struct {
	Version, Kind, Expected, Actual, RootCause, SuggestedCommand string `json:",omitempty"`
}
type Diagnosis struct {
	Status, ManifestDigest, LiveFingerprint, ExpectedFingerprint string
	First                                                        *Divergence `json:",omitempty"`
	Revisions                                                    int
	History                                                      int
}

func (s Service) Diagnose(ctx context.Context, dir string) (Diagnosis, error) {
	var out Diagnosis
	if s.Store == nil || s.Verify == nil || s.LiveFingerprint == nil {
		return out, ErrRefused
	}
	snap, e := migrate.LoadSnapshot(dir)
	if e != nil {
		return out, ErrRefused
	}
	if len(snap.Manifest.Entries) == 0 {
		return out, ErrRefused
	}
	first := snap.Manifest.Entries[0]
	a, e := artifact.Parse(snap.Files[first.ArtifactFile])
	if e != nil {
		return out, ErrRefused
	}
	verified, e := s.Verify(a)
	if e != nil {
		return out, ErrRefused
	}
	payload, e := verified.Payload()
	if e != nil {
		return out, ErrRefused
	}
	lockIdentity, e := executor.LockKey(payload.DatabaseIdentity, payload.TargetEnvironment)
	if e != nil {
		return out, ErrRefused
	}
	session, e := s.Store.OpenSession(ctx)
	if e != nil {
		return out, e
	}
	defer session.Close(context.Background())
	if ok, e := session.Lock(ctx, lockIdentity); e != nil || !ok {
		return out, ErrRefused
	}
	defer session.Unlock(context.Background(), lockIdentity)
	rows, e := session.Revisions(ctx)
	if e != nil {
		return out, e
	}
	out.ManifestDigest = snap.Manifest.Digest
	out.Revisions = len(rows)
	history, e := session.ExecutorRecords(ctx, payload.DatabaseIdentity+"/"+payload.TargetEnvironment)
	if e != nil {
		return out, e
	}
	out.History = len(history)
	by := map[string]revision.Revision{}
	for _, r := range rows {
		by[r.Version] = r
	}
	for _, r := range rows {
		found := false
		for _, m := range snap.Manifest.Entries {
			if m.Version == r.Version {
				found = true
				if r.State == "pending" || r.State == "partial" || r.State == "failed" {
					out.First = &Divergence{Version: r.Version, Kind: "dirty", Expected: "applied", Actual: r.State, RootCause: "incomplete revision evidence", SuggestedCommand: safeCommand("reconcile", r.Version)}
					break
				}
				if r.FileDigest != m.SQLDigest || r.ArtifactDigest != m.ArtifactDigest {
					out.First = &Divergence{Version: r.Version, Kind: "checksum", Expected: m.SQLDigest, Actual: r.FileDigest, RootCause: "recorded revision differs from verified manifest", SuggestedCommand: safeCommand("remove", r.Version)}
					break
				}
			}
		}
		if out.First != nil {
			break
		}
		if !found {
			out.First = &Divergence{Version: r.Version, Kind: "unknown", Actual: r.FileName, RootCause: "revision is absent from verified manifest", SuggestedCommand: safeCommand("remove", r.Version)}
			break
		}
	}
	if out.First == nil {
		for _, m := range snap.Manifest.Entries {
			if _, ok := by[m.Version]; !ok {
				break
			}
			raw := snap.Files[m.ArtifactFile]
			a, e := artifact.Parse(raw)
			if e != nil {
				return out, ErrRefused
			}
			v, e := s.Verify(a)
			if e != nil {
				return out, ErrRefused
			}
			p, _ := v.Payload()
			out.ExpectedFingerprint = p.Plan.ToFingerprint
		}
	}
	live, e := s.LiveFingerprint(ctx)
	if e != nil {
		return out, e
	}
	out.LiveFingerprint = live
	if out.First == nil && out.ExpectedFingerprint != "" && live != out.ExpectedFingerprint {
		out.First = &Divergence{Kind: "manual_drift", Expected: out.ExpectedFingerprint, Actual: live, RootCause: "live canonical schema differs from applied head", SuggestedCommand: "autosql migrate diagnose --config <trusted-config> --env <environment>"}
	}
	if out.First == nil {
		out.Status = "consistent"
	} else {
		out.Status = "diverged"
	}
	return out, nil
}
func safeCommand(action, version string) string {
	return fmt.Sprintf("autosql migrate repair %s --proposal <signed-proposal.json> --config <trusted-config> --target-version %s", action, version)
}

func (s Service) Apply(ctx context.Context, p Proposal) error {
	if s.Store == nil || s.Audit == nil || s.Now == nil || p.Verify(s.Keys, s.Now()) != nil {
		return ErrRefused
	}
	if p.Action == "remove" && p.ApprovalLevel != "destructive" {
		return s.refuse(ctx, p)
	}
	session, e := s.Store.OpenSession(ctx)
	if e != nil {
		return e
	}
	defer session.Close(context.Background())
	lockIdentity, e := executor.LockKey(p.DatabaseIdentity, p.Environment)
	if e != nil {
		return s.refuse(ctx, p)
	}
	ok, e := session.Lock(ctx, lockIdentity)
	if e != nil || !ok {
		return s.refuse(ctx, p)
	}
	defer session.Unlock(context.Background(), lockIdentity)
	rows, e := session.Revisions(ctx)
	if e != nil {
		return e
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].Version < rows[j].Version })
	var before *revision.Revision
	for i := range rows {
		if rows[i].Version == p.TargetVersion {
			before = &rows[i]
			break
		}
	}
	if before == nil || before.State != p.ExpectedBeforeState || revisionDigest(*before) != p.ExpectedBeforeDigest || before.ManifestDigest != p.ManifestDigest || before.BundleDigest != p.GuardrailDigest {
		return s.refuse(ctx, p)
	}
	if e = s.Audit.AppendDurable(ctx, AuditRecord{Type: "repair_requested", ProposalDigest: p.Digest, Action: p.Action, TargetVersion: p.TargetVersion, Operator: p.Operator, At: s.Now()}); e != nil {
		return e
	}
	if e = s.Audit.AppendDurable(ctx, AuditRecord{Type: "repair_applied", ProposalDigest: p.Digest, Action: p.Action, TargetVersion: p.TargetVersion, Operator: p.Operator, At: s.Now()}); e != nil {
		return e
	}
	tx, e := session.Begin(ctx)
	if e != nil {
		return e
	}
	defer tx.Rollback(context.Background())
	if e = session.Repair(ctx, tx, p.TargetVersion, p.Action, p.ExpectedBeforeState, p.ExpectedAfterState, p.Digest, p.Operator, s.Now()); e != nil {
		return e
	}
	return tx.Commit(ctx)
}
func (s Service) refuse(ctx context.Context, p Proposal) error {
	if s.Audit != nil && s.Now != nil {
		_ = s.Audit.AppendDurable(ctx, AuditRecord{Type: "repair_refused", ProposalDigest: p.Digest, Action: p.Action, TargetVersion: p.TargetVersion, Operator: p.Operator, At: s.Now()})
	}
	return ErrRefused
}
func revisionDigest(r revision.Revision) string {
	raw, _ := json.Marshal(struct{ Version, State, FileDigest, ArtifactDigest, ManifestDigest string }{r.Version, r.State, r.FileDigest, r.ArtifactDigest, r.ManifestDigest})
	sum := sha256.Sum256(append([]byte("autosql.repair.revision/v1\x00"), raw...))
	return "sha256:" + hex.EncodeToString(sum[:])
}
func RevisionDigest(r revision.Revision) string { return revisionDigest(r) }
