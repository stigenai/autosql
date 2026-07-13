// Package inventory models database targets and their deployment history.
//
// The store deliberately accepts only already-redacted, structured metadata:
// credentials, SQL text, and query results must never enter the inventory.
package inventory

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"
	"sync"
	"time"
)

type SyncStatus string

const (
	SyncCurrent SyncStatus = "current"
	SyncPending SyncStatus = "pending"
	SyncDrifted SyncStatus = "drifted"
	SyncUnknown SyncStatus = "unknown"
	SyncFailed  SyncStatus = "failed"
)

type EventStatus string

const (
	Passed              EventStatus = "passed"
	Failed              EventStatus = "failed"
	NoOp                EventStatus = "no-op"
	DryRun              EventStatus = "dry-run"
	Canceled            EventStatus = "canceled"
	PartiallySuccessful EventStatus = "partially-successful"
)

var (
	ErrInvalid   = errors.New("invalid inventory record")
	ErrConflict  = errors.New("inventory idempotency conflict")
	ErrNotFound  = errors.New("inventory target or event not found")
	ErrSensitive = errors.New("inventory record contains sensitive data")
)

func validStatus(s SyncStatus) bool {
	switch s {
	case SyncCurrent, SyncPending, SyncDrifted, SyncUnknown, SyncFailed:
		return true
	}
	return false
}
func validEventStatus(s EventStatus) bool {
	switch s {
	case Passed, Failed, NoOp, DryRun, Canceled, PartiallySuccessful:
		return true
	}
	return false
}

// Target identifies a database without storing a connection string.
type Target struct {
	Project     string `json:"project"`
	Environment string `json:"environment"`
	ID          string `json:"target_id"`
}

func (t Target) Key() string { return t.Project + "\x00" + t.Environment + "\x00" + t.ID }

type Observation struct {
	ReportID        string     `json:"report_id"`
	CurrentVersion  string     `json:"current_version"`
	ExpectedVersion string     `json:"expected_version"`
	SyncStatus      SyncStatus `json:"sync_status"`
	ObservedAt      time.Time  `json:"observed_at"`
}

type Record struct {
	Target
	CurrentVersion  string     `json:"current_version"`
	ExpectedVersion string     `json:"expected_version"`
	SyncStatus      SyncStatus `json:"sync_status"`
	LastObservedAt  time.Time  `json:"last_observed_at"`
	LastReportID    string     `json:"last_report_id"`
}

// TargetResult is intentionally limited to safe, non-SQL deployment facts.
type TargetResult struct {
	TargetID string            `json:"target_id"`
	Status   EventStatus       `json:"status"`
	Details  map[string]string `json:"details,omitempty"`
}

type DeploymentEvent struct {
	ID             string         `json:"id"`
	RunID          string         `json:"run_id"`
	ArtifactDigest string         `json:"artifact_digest"`
	Project        string         `json:"project"`
	Environment    string         `json:"environment"`
	Status         EventStatus    `json:"status"`
	At             time.Time      `json:"at"`
	Targets        []TargetResult `json:"targets"`
}

type Query struct {
	Project, Environment, TargetID, ArtifactDigest string
	Statuses                                       []EventStatus
	From, To                                       time.Time
}

type Store struct {
	mu      sync.RWMutex
	targets map[string]Record
	reports map[string]string
	events  map[string]DeploymentEvent
}

func NewStore() *Store {
	return &Store{targets: map[string]Record{}, reports: map[string]string{}, events: map[string]DeploymentEvent{}}
}

func (s *Store) Upsert(t Target, o Observation) (Record, error) {
	if s == nil || t.Project == "" || t.Environment == "" || t.ID == "" || o.ReportID == "" || o.ObservedAt.IsZero() || !validStatus(o.SyncStatus) {
		return Record{}, ErrInvalid
	}
	if err := safeTarget(t); err != nil {
		return Record{}, err
	}
	fp := fingerprint(struct {
		T Target
		O Observation
	}{t, o})
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.reports[o.ReportID]; ok {
		if old != fp {
			return Record{}, ErrConflict
		}
		return s.targets[t.Key()], nil
	}
	r := Record{Target: t, CurrentVersion: o.CurrentVersion, ExpectedVersion: o.ExpectedVersion, SyncStatus: o.SyncStatus, LastObservedAt: o.ObservedAt.UTC(), LastReportID: o.ReportID}
	s.targets[t.Key()] = r
	s.reports[o.ReportID] = fp
	return r, nil
}

