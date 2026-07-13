// Package monitor discovers approved database targets and runs bounded,
// read-only schema/security observations with resumable health state.
package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"sort"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalid   = errors.New("invalid monitoring configuration")
	ErrDiscovery = errors.New("target discovery failed")
	ErrLimit     = errors.New("monitoring limit exceeded")
	ErrRevoked   = errors.New("target is revoked")
	ErrStale     = errors.New("target observation is stale")
	ErrSensitive = errors.New("monitoring state contains sensitive data")
)

type Provider string

const (
	AWS    Provider = "aws"
	GCP    Provider = "gcp"
	Azure  Provider = "azure"
	Static Provider = "static"
)

type Target struct {
	Provider      Provider          `json:"provider"`
	ID            string            `json:"id"`
	Project       string            `json:"project,omitempty"`
	Environment   string            `json:"environment"`
	Region        string            `json:"region,omitempty"`
	Labels        map[string]string `json:"labels,omitempty"`
	ConnectionRef string            `json:"connection_ref"`
	Revoked       bool              `json:"revoked,omitempty"`
}

func (t Target) Validate() error {
	if t.Provider == "" || t.ID == "" || t.Environment == "" || t.ConnectionRef == "" {
		return ErrInvalid
	}
	if strings.ContainsAny(t.ID+t.Environment+t.ConnectionRef, "\r\n") {
		return ErrInvalid
	}
	if strings.Contains(t.ConnectionRef, "://") {
		u, e := url.Parse(t.ConnectionRef)
		if e != nil || u.User != nil {
			return ErrSensitive
		}
	}
	for k, v := range t.Labels {
		if k == "" || strings.ContainsAny(k+v, "\r\n") {
			return ErrInvalid
		}
	}
	return nil
}

