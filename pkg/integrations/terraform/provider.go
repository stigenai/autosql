// Package terraform implements the protocol-neutral core of the AutoSQL
// Terraform provider. It is intentionally independent of Terraform SDK
// versions so the same plan/state contract can be used by Terraform, OpenTofu,
// and offline CI validators.
package terraform

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"autosql/pkg/integrations/deploy"
)

var (
	ErrInvalid       = errors.New("invalid Terraform provider request")
	ErrSensitive     = errors.New("sensitive database value cannot enter Terraform state")
	ErrApproval      = errors.New("Terraform apply requires an approved plan")
	ErrStateConflict = errors.New("Terraform state lock conflict")
	ErrArtifact      = errors.New("artifact and policy binding mismatch")
	ErrNotFound      = errors.New("Terraform resource not found")
)

const ProtocolVersion = "autosql.terraform/v1"

type Workflow string

const (
	Declarative Workflow = "declarative"
	Versioned   Workflow = "versioned"
)

// ResourceConfig is the non-sensitive Terraform resource model. ConnectionRef
// must remain an opaque env:// or file:// reference; a provider must never
// write a resolved URL or password into state.
type ResourceConfig struct {
	ID             string   `json:"id"`
	Workflow       Workflow `json:"workflow"`
	SourceRef      string   `json:"source_ref"`
	ArtifactDigest string   `json:"artifact_digest"`
	PolicyDigest   string   `json:"policy_digest"`
	TargetSnapshot string   `json:"target_snapshot"`
	TargetID       string   `json:"target_id"`
	Environment    string   `json:"environment"`
	ConnectionRef  string   `json:"connection_ref"`
	Destroy        bool     `json:"destroy,omitempty"`
}

type State struct {
	ID             string    `json:"id"`
	Workflow       Workflow  `json:"workflow"`
	SourceRef      string    `json:"source_ref"`
	ArtifactDigest string    `json:"artifact_digest"`
	PolicyDigest   string    `json:"policy_digest"`
	TargetSnapshot string    `json:"target_snapshot"`
	TargetID       string    `json:"target_id"`
	Environment    string    `json:"environment"`
	ConnectionRef  string    `json:"connection_ref"`
	AppliedAt      time.Time `json:"applied_at,omitempty"`
	ObservedDigest string    `json:"observed_digest,omitempty"`
}

func (c ResourceConfig) Validate() error {
	if strings.TrimSpace(c.ID) == "" || strings.TrimSpace(c.SourceRef) == "" ||
		strings.TrimSpace(c.ArtifactDigest) == "" || strings.TrimSpace(c.PolicyDigest) == "" ||
		strings.TrimSpace(c.TargetSnapshot) == "" || strings.TrimSpace(c.TargetID) == "" ||
		strings.TrimSpace(c.Environment) == "" || (c.Workflow != Declarative && c.Workflow != Versioned) {
		return ErrInvalid
	}
	if err := validateReference(c.ConnectionRef); err != nil {
		return err
	}
	return nil
}

func (s State) Validate() error {
	return (ResourceConfig{ID: s.ID, Workflow: s.Workflow, SourceRef: s.SourceRef, ArtifactDigest: s.ArtifactDigest, PolicyDigest: s.PolicyDigest, TargetSnapshot: s.TargetSnapshot, TargetID: s.TargetID, Environment: s.Environment, ConnectionRef: s.ConnectionRef}).Validate()
}

func validateReference(ref string) error {
	if strings.TrimSpace(ref) == "" {
		return ErrInvalid
	}
	if strings.HasPrefix(ref, "env://") || strings.HasPrefix(ref, "file://") {
		return nil
	}
	// Reject URL credentials and common provider state mistakes. A provider
	// may accept another opaque reference scheme only after adding it here.
	if strings.Contains(ref, "://") || strings.ContainsAny(ref, "@") || strings.Contains(strings.ToLower(ref), "password") {
		return ErrSensitive
	}
	return ErrInvalid
}

func (s State) MarshalJSON() ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, err
	}
	type alias State
	return json.Marshal(alias(s))
}

type Action string

const (
	Create Action = "create"
	Update Action = "update"
	Delete Action = "delete"
	NoOp   Action = "no-op"
)

type PlanRequest struct {
	Desired ResourceConfig
	Prior   *State
	Approve bool
}

type Plan struct {
	ProtocolVersion string    `json:"protocol_version"`
	ResourceID      string    `json:"resource_id"`
	Action          Action    `json:"action"`
	ArtifactDigest  string    `json:"artifact_digest"`
	PolicyDigest    string    `json:"policy_digest"`
	TargetSnapshot  string    `json:"target_snapshot"`
	RequiresApply   bool      `json:"requires_apply"`
	PlanDigest      string    `json:"plan_digest"`
	CreatedAt       time.Time `json:"created_at"`
}

