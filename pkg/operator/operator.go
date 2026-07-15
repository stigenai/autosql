// Package operator provides the deterministic reconciliation core used by the
// Kubernetes controller. Kubernetes adapters only translate CRs to these
// types; no secret value is represented in a spec or status.
package operator

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sync"
	"time"
)

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type ResourceKind string

const (
	Declarative ResourceKind = "DeclarativeSchema"
	Versioned   ResourceKind = "VersionedMigration"
)

type Source struct {
	Inline         string           `json:"inline,omitempty" yaml:"inline,omitempty"`
	SecretRef      *SecretKeyRef    `json:"secretRef,omitempty" yaml:"secretRef,omitempty"`
	ConfigMapRef   *ConfigMapKeyRef `json:"configMapRef,omitempty" yaml:"configMapRef,omitempty"`
	URL            string           `json:"url,omitempty" yaml:"url,omitempty"`
	RegistryDigest string           `json:"registryDigest,omitempty" yaml:"registryDigest,omitempty"`
}
type SecretKeyRef struct {
	Name string `json:"name" yaml:"name"`
	Key  string `json:"key" yaml:"key"`
}
type ConfigMapKeyRef struct {
	Name string `json:"name" yaml:"name"`
	Key  string `json:"key" yaml:"key"`
}
type Spec struct {
	Kind            ResourceKind  `json:"kind" yaml:"kind"`
	Generation      int64         `json:"generation" yaml:"generation"`
	Source          Source        `json:"source" yaml:"source"`
	ArtifactDigest  string        `json:"artifactDigest,omitempty" yaml:"artifactDigest,omitempty"`
	DatabaseURL     *SecretKeyRef `json:"databaseURL,omitempty" yaml:"databaseURL,omitempty"`
	RequireApproval bool          `json:"requireApproval,omitempty" yaml:"requireApproval,omitempty"`
}
type ConditionType string

const (
	Planning ConditionType = "Planning"
	Approval ConditionType = "ApprovalRequired"
	Applying ConditionType = "Applying"
	Ready    ConditionType = "Ready"
	Failed   ConditionType = "Failed"
)

type Condition struct {
	Type               ConditionType `json:"type"`
	Status             string        `json:"status"`
	Reason             string        `json:"reason,omitempty"`
	Message            string        `json:"message,omitempty"`
	ObservedGeneration int64         `json:"observedGeneration"`
	LastTransitionTime time.Time     `json:"lastTransitionTime"`
}
type Status struct {
	Conditions         []Condition `json:"conditions,omitempty"`
	ObservedGeneration int64       `json:"observedGeneration"`
	AppliedDigest      string      `json:"appliedDigest,omitempty"`
	PlanDigest         string      `json:"planDigest,omitempty"`
	TargetIdentity     string      `json:"targetIdentity,omitempty"`
	OperationID        string      `json:"operationID,omitempty"`
	RecoveryState      string      `json:"recoveryState,omitempty"`
	ExecutionID        string      `json:"executionID,omitempty"`
	PendingStep        string      `json:"pendingStep,omitempty"`
	RecoveryGuidance   string      `json:"recoveryGuidance,omitempty"`
	AppliedSteps       int         `json:"appliedSteps,omitempty"`
	RetryCount         int         `json:"retryCount,omitempty"`
}
type Resource struct {
	Name   string
	Spec   Spec
	Status Status
	// ResolvedDatabaseURL is transient controller input. It is never written
	// to a record or Kubernetes status.
	ResolvedDatabaseURL string `json:"-"`
	ResolvedSource      string `json:"-"`
}
type Record struct {
	Status                  Status
	ApplyingKey, AppliedKey string
}
type Store interface {
	Load(string) (Record, bool)
	Save(string, Record) error
}
type MemoryStore struct {
	mu sync.Mutex
	m  map[string]Record
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{m: map[string]Record{}} }
func (s *MemoryStore) Load(k string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.m[k]
	return r, ok
}
func (s *MemoryStore) Save(k string, r Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.m[k] = r
	return nil
}

type Leader interface {
	Acquire(context.Context, string) (bool, error)
}
type AlwaysLeader struct{}

func (AlwaysLeader) Acquire(context.Context, string) (bool, error) { return true, nil }

// ApplyResult carries the non-secret execution identifiers that a controller
// can persist in status for recovery and audit correlation.
type ApplyResult struct {
	Status           string
	PlanDigest       string
	TargetIdentity   string
	ExecutionID      string
	PendingStep      string
	RecoveryGuidance string
	AppliedSteps     int
}

type ApplyFunc func(context.Context, Resource, string) (ApplyResult, error)
type Reconciler struct {
	Store  Store
	Leader Leader
	Apply  ApplyFunc
	Now    func() time.Time
	mu     sync.Mutex
}

