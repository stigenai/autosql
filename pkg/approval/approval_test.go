package approval

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

type fakeAuthority struct{ identities map[string]Identity }

func (a fakeAuthority) ResolveActor(_ context.Context, id string) (Identity, error) {
	i, ok := a.identities[id]
	if !ok {
		return Identity{}, errors.New("unknown identity")
	}
	return i, nil
}
func (a fakeAuthority) VerifyApproval(_ context.Context, approval Approval) (VerifiedApproval, error) {
	i, ok := a.identities[approval.Approver]
	if !ok || approval.Proof != "signed:"+approval.Approver {
		return VerifiedApproval{}, errors.New("invalid approval proof")
	}
	return VerifiedApproval{Identity: i, PlanDigest: approval.PlanDigest, Environment: approval.Environment, ApprovedAt: approval.ApprovedAt, ExpiresAt: approval.ExpiresAt}, nil
}

type durableMemorySink struct {
	mu      sync.Mutex
	records []AuditRecord
	fail    bool
	cancel  context.CancelFunc
}

func (s *durableMemorySink) Tail(context.Context) (*AuditRecord, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.records) == 0 {
		return nil, nil
	}
	r := s.records[len(s.records)-1]
	return &r, nil
}
func (s *durableMemorySink) AppendDurable(_ context.Context, expected string, record AuditRecord) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.fail {
		return errors.New("persistence failure")
	}
	actual := ""
	if len(s.records) > 0 {
		actual = s.records[len(s.records)-1].Hash
	}
	if actual != expected {
		return ErrAuditConflict
	}
	s.records = append(s.records, record)
	if s.cancel != nil {
		s.cancel()
	}
	return nil
}

func base(now time.Time) (Gate, Request, *durableMemorySink) {
	sink := &durableMemorySink{}
	authority := fakeAuthority{identities: map[string]Identity{
		"author":   {ID: "author", Roles: []string{"developer"}},
		"deployer": {ID: "deployer", Roles: []string{"release"}, EmergencyAuthority: true},
		"alice":    {ID: "alice", Roles: []string{"dba"}},
		"bob":      {ID: "bob", Roles: []string{"dba", "security"}},
		"mallory":  {ID: "mallory"},
	}}
	g := Gate{
		Now:       func() time.Time { return now },
		Audit:     &Chain{Sink: sink},
		Authority: authority,
		Policy: Policy{Environments: map[string]EnvironmentPolicy{
			"prod": {Allowed: true, Requirements: []Requirement{{MinimumRisk: RiskHigh, ApproverCount: 2, Roles: []string{"dba"}}}},
		}},
	}
	r := Request{Plan: Plan{Digest: "sha256:abc", Environment: "prod", Author: "author", Risk: RiskHigh}, RequestedBy: "deployer"}
	return g, r, sink
}

func approval(req Request, who string, now time.Time) Approval {
	return Approval{PlanDigest: req.Plan.Digest, Environment: req.Plan.Environment, Approver: who, ApprovedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Proof: "signed:" + who}
}

