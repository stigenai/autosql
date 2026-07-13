// Package drift monitors registered database targets without applying changes.
package drift

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"sort"
	"sync"
	"time"

	"autosql/pkg/schema"
)

var (
	ErrInvalidTarget = errors.New("invalid drift target")
	ErrUnbounded     = errors.New("drift inspection exceeds configured bound")
)

type Inspector interface {
	Inspect(context.Context, Target) (schema.Document, error)
}

type Target struct {
	ID              string
	ExpectedDigest  string
	Expected        schema.Document
	ReadOnly        bool
	MaxResources    int
	IgnoredPatterns []string
}

type Classification string

const (
	Managed Classification = "managed"
	Ignored Classification = "ignored"
)

type Change struct {
	ID             string           `json:"id"`
	Operation      schema.Operation `json:"operation"`
	ResourceID     string           `json:"resource_id"`
	Classification Classification   `json:"classification"`
}

type Incident struct {
	Key         string     `json:"key"`
	TargetID    string     `json:"target_id"`
	Fingerprint string     `json:"fingerprint"`
	FirstSeen   time.Time  `json:"first_seen"`
	LastSeen    time.Time  `json:"last_seen"`
	ResolvedAt  *time.Time `json:"resolved_at,omitempty"`
	Status      string     `json:"status"`
	Changes     []Change   `json:"changes"`
	Remediation []Change   `json:"remediation"`
}

type Monitor struct {
	mu        sync.Mutex
	incidents map[string]Incident
	Now       func() time.Time
}

func New() *Monitor { return &Monitor{incidents: map[string]Incident{}, Now: time.Now} }

func (m *Monitor) Check(ctx context.Context, inspector Inspector, target Target) (Incident, error) {
	if inspector == nil || target.ID == "" || target.Expected.Version == "" || !target.ReadOnly {
		return Incident{}, ErrInvalidTarget
	}
	if target.MaxResources <= 0 {
		return Incident{}, ErrInvalidTarget
	}
	actual, err := inspector.Inspect(ctx, target)
	if err != nil {
		return Incident{}, err
	}
	if len(actual.Graph.Resources) > target.MaxResources {
		return Incident{}, ErrUnbounded
	}
	// Diff from actual to expected so the returned changes are a remediation
	// plan, never an instruction to apply observed drift back to the target.
	changes, err := schema.Diff(actual, target.Expected, schema.DiffOptions{})
	if err != nil {
		return Incident{}, fmt.Errorf("drift diff: %w", err)
	}
	actual.Normalize()
	fingerprint, err := schema.SemanticFingerprint(actual)
	if err != nil {
		return Incident{}, err
	}
	// Include change bytes so an identical actual document under another
	// expected digest cannot accidentally reuse an incident.
	changeRaw, _ := json.Marshal(changes)
	key := target.ID + "\x00" + target.ExpectedDigest + "\x00" + string(changeRaw)
	if len(changes.Changes) == 0 {
		m.mu.Lock()
		for k, old := range m.incidents {
			if old.TargetID == target.ID && old.Status == "open" {
				now := m.Now().UTC()
				old.Status = "resolved"
				old.ResolvedAt = &now
				old.LastSeen = now
				m.incidents[k] = old
			}
		}
		m.mu.Unlock()
		return Incident{TargetID: target.ID, Fingerprint: fingerprint, Status: "in_sync", Remediation: nil}, nil
	}
	incKey := digest(key)
	now := m.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	if old, ok := m.incidents[incKey]; ok {
		old.LastSeen = now
		old.Status = "open"
		m.incidents[incKey] = old
		return cloneIncident(old), nil
	}
	inc := Incident{Key: incKey, TargetID: target.ID, Fingerprint: fingerprint, FirstSeen: now, LastSeen: now, Status: "open"}
	for _, c := range changes.Changes {
		classification := Managed
		if ignored(c, target.IgnoredPatterns) {
			classification = Ignored
		}
		x := Change{ID: c.ID, Operation: c.Operation, ResourceID: c.ResourceID, Classification: classification}
		inc.Changes = append(inc.Changes, x)
		if classification == Managed {
			inc.Remediation = append(inc.Remediation, x)
		}
	}
	m.incidents[incKey] = inc
	return cloneIncident(inc), nil
}

func ignored(c schema.Change, patterns []string) bool {
	for _, p := range patterns {
		if ok, _ := path.Match(p, c.ResourceID); ok {
			return true
		}
	}
	return false
}

func digest(s string) string {
	b := sha256.Sum256([]byte(s))
	return "drift:" + hex.EncodeToString(b[:])
}

func cloneIncident(in Incident) Incident {
	in.Changes = append([]Change(nil), in.Changes...)
	in.Remediation = append([]Change(nil), in.Remediation...)
	return in
}

func (m *Monitor) Incidents() []Incident {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]Incident, 0, len(m.incidents))
	for _, i := range m.incidents {
		out = append(out, cloneIncident(i))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].FirstSeen.Before(out[j].FirstSeen) })
	return out
}

// AcceptBaseline records operator acknowledgement of the current drift. It
// resolves all open incidents for a target without applying any database
// mutation; the next differing observation creates a fresh incident.
func (m *Monitor) AcceptBaseline(targetID string) error {
	if m == nil || targetID == "" {
		return ErrInvalidTarget
	}
	now := m.Now().UTC()
	m.mu.Lock()
	defer m.mu.Unlock()
	for key, inc := range m.incidents {
		if inc.TargetID != targetID || inc.Status != "open" {
			continue
		}
		inc.Status = "resolved"
		inc.LastSeen = now
		inc.ResolvedAt = &now
		m.incidents[key] = inc
	}
	return nil
}
