// Package deploy contains secret-free Terraform and generic deployment
// integration contracts bound to immutable artifacts.
package deploy

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"
)

var (
	ErrInvalid  = errors.New("invalid deployment request")
	ErrConflict = errors.New("deployment artifact conflict")
	ErrNotFound = errors.New("deployment not found")
	ErrDestroy  = errors.New("destroy requires explicit approval")
)

type Action string

const (
	ActionPlan    Action = "plan"
	ActionApply   Action = "apply"
	ActionObserve Action = "observe"
	ActionCancel  Action = "cancel"
	ActionDestroy Action = "destroy"
	Plan                 = ActionPlan
	Apply                = ActionApply
	Observe              = ActionObserve
	Cancel               = ActionCancel
	Destroy              = ActionDestroy
)

type Request struct {
	// ID is retained as a short alias for DeploymentID for generic runners.
	ID              string
	DeploymentID    string
	ArtifactDigest  string
	TargetSnapshot  string
	TargetID        string
	Environment     string
	ConnectionRef   string
	Action          Action
	DestroyApproved bool
}

func (r Request) Validate() error {
	if r.DeploymentID == "" {
		r.DeploymentID = r.ID
	}
	if r.DeploymentID == "" || r.ArtifactDigest == "" || r.Environment == "" || r.Action == "" {
		return ErrInvalid
	}
	if r.ConnectionRef != "" && !strings.HasPrefix(r.ConnectionRef, "env://") {
		return errors.New("resolved connection secret in state")
	}
	if r.Action == ActionDestroy && !r.DestroyApproved {
		return ErrDestroy
	}
	return nil
}

// Plan records an idempotent Terraform-style plan in the deployment store.
func (s *Store) Plan(r Request) (PlanResult, error) {
	if r.DeploymentID == "" {
		r.DeploymentID = r.ID
	}
	s.mu.Lock()
	if s.plans == nil {
		s.plans = map[string]PlanResult{}
	}
	priorValue, exists := s.plans[r.DeploymentID]
	s.mu.Unlock()
	var prior *PlanResult
	if exists {
		prior = &priorValue
	}
	p, err := TerraformPlan(r, prior)
	if err != nil {
		return PlanResult{}, err
	}
	s.mu.Lock()
	s.plans[r.DeploymentID] = p
	s.mu.Unlock()
	return p, nil
}

type PlanResult struct {
	DeploymentID, ArtifactDigest, TargetSnapshot string
	NoOp                                         bool
	Action                                       Action
	CreatedAt                                    time.Time
}

func TerraformPlan(r Request, prior *PlanResult) (PlanResult, error) {
	if err := r.Validate(); err != nil {
		return PlanResult{}, err
	}
	if r.DeploymentID == "" {
		r.DeploymentID = r.ID
	}
	if prior != nil && prior.DeploymentID == r.DeploymentID && prior.ArtifactDigest == r.ArtifactDigest && prior.TargetSnapshot == r.TargetSnapshot {
		out := *prior
		out.NoOp = true
		return out, nil
	}
	return PlanResult{DeploymentID: r.DeploymentID, ArtifactDigest: r.ArtifactDigest, TargetSnapshot: r.TargetSnapshot, Action: r.Action, CreatedAt: time.Now().UTC()}, nil
}

type Result struct {
	ID, ArtifactDigest, TargetID, State string
	Action                              Action
	StartedAt, UpdatedAt                time.Time
	Message                             string
}
type Runner interface {
	Run(context.Context, Request) (string, error)
	Observe(context.Context, string) (Result, error)
	Cancel(context.Context, string) error
}
type Store struct {
	mu      sync.Mutex
	results map[string]Result
	plans   map[string]PlanResult
}

func NewStore() *Store { return &Store{results: map[string]Result{}, plans: map[string]PlanResult{}} }
func (s *Store) Apply(ctx context.Context, r Request, run Runner) (Result, error) {
	if err := r.Validate(); err != nil {
		return Result{}, err
	}
	if run == nil {
		return Result{}, ErrInvalid
	}
	s.mu.Lock()
	if old, ok := s.results[r.DeploymentID]; ok && old.ArtifactDigest != r.ArtifactDigest {
		s.mu.Unlock()
		return Result{}, ErrConflict
	}
	now := time.Now().UTC()
	x := Result{ID: r.DeploymentID, ArtifactDigest: r.ArtifactDigest, TargetID: r.TargetID, Action: r.Action, State: "running", StartedAt: now, UpdatedAt: now}
	s.results[r.DeploymentID] = x
	s.mu.Unlock()
	msg, err := run.Run(ctx, r)
	s.mu.Lock()
	defer s.mu.Unlock()
	x = s.results[r.DeploymentID]
	x.UpdatedAt = time.Now().UTC()
	if err != nil {
		x.State = "failed"
		x.Message = "deployment failed"
	} else {
		x.State = "applied"
		x.Message = msg
	}
	s.results[r.DeploymentID] = x
	return x, err
}
func (s *Store) Get(id string) (Result, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	x, ok := s.results[id]
	if !ok {
		return Result{}, ErrNotFound
	}
	return x, nil
}

type WebhookRequest struct {
	Event, CorrelationID, ArtifactDigest, TargetID string
	Payload                                        map[string]string
}
type Webhook interface {
	Send(context.Context, WebhookRequest) error
}
type Generic struct{ Hook Webhook }

func (g Generic) Start(ctx context.Context, r Request) error   { return g.send(ctx, "start", r) }
func (g Generic) Observe(ctx context.Context, r Request) error { return g.send(ctx, "observe", r) }
func (g Generic) Cancel(ctx context.Context, r Request) error  { return g.send(ctx, "cancel", r) }
func (g Generic) send(ctx context.Context, e string, r Request) error {
	if g.Hook == nil || r.Validate() != nil {
		return ErrInvalid
	}
	return g.Hook.Send(ctx, WebhookRequest{Event: e, CorrelationID: r.DeploymentID, ArtifactDigest: r.ArtifactDigest, TargetID: r.TargetID})
}

type RemoteEvent struct {
	Event, CorrelationID, ArtifactDigest, TargetID string
	At                                             time.Time
}
type MemoryRemote struct {
	mu     sync.Mutex
	events map[string][]RemoteEvent
}

func NewMemoryRemote() *MemoryRemote { return &MemoryRemote{events: map[string][]RemoteEvent{}} }
func (m *MemoryRemote) Start(_ context.Context, r Request) (RemoteEvent, error) {
	if err := r.Validate(); err != nil {
		return RemoteEvent{}, err
	}
	e := RemoteEvent{Event: "start", CorrelationID: r.DeploymentID, ArtifactDigest: r.ArtifactDigest, TargetID: r.TargetID, At: time.Now().UTC()}
	m.mu.Lock()
	m.events[r.DeploymentID] = append(m.events[r.DeploymentID], e)
	m.mu.Unlock()
	return e, nil
}
func (m *MemoryRemote) Observe(_ context.Context, id string) ([]RemoteEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.events[id]; !ok {
		return nil, ErrNotFound
	}
	return append([]RemoteEvent(nil), m.events[id]...), nil
}
func (m *MemoryRemote) Cancel(_ context.Context, id string) (RemoteEvent, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if _, ok := m.events[id]; !ok {
		return RemoteEvent{}, ErrNotFound
	}
	e := RemoteEvent{Event: "cancel", CorrelationID: id, At: time.Now().UTC()}
	m.events[id] = append(m.events[id], e)
	return e, nil
}