func (s *Store) Get(t Target) (Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.targets[t.Key()]
	if !ok {
		return Record{}, ErrNotFound
	}
	return r, nil
}

func (s *Store) Append(e DeploymentEvent) error {
	if s == nil || e.ID == "" || e.Project == "" || e.Environment == "" || e.At.IsZero() || !validEventStatus(e.Status) || len(e.Targets) == 0 {
		return ErrInvalid
	}
	if e.Status == PartiallySuccessful && len(e.Targets) < 2 {
		return ErrInvalid
	}
	if err := safeDeployment(e); err != nil {
		return err
	}
	e.At = e.At.UTC()
	sort.Slice(e.Targets, func(i, j int) bool { return e.Targets[i].TargetID < e.Targets[j].TargetID })
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.events[e.ID]; ok {
		if fingerprint(old) != fingerprint(e) {
			return ErrConflict
		}
		return nil
	}
	s.events[e.ID] = cloneEvent(e)
	return nil
}

func (s *Store) Events(q Query) []DeploymentEvent {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]DeploymentEvent, 0)
	for _, e := range s.events {
		if q.Project != "" && e.Project != q.Project || q.Environment != "" && e.Environment != q.Environment || q.ArtifactDigest != "" && e.ArtifactDigest != q.ArtifactDigest || !q.From.IsZero() && e.At.Before(q.From) || !q.To.IsZero() && e.At.After(q.To) || len(q.Statuses) > 0 && !containsStatus(q.Statuses, e.Status) {
			continue
		}
		if q.TargetID != "" && !eventTargets(e, q.TargetID) {
			continue
		}
		out = append(out, cloneEvent(e))
	}
	sort.Slice(out, func(i, j int) bool { return out[i].At.Before(out[j].At) })
	return out
}

func containsStatus(xs []EventStatus, x EventStatus) bool {
	for _, y := range xs {
		if x == y {
			return true
		}
	}
	return false
}
func eventTargets(e DeploymentEvent, id string) bool {
	for _, t := range e.Targets {
		if t.TargetID == id {
			return true
		}
	}
	return false
}
func cloneEvent(e DeploymentEvent) DeploymentEvent {
	e.Targets = append([]TargetResult(nil), e.Targets...)
	for i := range e.Targets {
		e.Targets[i].Details = cloneMap(e.Targets[i].Details)
	}
	return e
}
func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	o := map[string]string{}
	for k, v := range m {
		o[k] = v
	}
	return o
}
func fingerprint(v any) string {
	b, _ := json.Marshal(v)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}

func safeTarget(t Target) error {
	for _, x := range []string{t.Project, t.Environment, t.ID} {
		if strings.ContainsAny(x, "\r\n") || strings.Contains(strings.ToLower(x), "password") || strings.Contains(x, "@") {
			return ErrSensitive
		}
		if u, err := url.Parse(x); err == nil && u.User != nil {
			return ErrSensitive
		}
	}
	return nil
}
func safeDeployment(e DeploymentEvent) error {
	if err := safeTarget(Target{Project: e.Project, Environment: e.Environment, ID: "event"}); err != nil {
		return err
	}
	for _, t := range e.Targets {
		if t.TargetID == "" || !validEventStatus(t.Status) {
			return ErrInvalid
		}
		if err := safeDetails(t.Details); err != nil {
			return err
		}
	}
	return nil
}
func safeDetails(m map[string]string) error {
	for k, v := range m {
		l := strings.ToLower(k)
		for _, bad := range []string{"password", "secret", "token", "credential", "dsn", "connection", "query", "sql", "result", "row"} {
			if strings.Contains(l, bad) {
				return fmt.Errorf("%w: %s", ErrSensitive, k)
			}
		}
		if strings.ContainsAny(v, "\r\n") {
			return ErrSensitive
		}
	}
	return nil
}
