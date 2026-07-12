package repair

import (
	"autosql/pkg/artifact"
	"autosql/pkg/migrate/revision"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync"
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
	svc := Service{Store: store, Audit: audit, Keys: map[string]ed25519.PublicKey{"operator": pub}, Now: func() time.Time { return now }, LockIdentity: "repair/" + schema, Authorize: func(context.Context, Proposal, revision.Revision) error { return nil }}
	mark := proposal(t, base, "mark", "applied", key, now)
	if e = svc.Apply(ctx, mark); e != nil {
		t.Fatal(e)
	}
	if len(audit.records) != 2 || audit.records[0].Type != "repair_requested" || audit.records[1].Type != "repair_applied" {
		t.Fatalf("audit=%+v", audit.records)
	}
	if e = svc.Apply(ctx, mark); e != nil {
		t.Fatalf("idempotent retry=%v", e)
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
	committed, _ := s.Revisions(ctx)
	s.Close(ctx)
	if len(committed) != 2 || committed[1].Kind != "reversal" {
		t.Fatal("applied-audit failure lost committed tombstone")
	}
	if e = svc.Apply(ctx, remove); e != nil {
		t.Fatalf("outbox recovery: %v", e)
	}
	s, _ = store.OpenSession(ctx)
	final, _ := s.Revisions(ctx)
	s.Close(ctx)
	if len(final) != 2 || final[1].Kind != "reversal" || final[1].ReversalOf != "1.0.0" {
		t.Fatalf("tombstone=%+v", final)
	}
	second := base
	second.Version = "2.0.0"
	second.State = "partial"
	if e = store.Insert(ctx, second); e != nil {
		t.Fatal(e)
	}
	race := proposal(t, second, "reconcile", "applied", key, now)
	start := make(chan struct{})
	results := make(chan error, 2)
	var wg sync.WaitGroup
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); <-start; results <- svc.Apply(ctx, race) }()
	}
	close(start)
	wg.Wait()
	close(results)
	wins, refused := 0, 0
	for er := range results {
		if er == nil {
			wins++
		} else if errors.Is(er, ErrRefused) {
			refused++
		}
	}
	if wins != 1 || refused != 1 {
		t.Fatalf("concurrent CAS wins=%d refused=%d", wins, refused)
	}
	stale := race
	stale.Reason = "different stale repair after incident review"
	stale.Digest = ""
	stale.Signature = artifact.Signature{}
	if e = stale.Sign("operator", key); e != nil {
		t.Fatal(e)
	}
	if e = svc.Apply(ctx, stale); !errors.Is(e, ErrRefused) {
		t.Fatalf("distinct stale proposal=%v", e)
	}
	denied := svc
	denied.Authorize = func(context.Context, Proposal, revision.Revision) error { return errors.New("policy denied") }
	third := base
	third.Version = "3.0.0"
	third.State = "partial"
	if e = store.Insert(ctx, third); e != nil {
		t.Fatal(e)
	}
	if e = denied.Apply(ctx, proposal(t, third, "mark", "applied", key, now)); !errors.Is(e, ErrRefused) {
		t.Fatalf("authorization=%v", e)
	}
	s, _ = store.OpenSession(ctx)
	afterDenied, _ := s.Revisions(ctx)
	s.Close(ctx)
	if afterDenied[len(afterDenied)-1].State != "partial" {
		t.Fatal("authorization denial mutated")
	}
	for i, name := range []string{"after_requested", "before_commit", "after_commit", "after_applied"} {
		t.Run(name, func(t *testing.T) {
			row := base
			row.Version = fmt.Sprintf("%d.0.0", 10+i)
			row.State = "partial"
			if er := store.Insert(ctx, row); er != nil {
				t.Fatal(er)
			}
			p := proposal(t, row, "reconcile", "applied", key, now)
			fault := svc
			boom := func() error { return errors.New("injected crash") }
			switch name {
			case "after_requested":
				fault.Hooks.AfterRequested = boom
			case "before_commit":
				fault.Hooks.BeforeCommit = boom
			case "after_commit":
				fault.Hooks.AfterCommit = boom
			case "after_applied":
				fault.Hooks.AfterApplied = boom
			}
			if er := fault.Apply(ctx, p); er == nil {
				t.Fatal("fault did not fire")
			}
			session, _ := store.OpenSession(ctx)
			current, _ := session.Revisions(ctx)
			pending, _ := session.PendingOutbox(ctx)
			session.Close(ctx)
			state := ""
			for _, x := range current {
				if x.Version == row.Version {
					state = x.State
				}
			}
			committed := name == "after_commit" || name == "after_applied"
			if committed && state != "applied" || !committed && state != "partial" {
				t.Fatalf("state=%s", state)
			}
			if name == "after_commit" && len(pending) == 0 {
				t.Fatal("committed repair missing outbox")
			}
			clean := svc
			if er := clean.Apply(ctx, p); er != nil {
				t.Fatalf("recovery=%v", er)
			}
			session, _ = store.OpenSession(ctx)
			finalRows, _ := session.Revisions(ctx)
			remaining, _ := session.PendingOutbox(ctx)
			session.Close(ctx)
			for _, x := range finalRows {
				if x.Version == row.Version && x.State != "applied" {
					t.Fatalf("recovery state=%s", x.State)
				}
			}
			if len(remaining) != 0 {
				t.Fatalf("outbox not drained: %d", len(remaining))
			}
		})
	}
}
