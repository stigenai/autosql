// Package operator provides the deterministic reconciliation core used by the
// Kubernetes controller. Kubernetes adapters only translate CRs to these
// types; no secret value is represented in a spec or status.
package operator

import (
	"context"
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
	RetryCount         int         `json:"retryCount,omitempty"`
}
type Resource struct {
	Name   string
	Spec   Spec
	Status Status
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

type ApplyFunc func(context.Context, Resource, string) error
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
	old, _ := r.Store.Load(obj.Name)
	st := old.Status
	key := fmt.Sprintf("%s/%d/%s", obj.Spec.Kind, obj.Spec.Generation, obj.Spec.ArtifactDigest)
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
	_ = r.Store.Save(obj.Name, Record{Status: st, ApplyingKey: key})
	if err := r.Apply(ctx, obj, obj.Spec.ArtifactDigest); err != nil {
		st.RetryCount++
		st = condition(st, Failed, "ApplyFailed", err.Error(), obj.Spec.Generation, now)
		_ = r.Store.Save(obj.Name, Record{Status: st, ApplyingKey: key})
		return st, err
	}
	st.ObservedGeneration = obj.Spec.Generation
	st.AppliedDigest = obj.Spec.ArtifactDigest
	st = condition(st, Ready, "Applied", "resource is converged", obj.Spec.Generation, now)
	_ = r.Store.Save(obj.Name, Record{Status: st, AppliedKey: key, ApplyingKey: key})
	return st, nil
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
	return nil
}
func condition(s Status, typ ConditionType, reason, msg string, g int64, at time.Time) Status {
	for i := range s.Conditions {
		s.Conditions[i].Status = "False"
	}
	s.Conditions = append(s.Conditions, Condition{Type: typ, Status: "True", Reason: reason, Message: msg, ObservedGeneration: g, LastTransitionTime: at})
	return s
}
