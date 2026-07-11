package approval

import (
	"context"
	"errors"
	"testing"
	"time"
)

func base(now time.Time) (Gate, Request, *MemoryLog) {
	log := &MemoryLog{}
	g := Gate{Now: func() time.Time { return now }, Audit: log, Policy: Policy{Environments: map[string]EnvironmentPolicy{
		"prod": {Allowed: true, Requirements: []Requirement{{MinimumRisk: RiskHigh, ApproverCount: 2, Roles: []string{"dba"}, ForbidSelfApproval: true}}},
	}}}
	r := Request{Plan: Plan{Digest: "sha256:abc", Environment: "prod", Author: "author", Risk: RiskHigh}, RequestedBy: "deployer"}
	return g, r, log
}

func TestGuardedApplyNeverMutatesOnGateFailure(t *testing.T) {
	now := time.Unix(100, 0)
	g, r, log := base(now)
	called := false
	err := g.GuardedApply(context.Background(), r, func(context.Context) error { called = true; return nil })
	if !errors.Is(err, ErrDenied) {
		t.Fatalf("got %v", err)
	}
	if called {
		t.Fatal("mutation called after denial")
	}
	if es := log.Events(); len(es) != 1 || es[0].Allowed {
		t.Fatalf("events=%+v", es)
	}
}

func TestDigestEnvironmentRolesExpiryAndSeparation(t *testing.T) {
	now := time.Unix(100, 0)
	g, r, _ := base(now)
	r.Approvals = []Approval{
		{PlanDigest: r.Plan.Digest, Environment: "prod", Approver: "author", Roles: []string{"dba"}, ApprovedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)},
		{PlanDigest: "other", Environment: "prod", Approver: "a", Roles: []string{"dba"}, ApprovedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)},
		{PlanDigest: r.Plan.Digest, Environment: "stage", Approver: "b", Roles: []string{"dba"}, ApprovedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)},
		{PlanDigest: r.Plan.Digest, Environment: "prod", Approver: "c", Roles: []string{"dba"}, ApprovedAt: now.Add(-time.Hour), ExpiresAt: now},
	}
	if _, err := g.Evaluate(r); !errors.Is(err, ErrDenied) {
		t.Fatalf("got %v", err)
	}
	r.Approvals = []Approval{
		{PlanDigest: r.Plan.Digest, Environment: "prod", Approver: "alice", Roles: []string{"dba"}, ApprovedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)},
		{PlanDigest: r.Plan.Digest, Environment: "prod", Approver: "bob", Roles: []string{"dba", "security"}, ApprovedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)},
	}
	d, err := g.Evaluate(r)
	if err != nil || !d.Allowed || len(d.Approvers) != 2 {
		t.Fatalf("decision=%+v err=%v", d, err)
	}
}

func TestEmergencyOverrideRequiresIdentityReasonAndAudit(t *testing.T) {
	now := time.Unix(100, 0)
	g, r, log := base(now)
	called := false
	r.Override = &EmergencyOverride{Identity: "oncall", Reason: "restore customer access"}
	if err := g.GuardedApply(context.Background(), r, func(context.Context) error { called = true; return nil }); err != nil {
		t.Fatal(err)
	}
	if !called {
		t.Fatal("mutation not called")
	}
	es := log.Events()
	if len(es) != 1 || !es[0].Emergency || es[0].Actor != "oncall" || es[0].Reason == "" {
		t.Fatalf("events=%+v", es)
	}
	r.Override = &EmergencyOverride{Identity: "", Reason: "because"}
	called = false
	if err := g.GuardedApply(context.Background(), r, func(context.Context) error { called = true; return nil }); !errors.Is(err, ErrDenied) {
		t.Fatalf("got %v", err)
	}
	if called {
		t.Fatal("invalid override mutated")
	}
}

type failingLog struct{}

func (failingLog) Append(context.Context, Event) error { return errors.New("disk full") }
func TestAuditFailurePreventsMutation(t *testing.T) {
	now := time.Unix(100, 0)
	g, r, _ := base(now)
	g.Audit = failingLog{}
	r.Override = &EmergencyOverride{Identity: "oncall", Reason: "incident"}
	called := false
	if err := g.GuardedApply(context.Background(), r, func(context.Context) error { called = true; return nil }); !errors.Is(err, ErrAudit) {
		t.Fatalf("got %v", err)
	}
	if called {
		t.Fatal("mutation called before durable audit")
	}
}

func TestMemoryLogReturnsImmutableSnapshot(t *testing.T) {
	l := &MemoryLog{}
	_ = l.Append(context.Background(), Event{Reason: "original"})
	got := l.Events()
	got[0].Reason = "changed"
	if l.Events()[0].Reason != "original" {
		t.Fatal("audit history was mutable")
	}
}

func TestRiskThresholdAndConfigurableSelfApproval(t *testing.T) {
	now := time.Unix(100, 0)
	g, r, _ := base(now)
	r.Plan.Risk = RiskLow
	if d, err := g.Evaluate(r); err != nil || !d.Allowed {
		t.Fatalf("low-risk decision=%+v err=%v", d, err)
	}
	r.Plan.Risk = RiskHigh
	ep := g.Policy.Environments["prod"]
	ep.Requirements[0].ApproverCount = 1
	ep.Requirements[0].ForbidSelfApproval = false
	g.Policy.Environments["prod"] = ep
	r.Approvals = []Approval{{PlanDigest: r.Plan.Digest, Environment: "prod", Approver: r.Plan.Author, Roles: []string{"dba"}, ApprovedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}}
	if d, err := g.Evaluate(r); err != nil || !d.Allowed {
		t.Fatalf("configured self-approval decision=%+v err=%v", d, err)
	}
}

func TestExpiredPlanDenied(t *testing.T) {
	now := time.Unix(100, 0)
	g, r, _ := base(now)
	r.Plan.ExpiresAt = now
	r.Override = &EmergencyOverride{Identity: "oncall", Reason: "incident"}
	if _, err := g.Evaluate(r); !errors.Is(err, ErrDenied) {
		t.Fatalf("got %v", err)
	}
}