func TestGuardedApplyWithTrustedApprovals(t *testing.T) {
	now := time.Unix(100, 0)
	g, req, sink := base(now)
	req.Approvals = []Approval{approval(req, "alice", now), approval(req, "bob", now)}
	called := false
	if err := g.GuardedApply(context.Background(), req, func(context.Context) error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !called || len(sink.records) != 1 || !sink.records[0].Event.Allowed || !verifyHash(sink.records[0]) {
		t.Fatalf("called=%v records=%+v", called, sink.records)
	}
}

func TestEveryGateFailurePreventsMutation(t *testing.T) {
	now := time.Unix(100, 0)
	tests := map[string]func(Gate, Request, *durableMemorySink) (Gate, Request){
		"missing author":    func(g Gate, r Request, _ *durableMemorySink) (Gate, Request) { r.Plan.Author = ""; return g, r },
		"missing requester": func(g Gate, r Request, _ *durableMemorySink) (Gate, Request) { r.RequestedBy = ""; return g, r },
		"author is requester": func(g Gate, r Request, _ *durableMemorySink) (Gate, Request) {
			r.RequestedBy = r.Plan.Author
			return g, r
		},
		"missing authority": func(g Gate, r Request, _ *durableMemorySink) (Gate, Request) { g.Authority = nil; return g, r },
		"forbidden environment": func(g Gate, r Request, _ *durableMemorySink) (Gate, Request) {
			r.Plan.Environment = "stage"
			return g, r
		},
		"expired plan":    func(g Gate, r Request, _ *durableMemorySink) (Gate, Request) { r.Plan.ExpiresAt = now; return g, r },
		"unmet approvals": func(g Gate, r Request, _ *durableMemorySink) (Gate, Request) { return g, r },
		"wrong approval binding": func(g Gate, r Request, _ *durableMemorySink) (Gate, Request) {
			a := approval(r, "alice", now)
			a.PlanDigest = "sha256:other"
			r.Approvals = []Approval{a, approval(r, "bob", now)}
			return g, r
		},
		"expired approval": func(g Gate, r Request, _ *durableMemorySink) (Gate, Request) {
			a := approval(r, "alice", now)
			a.ExpiresAt = now
			r.Approvals = []Approval{a, approval(r, "bob", now)}
			return g, r
		},
		"self approvals": func(g Gate, r Request, _ *durableMemorySink) (Gate, Request) {
			r.Approvals = []Approval{approval(r, r.Plan.Author, now), approval(r, r.RequestedBy, now)}
			return g, r
		},
		"self asserted proof": func(g Gate, r Request, _ *durableMemorySink) (Gate, Request) {
			a := approval(r, "alice", now)
			a.Proof = "forged"
			r.Approvals = []Approval{a, approval(r, "bob", now)}
			return g, r
		},
		"untrusted roles": func(g Gate, r Request, _ *durableMemorySink) (Gate, Request) {
			r.Approvals = []Approval{approval(r, "mallory", now), approval(r, "bob", now)}
			return g, r
		},
		"invalid emergency": func(g Gate, r Request, _ *durableMemorySink) (Gate, Request) {
			r.Override = &EmergencyOverride{Identity: "deployer"}
			return g, r
		},
		"impersonated emergency": func(g Gate, r Request, _ *durableMemorySink) (Gate, Request) {
			r.Override = &EmergencyOverride{Identity: "alice", Reason: "incident"}
			return g, r
		},
		"unauthorized emergency": func(g Gate, r Request, _ *durableMemorySink) (Gate, Request) {
			r.RequestedBy = "mallory"
			r.Override = &EmergencyOverride{Identity: "mallory", Reason: "incident"}
			return g, r
		},
		"nil audit": func(g Gate, r Request, _ *durableMemorySink) (Gate, Request) {
			r.Override = &EmergencyOverride{Identity: "deployer", Reason: "incident"}
			g.Audit = nil
			return g, r
		},
		"persistence failure": func(g Gate, r Request, s *durableMemorySink) (Gate, Request) {
			r.Override = &EmergencyOverride{Identity: "deployer", Reason: "incident"}
			s.fail = true
			return g, r
		},
		"tampered audit tail": func(g Gate, r Request, s *durableMemorySink) (Gate, Request) {
			r.Override = &EmergencyOverride{Identity: "deployer", Reason: "incident"}
			s.records = []AuditRecord{{Sequence: 1, Hash: "forged", Event: Event{Type: "old"}}}
			return g, r
		},
	}
	for name, setup := range tests {
		t.Run(name, func(t *testing.T) {
			g, req, sink := base(now)
			g, req = setup(g, req, sink)
			called := false
			if err := g.GuardedApply(context.Background(), req, func(context.Context) error { called = true; return nil }); err == nil {
				t.Fatal("expected failure")
			}
			if called {
				t.Fatal("mutation called after gate failure")
			}
		})
	}
}

func TestNilMutationAndCanceledContextNeverMutate(t *testing.T) {
	now := time.Unix(100, 0)
	g, req, _ := base(now)
	req.Override = &EmergencyOverride{Identity: "deployer", Reason: "incident"}
	if err := g.GuardedApply(context.Background(), req, nil); !errors.Is(err, ErrDenied) {
		t.Fatalf("got %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	called := false
	if err := g.GuardedApply(ctx, req, func(context.Context) error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
}

func TestCancellationAfterAuditAndExpiryAfterAuditPreventMutation(t *testing.T) {
	now := time.Unix(100, 0)
	g, req, sink := base(now)
	req.Override = &EmergencyOverride{Identity: "deployer", Reason: "incident"}
	ctx, cancel := context.WithCancel(context.Background())
	sink.cancel = cancel
	called := false
	if err := g.GuardedApply(ctx, req, func(context.Context) error { called = true; return nil }); !errors.Is(err, context.Canceled) || called {
		t.Fatalf("err=%v called=%v", err, called)
	}

	g, req, sink = base(now)
	req.Approvals = []Approval{approval(req, "alice", now), approval(req, "bob", now)}
	req.Plan.ExpiresAt = now.Add(time.Second)
	calls := 0
	g.Now = func() time.Time {
		calls++
		if calls >= 3 {
			return now.Add(2 * time.Second)
		}
		return now
	}
	called = false
	if err := g.GuardedApply(context.Background(), req, func(context.Context) error { called = true; return nil }); !errors.Is(err, ErrDenied) || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
	if len(sink.records) != 2 || sink.records[1].Event.Type != "apply_denied" {
		t.Fatalf("records=%+v", sink.records)
	}
}

func TestExactVerifiedApprovalExpiryIsRechecked(t *testing.T) {
	now := time.Unix(100, 0)
	g, req, sink := base(now)
	alice := approval(req, "alice", now)
	bob := approval(req, "bob", now)
	alice.ExpiresAt, bob.ExpiresAt = now.Add(time.Second), now.Add(time.Second)
	forgedLater := approval(req, "alice", now)
	forgedLater.Proof = "forged"
	req.Approvals = []Approval{alice, forgedLater, bob}
	calls := 0
	g.Now = func() time.Time {
		calls++
		if calls >= 3 {
			return now.Add(2 * time.Second)
		}
		return now
	}
	called := false
	if err := g.GuardedApply(context.Background(), req, func(context.Context) error { called = true; return nil }); !errors.Is(err, ErrDenied) || called {
		t.Fatalf("err=%v called=%v", err, called)
	}
	if len(sink.records) != 2 || sink.records[1].Event.Reason != "authorization expired before mutation" {
		t.Fatalf("records=%+v", sink.records)
	}
}

func TestEmergencyRequiresTrustedAuthorityAndAuditIdentityReason(t *testing.T) {
	now := time.Unix(100, 0)
	g, req, sink := base(now)
	req.Override = &EmergencyOverride{Identity: "deployer", Reason: "restore customer access"}
	if err := g.GuardedApply(context.Background(), req, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	e := sink.records[0].Event
	if !e.Emergency || e.Actor != "deployer" || e.Reason != req.Override.Reason {
		t.Fatalf("event=%+v", e)
	}
}

func TestFileSinkPersistsAndDetectsTampering(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	chain := &Chain{Sink: &FileSink{Path: path}}
	for i := 0; i < 2; i++ {
		if _, err := chain.AppendDurable(context.Background(), Event{At: time.Unix(int64(i+1), 0), Type: "decision", Reason: "original"}); err != nil {
			t.Fatal(err)
		}
	}
	tail, err := (&FileSink{Path: path}).Tail(context.Background())
	if err != nil || tail == nil || tail.Sequence != 2 || !verifyHash(*tail) {
		t.Fatalf("tail=%+v err=%v", tail, err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(strings.Replace(string(b), "original", "tampered", 1)), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (&FileSink{Path: path}).Tail(context.Background()); err == nil {
		t.Fatal("tampering was not detected")
	}
}