type Filter struct {
	Provider    Provider          `json:"provider,omitempty"`
	Environment string            `json:"environment,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	IDPattern   string            `json:"id_pattern,omitempty"`
}

func (f Filter) Validate() error {
	if f.Environment != "" {
		if _, e := path.Match(f.Environment, "x"); e != nil {
			return ErrInvalid
		}
	}
	if f.IDPattern != "" {
		if _, e := path.Match(f.IDPattern, "x"); e != nil {
			return ErrInvalid
		}
	}
	for k, v := range f.Labels {
		if k == "" || v == "" {
			return ErrInvalid
		}
		if _, e := path.Match(v, "x"); e != nil {
			return ErrInvalid
		}
	}
	return nil
}
func (f Filter) Match(t Target) bool {
	if f.Provider != "" && f.Provider != t.Provider {
		return false
	}
	if f.Environment != "" {
		ok, _ := path.Match(f.Environment, t.Environment)
		if !ok {
			return false
		}
	}
	if f.IDPattern != "" {
		ok, _ := path.Match(f.IDPattern, t.ID)
		if !ok {
			return false
		}
	}
	for k, v := range f.Labels {
		got, ok := t.Labels[k]
		m, _ := path.Match(v, got)
		if !ok || !m {
			return false
		}
	}
	return true
}

type Discovery interface {
	Discover(context.Context) ([]Target, error)
}
type StaticDiscovery struct{ Targets []Target }

func (s StaticDiscovery) Discover(context.Context) ([]Target, error) {
	return append([]Target(nil), s.Targets...), nil
}

type DiscoveryConfig struct {
	Providers  []Discovery   `json:"-"`
	Filter     Filter        `json:"filter"`
	MaxTargets int           `json:"max_targets"`
	Timeout    time.Duration `json:"timeout"`
}
type Snapshot struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Targets   []Target  `json:"targets"`
}

func DiscoverTargets(ctx context.Context, c DiscoveryConfig) (Snapshot, error) {
	if len(c.Providers) == 0 || c.MaxTargets <= 0 || c.Timeout <= 0 || c.Filter.Validate() != nil {
		return Snapshot{}, ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, c.Timeout)
	defer cancel()
	var all []Target
	for _, p := range c.Providers {
		if p == nil {
			return Snapshot{}, ErrInvalid
		}
		ts, e := p.Discover(ctx)
		if e != nil {
			return Snapshot{}, fmt.Errorf("%w: %v", ErrDiscovery, e)
		}
		all = append(all, ts...)
	}
	seen := map[string]bool{}
	out := make([]Target, 0, len(all))
	for _, t := range all {
		if e := t.Validate(); e != nil {
			return Snapshot{}, e
		}
		if !c.Filter.Match(t) {
			continue
		}
		if seen[t.ID] {
			return Snapshot{}, fmt.Errorf("%w: duplicate %s", ErrDiscovery, t.ID)
		}
		seen[t.ID] = true
		t.ConnectionRef = maskRef(t.ConnectionRef)
		out = append(out, t)
	}
	if len(out) > c.MaxTargets {
		return Snapshot{}, fmt.Errorf("%w: %d targets exceeds %d", ErrLimit, len(out), c.MaxTargets)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].ID < out[j].ID })
	b, _ := json.Marshal(out)
	h := sha256.Sum256(b)
	return Snapshot{ID: "sha256:" + hex.EncodeToString(h[:]), CreatedAt: time.Now().UTC(), Targets: out}, nil
}
func maskRef(ref string) string {
	u, e := url.Parse(ref)
	if e == nil && u.Scheme != "" {
		u.User = nil
		return u.String()
	}
	return ref
}

type FindingSeverity string

const (
	Info     FindingSeverity = "info"
	Warning  FindingSeverity = "warning"
	Critical FindingSeverity = "critical"
)

type Finding struct {
	Code        string          `json:"code"`
	Severity    FindingSeverity `json:"severity"`
	Message     string          `json:"message"`
	Remediation string          `json:"remediation"`
}
type Inspection struct {
	TargetID         string    `json:"target_id"`
	ObservedDigest   string    `json:"observed_digest"`
	ExpectedDigest   string    `json:"expected_digest"`
	Findings         []Finding `json:"findings,omitempty"`
	ChangePlanDigest string    `json:"change_plan_digest,omitempty"`
	CheckedAt        time.Time `json:"checked_at"`
}
type Inspector interface {
	Inspect(context.Context, Target, []string) (Inspection, error)
}
type Health string

const (
	Healthy Health = "healthy"
	Drifted Health = "drifted"
	Failed  Health = "failed"
	Stale   Health = "stale"
	Revoked Health = "revoked"
)

type Checkpoint struct {
	TargetID    string      `json:"target_id"`
	Health      Health      `json:"health"`
	LastChecked time.Time   `json:"last_checked"`
	LastSuccess time.Time   `json:"last_success,omitempty"`
	Error       string      `json:"error,omitempty"`
	Inspection  *Inspection `json:"inspection,omitempty"`
}
type Proposal struct {
	ID               string    `json:"id"`
	TargetID         string    `json:"target_id"`
	SnapshotID       string    `json:"snapshot_id"`
	ChangePlanDigest string    `json:"change_plan_digest"`
	Findings         []Finding `json:"findings"`
	CreatedAt        time.Time `json:"created_at"`
	RequiresApproval bool      `json:"requires_approval"`
}

type AlertSink interface {
	Send(context.Context, Alert) error
}
type Alert struct {
	Type       string    `json:"type"`
	TargetID   string    `json:"target_id"`
	Health     Health    `json:"health"`
	ProposalID string    `json:"proposal_id,omitempty"`
	Message    string    `json:"message,omitempty"`
	At         time.Time `json:"at"`
}
type Webhook func(context.Context, Alert) error

func (w Webhook) Send(ctx context.Context, a Alert) error {
	if w == nil {
		return ErrInvalid
	}
	return w(ctx, a)
}

type Monitor struct {
	mu          sync.Mutex
	checkpoints map[string]Checkpoint
	proposals   map[string]Proposal
	Now         func() time.Time
}

func New() *Monitor {
	return &Monitor{checkpoints: map[string]Checkpoint{}, proposals: map[string]Proposal{}, Now: time.Now}
}
func (m *Monitor) RunOnce(ctx context.Context, s Snapshot, inspector Inspector, scope []string, maxConcurrent int, sink AlertSink) ([]Checkpoint, error) {
	if m == nil || inspector == nil || s.ID == "" || maxConcurrent <= 0 {
		return nil, ErrInvalid
	}
	if len(s.Targets) == 0 {
		return nil, nil
	}
	sem := make(chan struct{}, maxConcurrent)
	var wg sync.WaitGroup
	var mu sync.Mutex
	out := make([]Checkpoint, 0, len(s.Targets))
	for _, target := range s.Targets {
		t := target
		wg.Add(1)
		go func() {
			defer wg.Done()
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return
			}
			defer func() { <-sem }()
			now := m.Now().UTC()
			if t.Revoked {
				m.record(Checkpoint{TargetID: t.ID, Health: Revoked, LastChecked: now})
				if sink != nil {
					_ = sink.Send(ctx, Alert{Type: "target_revoked", TargetID: t.ID, Health: Revoked, Message: "target is revoked", At: now})
				}
				return
			}
			inspection, e := inspector.Inspect(ctx, t, scope)
			cp := Checkpoint{TargetID: t.ID, LastChecked: now, Inspection: &inspection}
			if e != nil {
				cp.Health = Failed
				cp.Error = "inspection failed"
			} else if len(inspection.Findings) > 0 {
				cp.Health = Drifted
			} else {
				cp.Health = Healthy
				cp.LastSuccess = now
			}
			m.mu.Lock()
			m.checkpoints[t.ID] = cp
			m.mu.Unlock()
			mu.Lock()
			out = append(out, cp)
			mu.Unlock()
			if e != nil && sink != nil {
				_ = sink.Send(ctx, Alert{Type: "monitor_failed", TargetID: t.ID, Health: Failed, Message: "target inspection failed", At: now})
			}
			if e == nil && len(inspection.Findings) > 0 {
				p := m.proposal(s.ID, t.ID, inspection)
				if sink != nil {
					_ = sink.Send(ctx, Alert{Type: "drift_detected", TargetID: t.ID, Health: Drifted, ProposalID: p.ID, Message: "managed drift detected", At: now})
				}
			}
		}()
	}
	wg.Wait()
	sort.Slice(out, func(i, j int) bool { return out[i].TargetID < out[j].TargetID })
	if err := ctx.Err(); err != nil {
		return out, err
	}
	return out, nil
}
func (m *Monitor) record(c Checkpoint) { m.mu.Lock(); m.checkpoints[c.TargetID] = c; m.mu.Unlock() }
func (m *Monitor) proposal(snapshot, target string, i Inspection) Proposal {
	raw, _ := json.Marshal(i)
	h := sha256.Sum256(raw)
	p := Proposal{ID: "proposal:" + hex.EncodeToString(h[:]), TargetID: target, SnapshotID: snapshot, ChangePlanDigest: i.ChangePlanDigest, Findings: append([]Finding(nil), i.Findings...), CreatedAt: m.Now().UTC(), RequiresApproval: true}
	m.mu.Lock()
	m.proposals[p.ID] = p
	m.mu.Unlock()
	return p
}
func (m *Monitor) Checkpoint(id string) (Checkpoint, bool) {
	m.mu.Lock()
	defer m.mu.Unlock()
	c, ok := m.checkpoints[id]
	return c, ok
}
func (m *Monitor) Proposals() []Proposal {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Proposal, 0, len(m.proposals))
	for _, p := range m.proposals {
		out = append(out, p)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out
}

func (m *Monitor) Stale(now time.Time, interval time.Duration) []Checkpoint {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []Checkpoint
	for id, c := range m.checkpoints {
		if c.Health == Healthy && now.Sub(c.LastSuccess) > interval {
			c.Health = Stale
			c.TargetID = id
			out = append(out, c)
			m.checkpoints[id] = c
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].TargetID < out[j].TargetID })
	return out
}
