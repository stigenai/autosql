package repair

import (
	"autosql/pkg/migrate/revision"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"os"
	"strings"
	"testing"
	"time"
)

type auditLog struct {
	records []AuditRecord
	failAt  int
}

func (a *auditLog) AppendDurable(_ context.Context, r AuditRecord) error {
	if a.failAt > 0 && len(a.records)+1 == a.failAt {
		return errors.New("audit failure password=secret")
	}
	a.records = append(a.records, r)
	return nil
}
func proposal(t *testing.T, r revision.Revision, action, after string, key ed25519.PrivateKey, now time.Time) Proposal {
	t.Helper()
	p := Proposal{Version: "autosql.repair-proposal/v1", Action: action, TargetVersion: r.Version, Reason: "approved repair after incident review", Operator: "trusted-operator", DatabaseIdentity: "repair-test", Environment: "test", ExpectedBeforeDigest: RevisionDigest(r), ExpectedBeforeState: r.State, ExpectedAfterState: after, ManifestDigest: r.ManifestDigest, GuardrailDigest: r.BundleDigest, ApprovalDigest: "sha256:approval", CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}
	if action == "remove" {
		p.ApprovalLevel = "destructive"
	}
	if e := p.Sign("operator", key); e != nil {
		t.Fatal(e)
	}
	return p
}
func TestProposalTamperExpiryAndReason(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC()
	r := revision.Revision{Version: "1", State: "partial", FileDigest: "f", ArtifactDigest: "a", ManifestDigest: "m", BundleDigest: "b"}
	p := proposal(t, r, "mark", "applied", key, now)
	if e := p.Verify(map[string]ed25519.PublicKey{"operator": pub}, now); e != nil {
		t.Fatal(e)
	}
	x := p
	x.Reason += " tampered"
	if x.Verify(map[string]ed25519.PublicKey{"operator": pub}, now) == nil {
		t.Fatal("tamper accepted")
	}
	if p.Verify(map[string]ed25519.PublicKey{"operator": pub}, p.ExpiresAt) == nil {
		t.Fatal("expired accepted")
	}
	x = p
	x.Reason = "short"
	if x.Sign("operator", key) == nil {
		t.Fatal("short reason accepted")
	}
}
func TestLiveMarkRemoveStaleAuditAtomicity(t *testing.T) {
	url := os.Getenv("AUTOSQL_REPAIR_TEST_DSN")
	if url == "" {
		t.Skip("repair PostgreSQL URL unset")
	}
	ctx := context.Background()
	schema := "autosql_repair_" + strings.ToLower(strings.ReplaceAll(time.Now().Format("150405.000000"), ".", ""))
	store, e := revision.Open(revision.Config{URL: url, Schema: schema})
	if e != nil {
		t.Fatal(e)
	}
	if e = store.Init(ctx); e != nil {
		t.Fatal(e)
	}
	now := time.Now().UTC()
	base := revision.Revision{Version: "1.0.0", Description: "broken", Kind: "migration", FileName: "V1__broken.sql", FileDigest: "sha256:file", ManifestDigest: "sha256:manifest", ManifestGeneration: "generation", ArtifactDigest: "sha256:artifact", PlanDigest: "sha256:plan", ChecksDigest: "sha256:checks", BundleDigest: "sha256:bundle", State: "partial", Attempt: 1, Operator: "executor", StartedAt: now, UpdatedAt: now}
	if e = store.Insert(ctx, base); e != nil {
		t.Fatal(e)
	}
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	audit := &auditLog{}
	svc := Service{Store: store, Audit: audit, Keys: map[string]ed25519.PublicKey{"operator": pub}, Now: func() time.Time { return now }, LockIdentity: "repair/" + schema}
	mark := proposal(t, base, "mark", "applied", key, now)
	if e = svc.Apply(ctx, mark); e != nil {
		t.Fatal(e)
	}
	if len(audit.records) != 2 || audit.records[0].Type != "repair_requested" || audit.records[1].Type != "repair_applied" {
		t.Fatalf("audit=%+v", audit.records)
	}
	if e = svc.Apply(ctx, mark); !errors.Is(e, ErrRefused) {
		t.Fatalf("stale retry=%v", e)
	}
	s, _ := store.OpenSession(ctx)
	rows, _ := s.Revisions(ctx)
	s.Close(ctx)
	if len(rows) != 1 || rows[0].State != "applied" {
		t.Fatalf("rows=%+v", rows)
	}
	failing := svc
	failing.Audit = &auditLog{failAt: 2}
	remove := proposal(t, rows[0], "remove", "removed", key, now)
	if e = failing.Apply(ctx, remove); e == nil {
		t.Fatal("audit failure accepted")
	}
	s, _ = store.OpenSession(ctx)
	unchanged, _ := s.Revisions(ctx)
	s.Close(ctx)
	if len(unchanged) != 1 {
		t.Fatal("audit failure mutated")
	}
	if e = svc.Apply(ctx, remove); e != nil {
		t.Fatal(e)
	}
	s, _ = store.OpenSession(ctx)
	final, _ := s.Revisions(ctx)
	s.Close(ctx)
	if len(final) != 2 || final[1].Kind != "reversal" || final[1].ReversalOf != "1.0.0" {
		t.Fatalf("tombstone=%+v", final)
	}
}
