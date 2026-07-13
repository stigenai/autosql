// Package analytics provides bounded, redacted schema statistics for safety
// decisions and historical operational review.
package analytics

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"sync"
	"time"
)

const Version = "autosql.analytics/v1"

var (
	ErrInvalid   = errors.New("invalid analytics snapshot")
	ErrSensitive = errors.New("analytics data contains sensitive values")
	ErrLimit     = errors.New("analytics collection limit exceeded")
)

type Table struct {
	ID              string `json:"id"`
	Schema          string `json:"schema"`
	Name            string `json:"name"`
	EstimatedRows   int64  `json:"estimated_rows"`
	TotalBytes      int64  `json:"total_bytes"`
	IndexBytes      int64  `json:"index_bytes"`
	LiveRows        int64  `json:"live_rows,omitempty"`
	DeadRows        int64  `json:"dead_rows,omitempty"`
	ReadCount       int64  `json:"read_count,omitempty"`
	WriteCount      int64  `json:"write_count,omitempty"`
	SequentialScans int64  `json:"sequential_scans,omitempty"`
}

type Index struct {
	ID      string `json:"id"`
	TableID string `json:"table_id"`
	Schema  string `json:"schema"`
	Name    string `json:"name"`
	Bytes   int64  `json:"bytes"`
	Scans   int64  `json:"scans"`
	Reads   int64  `json:"reads,omitempty"`
	Writes  int64  `json:"writes,omitempty"`
	Unique  bool   `json:"unique,omitempty"`
	Valid   bool   `json:"valid"`
}

type Constraint struct {
	ID      string `json:"id"`
	TableID string `json:"table_id"`
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Columns int    `json:"columns"`
}

type Complexity struct {
	Score              float64 `json:"score"`
	Tables             int     `json:"tables"`
	Indexes            int     `json:"indexes"`
	Constraints        int     `json:"constraints"`
	ForeignKeys        int     `json:"foreign_keys"`
	Views              int     `json:"views"`
	Triggers           int     `json:"triggers"`
	MaxIndexesPerTable int     `json:"max_indexes_per_table"`
}

type PermissionSummary struct {
	Role    string   `json:"role,omitempty"`
	Granted []string `json:"granted,omitempty"`
	Skipped []string `json:"skipped,omitempty"`
}

type Snapshot struct {
	Version        string            `json:"version"`
	TargetID       string            `json:"target_id"`
	ArtifactDigest string            `json:"artifact_digest"`
	SchemaDigest   string            `json:"schema_digest"`
	ObservedAt     time.Time         `json:"observed_at"`
	Tables         []Table           `json:"tables,omitempty"`
	Indexes        []Index           `json:"indexes,omitempty"`
	Constraints    []Constraint      `json:"constraints,omitempty"`
	Complexity     Complexity        `json:"complexity"`
	Permissions    PermissionSummary `json:"permissions,omitempty"`
}