func (r *Reconciler) Reconcile(ctx context.Context, obj Resource, approved bool) (Status, error) {
	if r == nil || r.Store == nil || r.Apply == nil || obj.Name == "" {
		return Status{}, errors.New("invalid reconciler or resource")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.Leader == nil {
		r.Leader = AlwaysLeader{}
	}
	if ok, e := r.Leader.Acquire(ctx, obj.Name); e != nil || !ok {
		return obj.Status, e
	}
	now := time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	key := applyKey(obj)
	old, found := r.Store.Load(obj.Name)
	if !found && obj.Status.ObservedGeneration == obj.Spec.Generation && obj.Status.AppliedDigest == obj.Spec.ArtifactDigest && obj.Status.RecoveryState == "none" {
		// Kubernetes status is the durable cross-pod checkpoint. Rehydrate a
		// fresh local store after pod replacement or leader movement so a
		// successfully applied generation is never executed twice.
		old = Record{Status: obj.Status, ApplyingKey: key, AppliedKey: key}
		_ = r.Store.Save(obj.Name, old)
	}
	st := old.Status
	if old.AppliedKey == key {
		return st, nil
	}
	if err := validate(obj.Spec); err != nil {
		st = condition(st, Failed, "Invalid", err.Error(), obj.Spec.Generation, now)
		_ = r.Store.Save(obj.Name, Record{Status: st})
		return st, err
	}
	st = condition(st, Planning, "PlanReady", "resource accepted", obj.Spec.Generation, now)
	if obj.Spec.RequireApproval && !approved {
		st = condition(st, Approval, "AwaitingApproval", "approval is required", obj.Spec.Generation, now)
		_ = r.Store.Save(obj.Name, Record{Status: st, ApplyingKey: key})
		return st, nil
	}
	st = condition(st, Applying, "ApplyStarted", "applying artifact", obj.Spec.Generation, now)
	st.OperationID = operationID(key)
	st.RecoveryState = "pending"
	_ = r.Store.Save(obj.Name, Record{Status: st, ApplyingKey: key})
	outcome, err := r.Apply(ctx, obj, obj.Spec.ArtifactDigest)
	st.PlanDigest = outcome.PlanDigest
	st.TargetIdentity = outcome.TargetIdentity
	st.ExecutionID = outcome.ExecutionID
	st.PendingStep = outcome.PendingStep
	st.RecoveryGuidance = outcome.RecoveryGuidance
	st.AppliedSteps = outcome.AppliedSteps
	if outcome.Status == "uncertain" {
		st.RecoveryState = "uncertain"
	}
	if err != nil {
		st.RetryCount++
		st = condition(st, Failed, "ApplyFailed", err.Error(), obj.Spec.Generation, now)
		_ = r.Store.Save(obj.Name, Record{Status: st, ApplyingKey: key})
		return st, err
	}
	st.ObservedGeneration = obj.Spec.Generation
	st.AppliedDigest = obj.Spec.ArtifactDigest
	if st.PlanDigest == "" {
		st.PlanDigest = obj.Spec.ArtifactDigest
	}
	st.RecoveryState = "none"
	st = condition(st, Ready, "Applied", "resource is converged", obj.Spec.Generation, now)
	_ = r.Store.Save(obj.Name, Record{Status: st, AppliedKey: key, ApplyingKey: key})
	return st, nil
}

func operationID(key string) string {
	digest := sha256.Sum256([]byte("autosql.operator.operation/v1\x00" + key))
	return "autosql-op-" + hex.EncodeToString(digest[:16])
}
func validate(s Spec) error {
	if s.Kind != Declarative && s.Kind != Versioned {
		return errors.New("unsupported resource kind")
	}
	n := 0
	if s.Source.Inline != "" {
		n++
	}
	if s.Source.SecretRef != nil {
		n++
	}
	if s.Source.ConfigMapRef != nil {
		n++
	}
	if s.Source.URL != "" {
		n++
	}
	if s.Source.RegistryDigest != "" {
		n++
	}
	if n != 1 {
		return errors.New("exactly one artifact source is required")
	}
	if s.Source.RegistryDigest != "" && !digestRE.MatchString(s.Source.RegistryDigest) {
		return errors.New("registryDigest must be a sha256 digest")
	}
	if s.DatabaseURL == nil || s.DatabaseURL.Name == "" || s.DatabaseURL.Key == "" {
		return errors.New("databaseURL secret reference is required")
	}
	if s.Generation < 1 {
		return errors.New("generation must be at least 1")
	}
	if s.ArtifactDigest == "" {
		return errors.New("operator migration requires an artifact digest")
	}
	if s.Source.RegistryDigest != "" && s.Source.RegistryDigest != s.ArtifactDigest {
		return errors.New("registryDigest must equal artifactDigest")
	}
	return nil
}

func applyKey(obj Resource) string {
	raw, _ := json.Marshal(struct {
		Kind           ResourceKind  `json:"kind"`
		Generation     int64         `json:"generation"`
		Source         Source        `json:"source"`
		ArtifactDigest string        `json:"artifact_digest"`
		DatabaseURL    *SecretKeyRef `json:"database_url"`
	}{obj.Spec.Kind, obj.Spec.Generation, obj.Spec.Source, obj.Spec.ArtifactDigest, obj.Spec.DatabaseURL})
	digest := sha256.Sum256(raw)
	return fmt.Sprintf("%s/%d/%s/%s", obj.Spec.Kind, obj.Spec.Generation, obj.Spec.ArtifactDigest, hex.EncodeToString(digest[:]))
}
func condition(s Status, typ ConditionType, reason, msg string, g int64, at time.Time) Status {
	for i := range s.Conditions {
		s.Conditions[i].Status = "False"
	}
	s.Conditions = append(s.Conditions, Condition{Type: typ, Status: "True", Reason: reason, Message: msg, ObservedGeneration: g, LastTransitionTime: at})
	return s
}
