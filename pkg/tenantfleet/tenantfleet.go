// Package tenantfleet provides isolated, resumable rollouts for database-per-
// tenant deployments discovered from cloud or application metadata.
package tenantfleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"
)

var (
	ErrInvalid   = errors.New("invalid tenant fleet configuration")
	ErrDuplicate = errors.New("duplicate tenant identity")
	ErrLimit     = errors.New("tenant fleet limit exceeded")
)

type Tenant struct {
	ID            string            `json:"tenant_id"`
	TargetID      string            `json:"target_id"`
	Environment   string            `json:"environment"`
	ConnectionRef string            `json:"connection_ref"`
	PolicyDigest  string            `json:"policy_digest"`
	Active        bool              `json:"active"`
	Labels        map[string]string `json:"labels,omitempty"`
}

func (t Tenant) Validate() error {
	if t.ID == "" || t.TargetID == "" || t.Environment == "" || t.ConnectionRef == "" || t.PolicyDigest == "" {
		return ErrInvalid
	}
	return nil
}

type Discovery interface {
	Discover(context.Context) ([]Tenant, error)
}
type StaticDiscovery struct{ Tenants []Tenant }

func (s StaticDiscovery) Discover(context.Context) ([]Tenant, error) {
	return append([]Tenant(nil), s.Tenants...), nil
}

type Snapshot struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Tenants   []Tenant  `json:"tenants"`
	Retired   []Tenant  `json:"retired,omitempty"`
}

func Discover(ctx context.Context, providers []Discovery, max int) (Snapshot, error) {
	if len(providers) == 0 || max <= 0 {
		return Snapshot{}, ErrInvalid
	}
	var all []Tenant
	for _, p := range providers {
		if p == nil {
			return Snapshot{}, ErrInvalid
		}
		ts, e := p.Discover(ctx)
		if e != nil {
			return Snapshot{}, e
		}
		all = append(all, ts...)
	}
	seen := map[string]bool{}
	out := make([]Tenant, 0, len(all))
	retired := make([]Tenant, 0)
	for _, t := range all {
		if e := t.Validate(); e != nil {
			return Snapshot{}, e
		}
		if !t.Active {
			retired = append(retired, t)
			continue
		}
		if seen[t.ID] {
			return Snapshot{}, fmt.Errorf("%w: %s", ErrDuplicate, t.ID)
		}
		seen[t.ID] = true
		out = append(out, t)
	}
	if len(out) > max {
		return Snapshot{}, ErrLimit
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	sort.Slice(retired, func(i, j int) bool { return retired[i].ID < retired[j].ID })
	b, _ := json.Marshal(out)
	h := sha256.Sum256(b)
	return Snapshot{ID: "sha256:" + hex.EncodeToString(h[:]), CreatedAt: time.Now().UTC(), Tenants: out, Retired: retired}, nil
}

type PolicyOverride struct {
	TenantID        string `json:"tenant_id"`
	PolicyDigest    string `json:"policy_digest"`
	RequireApproval bool   `json:"require_approval"`
	Approved        bool   `json:"approved"`
}
type RolloutConfig struct {
	MaxConcurrent int
	CanaryCount   int
	Overrides     map[string]PolicyOverride
	Resume        map[string]Result
}
type Executor interface {
	Apply(context.Context, Tenant, PolicyOverride) error
}
type State string

const (
	StatePending State = "pending"
	StateRunning State = "running"
	StatePassed  State = "passed"
	StateFailed  State = "failed"
	StateSkipped State = "skipped"
)

type Result struct {
	TenantID         string    `json:"tenant_id"`
	TargetID         string    `json:"target_id"`
	PolicyDigest     string    `json:"policy_digest"`
	ApprovalRequired bool      `json:"approval_required"`
	ApprovalGranted  bool      `json:"approval_granted"`
	State            State     `json:"state"`
	Error            string    `json:"error,omitempty"`
	At               time.Time `json:"at"`
}
type Report struct {
	SnapshotID string   `json:"snapshot_id"`
	Results    []Result `json:"results"`
	Status     string   `json:"status"`
}

func Execute(ctx context.Context, s Snapshot, c RolloutConfig, x Executor) (Report, error) {
	if x == nil || c.MaxConcurrent <= 0 || c.CanaryCount < 0 || c.CanaryCount > len(s.Tenants) {
		return Report{}, ErrInvalid
	}
	ordered := append([]Tenant(nil), s.Tenants...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	results := make([]Result, 0, len(ordered))
	for i, t := range ordered {
		if old, ok := c.Resume[t.ID]; ok && (old.State == StatePassed || old.State == StateSkipped) {
			results = append(results, old)
			continue
		}
		if c.CanaryCount > 0 && i >= c.CanaryCount && hasFailed(results) {
			results = append(results, Result{TenantID: t.ID, TargetID: t.TargetID, State: StateSkipped, At: time.Now().UTC()})
			continue
		}
		if err := ctx.Err(); err != nil {
			return Report{}, err
		}
		override := c.Overrides[t.ID]
		r := Result{TenantID: t.ID, TargetID: t.TargetID, PolicyDigest: t.PolicyDigest, ApprovalRequired: override.RequireApproval, ApprovalGranted: !override.RequireApproval || override.Approved, At: time.Now().UTC()}
		var e error
		if override.RequireApproval && !override.Approved {
			e = errors.New("tenant approval required")
		} else {
			e = x.Apply(ctx, t, override)
		}
		if e != nil {
			r.State = StateFailed
			r.Error = "tenant apply failed"
		} else {
			r.State = StatePassed
		}
		results = append(results, r)
	}
	status := "passed"
	for _, r := range results {
		if r.State == StateFailed {
			status = "failed"
			break
		}
		if r.State == StateSkipped {
			status = "partial"
		}
	}
	return Report{SnapshotID: s.ID, Results: results, Status: status}, nil
}
func hasFailed(rs []Result) bool {
	for _, r := range rs {
		if r.State == StateFailed {
			return true
		}
	}
	return false
}

type MemoryExecutor struct {
	mu    sync.Mutex
	Calls []string
	Fail  map[string]bool
}

func (m *MemoryExecutor) Apply(_ context.Context, t Tenant, _ PolicyOverride) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.Calls = append(m.Calls, t.ID)
	if m.Fail[t.ID] {
		return errors.New("failed")
	}
	return nil
}