func digest(s string) bool {
	if len(s) != 71 || !strings.HasPrefix(s, "sha256:") {
		return false
	}
	for _, c := range s[7:] {
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func safe(s string) bool { return !strings.ContainsAny(s, "\r\n\x00") && !strings.Contains(s, "://") }

func (s Snapshot) Validate() error {
	if s.Version != Version || s.TargetID == "" || !safe(s.TargetID) || !digest(s.ArtifactDigest) || !digest(s.SchemaDigest) || s.ObservedAt.IsZero() {
		return ErrInvalid
	}
	seen := map[string]bool{}
	for _, t := range s.Tables {
		if strings.Contains(t.ID+t.Schema+t.Name, "://") {
			return ErrSensitive
		}
		if t.ID == "" || t.Name == "" || !safe(t.ID) || !safe(t.Schema) || !safe(t.Name) || t.EstimatedRows < 0 || t.TotalBytes < 0 || t.IndexBytes < 0 || t.LiveRows < 0 || t.DeadRows < 0 || t.ReadCount < 0 || t.WriteCount < 0 || t.SequentialScans < 0 || seen[t.ID] {
			return ErrInvalid
		}
		seen[t.ID] = true
	}
	for _, i := range s.Indexes {
		if i.ID == "" || i.TableID == "" || i.Name == "" || !safe(i.ID) || !safe(i.TableID) || !safe(i.Schema) || !safe(i.Name) || i.Bytes < 0 || i.Scans < 0 || i.Reads < 0 || i.Writes < 0 || !seen[i.TableID] {
			return ErrInvalid
		}
	}
	for _, c := range s.Constraints {
		if c.ID == "" || c.TableID == "" || c.Kind == "" || c.Name == "" || !safe(c.ID) || !safe(c.TableID) || !safe(c.Kind) || !safe(c.Name) || c.Columns < 0 || !seen[c.TableID] {
			return ErrInvalid
		}
	}
	if s.Complexity.Score < 0 || s.Complexity.Tables != len(s.Tables) || s.Complexity.Indexes != len(s.Indexes) || s.Complexity.Constraints != len(s.Constraints) {
		return ErrInvalid
	}
	if s.Permissions.Role != "" && !safe(s.Permissions.Role) {
		return ErrSensitive
	}
	for _, v := range append(append([]string{}, s.Permissions.Granted...), s.Permissions.Skipped...) {
		if !safe(v) {
			return ErrSensitive
		}
	}
	return nil
}

func (s Snapshot) Digest() (string, error) {
	if err := s.Validate(); err != nil {
		return "", err
	}
	b, err := json.Marshal(s)
	if err != nil {
		return "", err
	}
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:]), nil
}

type Request struct {
	TargetID, ArtifactDigest, SchemaDigest string
	MaxTables, MaxIndexes, MaxConstraints  int
}

func (r Request) Validate() error {
	if r.TargetID == "" || !safe(r.TargetID) || !digest(r.ArtifactDigest) || !digest(r.SchemaDigest) || r.MaxTables <= 0 || r.MaxIndexes <= 0 || r.MaxConstraints <= 0 {
		return ErrInvalid
	}
	return nil
}

// Source is implemented by a read-only catalog adapter. Implementations must
// enforce permissions and return only aggregate metadata, never row contents.
type Source interface {
	Collect(Request) (tables []Table, indexes []Index, constraints []Constraint, permissions PermissionSummary, err error)
}
type Collector struct {
	Source Source
	Now    func() time.Time
}

func (c Collector) Collect(r Request) (Snapshot, error) {
	if err := r.Validate(); err != nil || c.Source == nil {
		return Snapshot{}, ErrInvalid
	}
	t, i, co, p, err := c.Source.Collect(r)
	if err != nil {
		return Snapshot{}, err
	}
	if len(t) > r.MaxTables || len(i) > r.MaxIndexes || len(co) > r.MaxConstraints {
		return Snapshot{}, ErrLimit
	}
	s := Snapshot{Version: Version, TargetID: r.TargetID, ArtifactDigest: r.ArtifactDigest, SchemaDigest: r.SchemaDigest, ObservedAt: time.Now().UTC(), Tables: t, Indexes: i, Constraints: co, Permissions: p}
	if c.Now != nil {
		s.ObservedAt = c.Now().UTC()
	}
	s.Complexity = score(s)
	if err := s.Validate(); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}

func score(s Snapshot) Complexity {
	c := Complexity{Tables: len(s.Tables), Indexes: len(s.Indexes), Constraints: len(s.Constraints)}
	for _, i := range s.Indexes {
		if i.Scans == 0 {
			c.Score += 1
		}
		for _, t := range s.Tables {
			if t.ID == i.TableID {
				n := 0
				for _, x := range s.Indexes {
					if x.TableID == t.ID {
						n++
					}
				}
				if n > c.MaxIndexesPerTable {
					c.MaxIndexesPerTable = n
				}
				break
			}
		}
	}
	for _, x := range s.Constraints {
		if strings.EqualFold(x.Kind, "foreign_key") {
			c.ForeignKeys++
		}
	}
	c.Score += float64(c.Tables) + float64(c.Indexes)*0.5 + float64(c.ForeignKeys)*0.75
	return c
}

type Thresholds struct {
	MaxTotalBytes, MaxDeadRows, MaxUnusedIndexes, MaxComplexity float64
	MaxGrowthBytes                                              int64
}
type Finding struct {
	Code, Severity, Message, Remediation string
	Value, Limit                         float64
}

