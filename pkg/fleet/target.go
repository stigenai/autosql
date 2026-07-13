// Package fleet models deployment targets and deterministic target snapshots.
package fleet

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"path"
	"regexp"
	"sort"
	"strings"
)

var (
	ErrInvalidTarget   = errors.New("invalid fleet target")
	ErrDuplicateTarget = errors.New("duplicate fleet target identity")
	ErrInvalidFilter   = errors.New("invalid fleet target filter")
)

var targetIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,191}$`)

// Target is the normalized, credential-free identity and deployment metadata
// for one database. ConnectionRef is a secret reference (for example
// env://PROD_DATABASE_URL), never a resolved credential.
type Target struct {
	ID               string            `json:"id"`
	Project          string            `json:"project,omitempty"`
	Environment      string            `json:"environment"`
	Region           string            `json:"region,omitempty"`
	Tenant           string            `json:"tenant,omitempty"`
	Tier             string            `json:"tier,omitempty"`
	Labels           map[string]string `json:"labels,omitempty"`
	ConnectionRef    string            `json:"connection_ref"`
	CurrentArtifact  string            `json:"current_artifact,omitempty"`
	ExpectedArtifact string            `json:"expected_artifact,omitempty"`
	SnapshotDigest   string            `json:"snapshot_digest,omitempty"`
}

func (t Target) Validate() error {
	if !targetIDPattern.MatchString(t.ID) || strings.TrimSpace(t.Environment) == "" || strings.TrimSpace(t.ConnectionRef) == "" {
		return ErrInvalidTarget
	}
	if strings.ContainsAny(t.ID, " \t\r\n") || strings.ContainsAny(t.Environment, "\r\n") || strings.ContainsAny(t.ConnectionRef, "\r\n") {
		return ErrInvalidTarget
	}
	if strings.Contains(t.ConnectionRef, "://") {
		u, err := url.Parse(t.ConnectionRef)
		if err != nil || u.Scheme == "" {
			return ErrInvalidTarget
		}
	}
	for k, v := range t.Labels {
		if strings.TrimSpace(k) == "" || strings.ContainsAny(k+v, "\r\n") {
			return ErrInvalidTarget
		}
	}
	if t.ExpectedArtifact != "" && !digestPattern.MatchString(t.ExpectedArtifact) {
		return ErrInvalidTarget
	}
	return nil
}

var digestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type TargetFilter struct {
	Environment string            `json:"environment,omitempty"`
	Labels      map[string]string `json:"labels,omitempty"`
	IDs         []string          `json:"ids,omitempty"`
}

func (f TargetFilter) Validate() error {
	if f.Environment != "" {
		if _, err := path.Match(f.Environment, "x"); err != nil {
			return ErrInvalidFilter
		}
	}
	for k, v := range f.Labels {
		if k == "" || v == "" {
			return ErrInvalidFilter
		}
		if _, err := path.Match(v, "x"); err != nil {
			return ErrInvalidFilter
		}
	}
	seen := map[string]bool{}
	for _, id := range f.IDs {
		if !targetIDPattern.MatchString(id) || seen[id] {
			return ErrInvalidFilter
		}
		seen[id] = true
	}
	return nil
}

func (f TargetFilter) matches(t Target) bool {
	if f.Environment != "" {
		ok, _ := path.Match(f.Environment, t.Environment)
		if !ok {
			return false
		}
	}
	if len(f.IDs) > 0 {
		found := false
		for _, id := range f.IDs {
			if id == t.ID {
				found = true
				break
			}
		}
		if !found {
			return false
		}
	}
	for k, want := range f.Labels {
		got, ok := t.Labels[k]
		match, _ := path.Match(want, got)
		if !ok || !match {
			return false
		}
	}
	return true
}

// DiscoveryProvider returns targets. Providers may resolve references while
// discovering, but the returned snapshot masks them before persistence/output.
type DiscoveryProvider interface {
	Discover(context.Context) ([]Target, error)
}

type StaticProvider struct{ Targets []Target }

func (p StaticProvider) Discover(context.Context) ([]Target, error) {
	return append([]Target(nil), p.Targets...), nil
}

// Snapshot is a deterministic, credential-masked inventory of targets. ID is
// a content digest, so plans can bind to exactly the enumerated set.
type Snapshot struct {
	ID      string   `json:"id"`
	Targets []Target `json:"targets"`
}

func BuildSnapshot(ctx context.Context, providers ...DiscoveryProvider) (Snapshot, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	all := make([]Target, 0)
	for _, p := range providers {
		if p == nil {
			return Snapshot{}, ErrInvalidTarget
		}
		targets, err := p.Discover(ctx)
		if err != nil {
			return Snapshot{}, err
		}
		all = append(all, targets...)
	}
	return NewSnapshot(all)
}

func NewSnapshot(targets []Target) (Snapshot, error) {
	seen := make(map[string]bool, len(targets))
	normalized := make([]Target, 0, len(targets))
	for _, t := range targets {
		if err := t.Validate(); err != nil {
			return Snapshot{}, err
		}
		if seen[t.ID] {
			return Snapshot{}, fmt.Errorf("%w: %s", ErrDuplicateTarget, t.ID)
		}
		seen[t.ID] = true
		t.ConnectionRef = MaskConnectionRef(t.ConnectionRef)
		t.Labels = cloneLabels(t.Labels)
		normalized = append(normalized, t)
	}
	sort.Slice(normalized, func(i, j int) bool { return normalized[i].ID < normalized[j].ID })
	s := Snapshot{Targets: normalized}
	raw, _ := json.Marshal(s.Targets)
	h := sha256.Sum256(append([]byte("autosql.fleet.snapshot/v1\x00"), raw...))
	s.ID = "sha256:" + hex.EncodeToString(h[:])
	return s, nil
}

func (s Snapshot) Validate() error {
	copy, err := NewSnapshot(s.Targets)
	if err != nil || copy.ID != s.ID {
		return ErrInvalidTarget
	}
	return nil
}

func (s Snapshot) Select(f TargetFilter) ([]Target, error) {
	if err := f.Validate(); err != nil {
		return nil, err
	}
	if err := s.Validate(); err != nil {
		return nil, err
	}
	out := make([]Target, 0)
	for _, t := range s.Targets {
		if f.matches(t) {
			out = append(out, cloneTarget(t))
		}
	}
	return out, nil
}

func cloneLabels(in map[string]string) map[string]string {
	if in == nil {
		return nil
	}
	out := make(map[string]string, len(in))
	for k, v := range in {
		out[k] = v
	}
	return out
}
func cloneTarget(t Target) Target { t.Labels = cloneLabels(t.Labels); return t }

// MaskConnectionRef removes URI userinfo and common secret query values. It is
// deterministic and safe for dry-run output; references such as env://NAME
// remain unchanged.
func MaskConnectionRef(ref string) string {
	if u, err := url.Parse(ref); err == nil && u.Scheme != "" {
		u.User = nil
		q := u.Query()
		for key := range q {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "password") || strings.Contains(lower, "secret") || strings.Contains(lower, "token") || lower == "key" {
				q.Set(key, "[REDACTED]")
			}
		}
		u.RawQuery = q.Encode()
		return u.String()
	}
	return ref
}