func PlanResource(req PlanRequest) (Plan, error) {
	if err := req.Desired.Validate(); err != nil {
		return Plan{}, err
	}
	if req.Prior != nil {
		if err := req.Prior.Validate(); err != nil {
			return Plan{}, err
		}
		if req.Prior.ID != req.Desired.ID {
			return Plan{}, fmt.Errorf("%w: resource id changed", ErrInvalid)
		}
	}
	action := Create
	if req.Prior != nil {
		action = Update
		if req.Prior.ArtifactDigest == req.Desired.ArtifactDigest && req.Prior.PolicyDigest == req.Desired.PolicyDigest && req.Prior.TargetSnapshot == req.Desired.TargetSnapshot {
			action = NoOp
		}
	}
	if req.Desired.Destroy {
		action = Delete
	}
	p := Plan{ProtocolVersion: ProtocolVersion, ResourceID: req.Desired.ID, Action: action, ArtifactDigest: req.Desired.ArtifactDigest, PolicyDigest: req.Desired.PolicyDigest, TargetSnapshot: req.Desired.TargetSnapshot, RequiresApply: action != NoOp, CreatedAt: time.Now().UTC()}
	p.PlanDigest = digest(p)
	return p, nil
}

func digest(p Plan) string {
	p.PlanDigest = ""
	b, _ := json.Marshal(p)
	h := sha256.Sum256(b)
	return "sha256:" + hex.EncodeToString(h[:])
}

type ApplyRequest struct {
	Plan         Plan
	Desired      ResourceConfig
	ApprovedPlan string
	Operator     string
}

type Executor interface {
	Apply(context.Context, deploy.Request) error
	Refresh(context.Context, string) (State, error)
}

func Apply(ctx context.Context, x Executor, req ApplyRequest, lock Locker) (State, error) {
	if x == nil || lock == nil || req.Operator == "" {
		return State{}, ErrInvalid
	}
	if err := req.Desired.Validate(); err != nil {
		return State{}, err
	}
	if req.Plan.PlanDigest == "" || digest(req.Plan) != req.Plan.PlanDigest || req.ApprovedPlan != req.Plan.PlanDigest {
		return State{}, ErrApproval
	}
	if req.Plan.ArtifactDigest != req.Desired.ArtifactDigest || req.Plan.PolicyDigest != req.Desired.PolicyDigest || req.Plan.TargetSnapshot != req.Desired.TargetSnapshot {
		return State{}, ErrArtifact
	}
	release, err := lock.Acquire(ctx, req.Desired.ID, req.Operator)
	if err != nil {
		return State{}, err
	}
	defer release()
	if req.Plan.Action != NoOp {
		action := deploy.ActionApply
		if req.Plan.Action == Delete {
			action = deploy.ActionDestroy
		}
		if err := x.Apply(ctx, deploy.Request{DeploymentID: req.Desired.ID, ArtifactDigest: req.Desired.ArtifactDigest, TargetSnapshot: req.Desired.TargetSnapshot, TargetID: req.Desired.TargetID, Environment: req.Desired.Environment, ConnectionRef: req.Desired.ConnectionRef, Action: action, DestroyApproved: req.Plan.Action == Delete}); err != nil {
			return State{}, err
		}
	}
	return State{ID: req.Desired.ID, Workflow: req.Desired.Workflow, SourceRef: req.Desired.SourceRef, ArtifactDigest: req.Desired.ArtifactDigest, PolicyDigest: req.Desired.PolicyDigest, TargetSnapshot: req.Desired.TargetSnapshot, TargetID: req.Desired.TargetID, Environment: req.Desired.Environment, ConnectionRef: req.Desired.ConnectionRef, AppliedAt: time.Now().UTC()}, nil
}

type Importer interface {
	Inspect(context.Context, string) (State, error)
}

func Import(ctx context.Context, in Importer, id, connectionRef string) (State, error) {
	if in == nil || id == "" {
		return State{}, ErrInvalid
	}
	if err := validateReference(connectionRef); err != nil {
		return State{}, err
	}
	s, err := in.Inspect(ctx, id)
	if err != nil {
		return State{}, err
	}
	s.ID = id
	s.ConnectionRef = connectionRef
	if err := s.Validate(); err != nil {
		return State{}, err
	}
	return s, nil
}

func Refresh(ctx context.Context, in Importer, current State) (State, error) {
	if err := current.Validate(); err != nil {
		return State{}, err
	}
	return Import(ctx, in, current.ID, current.ConnectionRef)
}

type Locker interface {
	Acquire(context.Context, string, string) (func(), error)
}

type MemoryLocker struct {
	mu   sync.Mutex
	held map[string]string
}

func NewMemoryLocker() *MemoryLocker { return &MemoryLocker{held: map[string]string{}} }
func (l *MemoryLocker) Acquire(ctx context.Context, id, owner string) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	if prior := l.held[id]; prior != "" && prior != owner {
		return nil, ErrStateConflict
	}
	l.held[id] = owner
	var once sync.Once
	return func() { once.Do(func() { l.mu.Lock(); delete(l.held, id); l.mu.Unlock() }) }, nil
}

type Attribute struct {
	Type        string
	Required    bool
	Sensitive   bool
	Description string
}
type ResourceSchema struct {
	Name       string
	Attributes map[string]Attribute
}

func Schema() []ResourceSchema {
	attrs := map[string]Attribute{
		"id": {Type: "string", Required: true}, "workflow": {Type: "string", Required: true}, "source_ref": {Type: "string", Required: true},
		"artifact_digest": {Type: "string", Required: true}, "policy_digest": {Type: "string", Required: true}, "target_snapshot": {Type: "string", Required: true},
		"target_id": {Type: "string", Required: true}, "environment": {Type: "string", Required: true}, "connection_ref": {Type: "string", Required: true, Sensitive: true},
	}
	return []ResourceSchema{{Name: "autosql_schema", Attributes: attrs}, {Name: "autosql_migration", Attributes: attrs}}
}
