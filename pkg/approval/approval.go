// Package approval enforces authorization policy immediately before a
// migration is applied. Approval records are bound to a plan digest and target
// environment; GuardedApply never invokes mutation when authorization fails.
package approval

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"
)

type Risk int

const (
	RiskLow Risk = iota
	RiskMedium
	RiskHigh
	RiskCritical
)

type Plan struct {
	Digest, Environment, Author string
	Risk                        Risk
	ExpiresAt                   time.Time
}

type Approval struct {
	PlanDigest, Environment, Approver string
	Roles                             []string
	ApprovedAt, ExpiresAt             time.Time
}

type Requirement struct {
	MinimumRisk        Risk
	ApproverCount      int
	Roles              []string
	ForbidSelfApproval bool
}

type EnvironmentPolicy struct {
	Allowed      bool
	Requirements []Requirement
}

type Policy struct{ Environments map[string]EnvironmentPolicy }

type EmergencyOverride struct{ Identity, Reason string }

type Request struct {
	Plan        Plan
	Approvals   []Approval
	Override    *EmergencyOverride
	RequestedBy string
}

var (
	ErrDenied = errors.New("apply approval denied")
	ErrAudit  = errors.New("approval audit failed")
)

type Decision struct {
	Allowed   bool
	Emergency bool
	Approvers []string
	Reason    string
}

type Event struct {
	At                                     time.Time
	Type                                   string
	PlanDigest, Environment, Actor, Reason string
	Emergency, Allowed                     bool
}

// AuditLog is append-only by construction: it exposes no update/delete method.
type AuditLog interface {
	Append(context.Context, Event) error
}

type MemoryLog struct {
	mu     sync.RWMutex
	events []Event
}

func (l *MemoryLog) Append(_ context.Context, e Event) error {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
	return nil
}
func (l *MemoryLog) Events() []Event {
	l.mu.RLock()
	defer l.mu.RUnlock()
	out := make([]Event, len(l.events))
	copy(out, l.events)
	return out
}

type Gate struct {
	Policy Policy
	Audit  AuditLog
	Now    func() time.Time
}

func (g Gate) Evaluate(req Request) (Decision, error) {
	now := g.Now
	if now == nil {
		now = time.Now
	}
	instant := now()
	if strings.TrimSpace(req.Plan.Digest) == "" || strings.TrimSpace(req.Plan.Environment) == "" {
		return Decision{Reason: "plan digest and environment are required"}, fmt.Errorf("%w: plan identity is incomplete", ErrDenied)
	}
	ep, ok := g.Policy.Environments[req.Plan.Environment]
	if !ok || !ep.Allowed {
		return Decision{Reason: "environment is not allowed"}, fmt.Errorf("%w: environment %q is not allowed", ErrDenied, req.Plan.Environment)
	}
	if !req.Plan.ExpiresAt.IsZero() && !req.Plan.ExpiresAt.After(instant) {
		return Decision{Reason: "plan has expired"}, fmt.Errorf("%w: plan has expired", ErrDenied)
	}
	if req.Override != nil {
		if strings.TrimSpace(req.Override.Identity) == "" || strings.TrimSpace(req.Override.Reason) == "" {
			return Decision{Reason: "emergency identity and reason are required"}, fmt.Errorf("%w: invalid emergency override", ErrDenied)
		}
		return Decision{Allowed: true, Emergency: true, Reason: req.Override.Reason}, nil
	}
	valid := map[string]Approval{}
	for _, a := range req.Approvals {
		if a.PlanDigest != req.Plan.Digest || a.Environment != req.Plan.Environment || a.ApprovedAt.After(instant) || !a.ExpiresAt.After(instant) {
			continue
		}
		if a.Approver == "" {
			continue
		}
		valid[a.Approver] = a
	}
	eligible := map[string]bool{}
	for _, r := range ep.Requirements {
		if req.Plan.Risk < r.MinimumRisk {
			continue
		}
		count := 0
		for _, a := range valid {
			if r.ForbidSelfApproval && (a.Approver == req.Plan.Author || a.Approver == req.RequestedBy) {
				continue
			}
			if hasAllRoles(a.Roles, r.Roles) {
				count++
				eligible[a.Approver] = true
			}
		}
		if count < r.ApproverCount {
			return Decision{Reason: fmt.Sprintf("requires %d eligible approvers with roles %v", r.ApproverCount, r.Roles)}, fmt.Errorf("%w: insufficient eligible approvals", ErrDenied)
		}
	}
	names := make([]string, 0, len(eligible))
	for name := range eligible {
		names = append(names, name)
	}
	sort.Strings(names)
	return Decision{Allowed: true, Approvers: names}, nil
}

func hasAllRoles(got, want []string) bool {
	for _, w := range want {
		found := false
		for _, g := range got {
			if g == w {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	return true
}

// GuardedApply audits the final decision and calls mutate only after every gate
// has passed and the allow event was durably appended. Audit failure is denial.
func (g Gate) GuardedApply(ctx context.Context, req Request, mutate func(context.Context) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	decision, err := g.Evaluate(req)
	now := g.Now
	if now == nil {
		now = time.Now
	}
	actor := req.RequestedBy
	if decision.Emergency {
		actor = req.Override.Identity
	}
	e := Event{At: now(), Type: "apply_denied", PlanDigest: req.Plan.Digest, Environment: req.Plan.Environment, Actor: actor, Reason: decision.Reason, Emergency: decision.Emergency, Allowed: decision.Allowed}
	if err == nil {
		e.Type = "apply_authorized"
	}
	if g.Audit == nil {
		return fmt.Errorf("%w: audit log is required", ErrAudit)
	}
	if auditErr := g.Audit.Append(ctx, e); auditErr != nil {
		return fmt.Errorf("%w: %v", ErrAudit, auditErr)
	}
	if err != nil {
		return err
	}
	if mutate == nil {
		return errors.New("apply mutation is nil")
	}
	return mutate(ctx)
}
