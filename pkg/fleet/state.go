package fleet

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sync"
	"time"
)

var (
	ErrInvalid      = ErrInvalidRollout
	ErrConflict     = errors.New("fleet target execution conflict")
	ErrNotFound     = errors.New("fleet rollout not found")
	ErrGate         = errors.New("fleet gate blocked")
	ErrUnauthorized = errors.New("fleet recovery unauthorized")
	ErrCanceled     = errors.New("fleet rollout canceled")
	ErrPolicy       = errors.New("fleet policy denied")
)

type TargetState string

const (
	Pending   TargetState = "pending"
	Running   TargetState = "running"
	Succeeded TargetState = "succeeded"
	Failed    TargetState = "failed"
	Canceled  TargetState = "canceled"
	Skipped   TargetState = "skipped"
	Drifted   TargetState = "drifted"
)

type TargetResult struct {
	TargetID  string      `json:"target_id"`
	State     TargetState `json:"state"`
	Attempts  int         `json:"attempts"`
	LastError string      `json:"last_error,omitempty"`
	UpdatedAt time.Time   `json:"updated_at"`
}
type RolloutState struct {
	RolloutID, PlanID, ArtifactDigest string
	State                             string
	Targets                           map[string]TargetResult
	TargetMeta                        map[string]Target
	Events                            []Event
	UpdatedAt                         time.Time
}
type Event struct {
	At             time.Time `json:"at"`
	Type           string    `json:"type"`
	RolloutID      string    `json:"rollout_id"`
	TargetID       string    `json:"target_id,omitempty"`
	ArtifactDigest string    `json:"artifact_digest"`
	Reason         string    `json:"reason,omitempty"`
}
type Store struct {
	mu       sync.Mutex
	rollouts map[string]RolloutState
	active   map[string]string
}

func cloneMap(m map[string]string) map[string]string { return cloneLabels(m) }

func NewStore() *Store {
	return &Store{rollouts: map[string]RolloutState{}, active: map[string]string{}}
}
func (s *Store) Create(id string, p RolloutPlan, targets []Target, now time.Time) (RolloutState, error) {
	if s == nil || id == "" || p.ArtifactDigest == "" || p.SnapshotID == "" {
		return RolloutState{}, ErrInvalidRollout
	}
	if err := p.Validate(targets); err != nil {
		return RolloutState{}, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.rollouts[id]; ok {
		return RolloutState{}, ErrConflict
	}
	r := RolloutState{RolloutID: id, PlanID: p.SnapshotID, ArtifactDigest: p.ArtifactDigest, State: "pending", Targets: map[string]TargetResult{}, TargetMeta: map[string]Target{}, UpdatedAt: now.UTC()}
	for _, t := range targets {
		r.Targets[t.ID] = TargetResult{TargetID: t.ID, State: Pending, UpdatedAt: now.UTC()}
		r.TargetMeta[t.ID] = cloneTarget(t)
	}
	s.rollouts[id] = r
	return cloneState(r), nil
}
func (s *Store) Get(id string) (RolloutState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.rollouts[id]
	if !ok {
		return RolloutState{}, ErrNotFound
	}
	return cloneState(r), nil
}
func cloneState(r RolloutState) RolloutState {
	targets := r.Targets
	r.Targets = map[string]TargetResult{}
	for k, v := range targets {
		r.Targets[k] = v
	}
	r.TargetMeta = map[string]Target{}
	for k, v := range r.TargetMeta {
		r.TargetMeta[k] = cloneTarget(v)
	}
	r.Events = append([]Event(nil), r.Events...)
	return r
}
func eventHash(e Event) string {
	b, _ := json.Marshal(e)
	h := sha256.Sum256(b)
	return hex.EncodeToString(h[:])
}