func Evaluate(s Snapshot, previous *Snapshot, t Thresholds) ([]Finding, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	if previous != nil && (previous.TargetID != s.TargetID || previous.SchemaDigest != s.SchemaDigest) {
		previous = nil
	}
	var out []Finding
	total, dead := int64(0), int64(0)
	unused := 0
	for _, x := range s.Tables {
		total += x.TotalBytes
		dead += x.DeadRows
	}
	for _, x := range s.Indexes {
		if x.Scans == 0 {
			unused++
		}
	}
	if t.MaxTotalBytes > 0 && float64(total) > t.MaxTotalBytes {
		out = append(out, Finding{"table_bytes", "warning", "table storage exceeds policy threshold", "review growth or partitioning", float64(total), t.MaxTotalBytes})
	}
	if t.MaxDeadRows > 0 && float64(dead) > t.MaxDeadRows {
		out = append(out, Finding{"dead_rows", "warning", "dead rows exceed policy threshold", "schedule bounded maintenance", float64(dead), t.MaxDeadRows})
	}
	if t.MaxUnusedIndexes > 0 && float64(unused) > t.MaxUnusedIndexes {
		out = append(out, Finding{"unused_indexes", "warning", "unused indexes exceed policy threshold", "confirm workload before removal", float64(unused), t.MaxUnusedIndexes})
	}
	if t.MaxComplexity > 0 && s.Complexity.Score > t.MaxComplexity {
		out = append(out, Finding{"complexity", "warning", "schema complexity exceeds policy threshold", "review dependencies and ownership", s.Complexity.Score, t.MaxComplexity})
	}
	if previous != nil && t.MaxGrowthBytes > 0 {
		old := int64(0)
		for _, x := range previous.Tables {
			old += x.TotalBytes
		}
		if total-old > t.MaxGrowthBytes {
			out = append(out, Finding{"growth_bytes", "warning", "schema storage growth exceeds policy threshold", "investigate growth before rollout", float64(total - old), float64(t.MaxGrowthBytes)})
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Code < out[j].Code })
	return out, nil
}

type Store struct {
	mu           sync.RWMutex
	byTarget     map[string][]Snapshot
	Retention    time.Duration
	MaxSnapshots int
}

func NewStore(retention time.Duration, max int) *Store {
	return &Store{byTarget: map[string][]Snapshot{}, Retention: retention, MaxSnapshots: max}
}
func (s *Store) Append(v Snapshot) error {
	if s == nil || v.Validate() != nil {
		return ErrInvalid
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	a := append(s.byTarget[v.TargetID], v)
	sort.SliceStable(a, func(i, j int) bool { return a[i].ObservedAt.Before(a[j].ObservedAt) })
	cutoff := time.Time{}
	if s.Retention > 0 {
		cutoff = v.ObservedAt.Add(-s.Retention)
	}
	out := a[:0]
	for _, x := range a {
		if cutoff.IsZero() || !x.ObservedAt.Before(cutoff) {
			out = append(out, x)
		}
	}
	if s.MaxSnapshots > 0 && len(out) > s.MaxSnapshots {
		out = out[len(out)-s.MaxSnapshots:]
	}
	s.byTarget[v.TargetID] = out
	return nil
}
func (s *Store) Query(target string, from, to time.Time) []Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []Snapshot
	for _, x := range s.byTarget[target] {
		if !from.IsZero() && x.ObservedAt.Before(from) {
			continue
		}
		if !to.IsZero() && x.ObservedAt.After(to) {
			continue
		}
		out = append(out, x)
	}
	return append([]Snapshot(nil), out...)
}

// StaticSource makes deterministic fixtures and permission-aware adapters easy
// to test without a database connection.
type StaticSource struct {
	Tables      []Table
	Indexes     []Index
	Constraints []Constraint
	Permissions PermissionSummary
}

func (s StaticSource) Collect(Request) ([]Table, []Index, []Constraint, PermissionSummary, error) {
	return append([]Table(nil), s.Tables...), append([]Index(nil), s.Indexes...), append([]Constraint(nil), s.Constraints...), s.Permissions, nil
}
