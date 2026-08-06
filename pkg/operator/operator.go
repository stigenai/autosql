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
	"strings"
	"sync"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/bootstrap"
	"autosql/pkg/workloadidentity"
)

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

type ResourceKind string
type AdoptionPolicy string

const (
	Declarative ResourceKind = "DeclarativeSchema"
	Versioned   ResourceKind = "VersionedMigration"

	AdoptIfEquivalent AdoptionPolicy = "IfEquivalent"
)

type Source struct {
	Format         string           `json:"format,omitempty" yaml:"format,omitempty"`
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

// BootstrapAuthorizationRef contains only namespaced runtime references and
// trusted public identity policy. Signing keys and resolved Secret values are
// deliberately not representable.
type BootstrapAuthorizationRef struct {
	ManifestSecretRef  SecretKeyRef `json:"manifestSecretRef" yaml:"manifestSecretRef"`
	PublicKeySecretRef SecretKeyRef `json:"publicKeySecretRef" yaml:"publicKeySecretRef"`
	Issuer             string       `json:"issuer" yaml:"issuer"`
	Signer             string       `json:"signer" yaml:"signer"`
	Purpose            string       `json:"purpose" yaml:"purpose"`
}
type Spec struct {
	Kind                   ResourceKind               `json:"kind" yaml:"kind"`
	Generation             int64                      `json:"generation" yaml:"generation"`
	Suspend                bool                       `json:"suspend,omitempty" yaml:"suspend,omitempty"`
	Source                 Source                     `json:"source" yaml:"source"`
	ArtifactDigest         string                     `json:"artifactDigest,omitempty" yaml:"artifactDigest,omitempty"`
	DatabaseURL            *SecretKeyRef              `json:"databaseURL,omitempty" yaml:"databaseURL,omitempty"`
	DatabaseIdentity       *workloadidentity.Binding  `json:"databaseIdentity,omitempty" yaml:"databaseIdentity,omitempty"`
	MaintenanceDatabaseURL *SecretKeyRef              `json:"maintenanceDatabaseURL,omitempty" yaml:"maintenanceDatabaseURL,omitempty"`
	DatabaseTarget         *bootstrap.DatabaseTarget  `json:"databaseTarget,omitempty" yaml:"databaseTarget,omitempty"`
	BootstrapAuthority     *bootstrap.Contract        `json:"bootstrapAuthority,omitempty" yaml:"bootstrapAuthority,omitempty"`
	BootstrapAuthorization *BootstrapAuthorizationRef `json:"bootstrapAuthorization,omitempty" yaml:"bootstrapAuthorization,omitempty"`
	CreateDatabase         bool                       `json:"createDatabase,omitempty" yaml:"createDatabase,omitempty"`
	AdoptionPolicy         AdoptionPolicy             `json:"adoptionPolicy,omitempty" yaml:"adoptionPolicy,omitempty"`
	PostgresVersion        int                        `json:"postgresVersion,omitempty" yaml:"postgresVersion,omitempty"`
	ConcurrentIndexes      bool                       `json:"concurrentIndexes,omitempty" yaml:"concurrentIndexes,omitempty"`
	RequireApproval        bool                       `json:"requireApproval,omitempty" yaml:"requireApproval,omitempty"`
}
type ConditionType string

const (
	Planning      ConditionType = "Planning"
	Approval      ConditionType = "ApprovalRequired"
	Applying      ConditionType = "Applying"
	Ready         ConditionType = "Ready"
	Failed        ConditionType = "Failed"
	Authorization ConditionType = "BootstrapAuthorization"
)

type AuthorizationState string

const (
	AuthorizationMissing  AuthorizationState = "Missing"
	AuthorizationInvalid  AuthorizationState = "Invalid"
	AuthorizationStale    AuthorizationState = "Stale"
	AuthorizationAccepted AuthorizationState = "Accepted"
)

type AuthorizationError struct {
	State AuthorizationState
}

func (e *AuthorizationError) Error() string {
	switch e.State {
	case AuthorizationMissing:
		return "bootstrap authorization is required"
	case AuthorizationStale:
		return "bootstrap authorization is stale for the current plan"
	default:
		return "bootstrap authorization is invalid"
	}
}

type Condition struct {
	Type               ConditionType `json:"type"`
	Status             string        `json:"status"`
	Reason             string        `json:"reason,omitempty"`
	Message            string        `json:"message,omitempty"`
	ObservedGeneration int64         `json:"observedGeneration"`
	LastTransitionTime time.Time     `json:"lastTransitionTime"`
}
type Status struct {
	Conditions             []Condition `json:"conditions,omitempty"`
	ObservedGeneration     int64       `json:"observedGeneration"`
	AppliedDigest          string      `json:"appliedDigest,omitempty"`
	PlanDigest             string      `json:"planDigest,omitempty"`
	TargetIdentity         string      `json:"targetIdentity,omitempty"`
	OperationID            string      `json:"operationID,omitempty"`
	RecoveryState          string      `json:"recoveryState,omitempty"`
	ExecutionID            string      `json:"executionID,omitempty"`
	PendingStep            string      `json:"pendingStep,omitempty"`
	RecoveryGuidance       string      `json:"recoveryGuidance,omitempty"`
	AppliedSteps           int         `json:"appliedSteps,omitempty"`
	RetryCount             int         `json:"retryCount,omitempty"`
	AppliedFingerprint     string      `json:"appliedFingerprint,omitempty"`
	AuthorizationExpiresAt time.Time   `json:"authorizationExpiresAt,omitempty"`
}
type RuntimeReferenceBinding struct {
	ResourceVersion string `json:"resourceVersion,omitempty"`
	ContentDigest   string `json:"contentDigest,omitempty"`
}
type Resource struct {
	Name               string
	MetadataGeneration int64
	Spec               Spec
	Status             Status
	// ResolvedDatabaseURL is transient controller input. It is never written
	// to a record or Kubernetes status.
	ResolvedDatabaseURL            string                    `json:"-"`
	ResolvedMaintenanceDatabaseURL string                    `json:"-"`
	ResolvedSource                 string                    `json:"-"`
	ResolvedAuthorizationManifest  []byte                    `json:"-"`
	ResolvedAuthorizationPublicKey []byte                    `json:"-"`
	VerifiedReleaseArtifact        artifact.VerifiedArtifact `json:"-"`
	SourceBinding                  RuntimeReferenceBinding   `json:"-"`
	DatabaseBinding                RuntimeReferenceBinding   `json:"-"`
	MaintenanceDatabaseBinding     RuntimeReferenceBinding   `json:"-"`
	AuthorizationManifestBinding   RuntimeReferenceBinding   `json:"-"`
	AuthorizationPublicKeyBinding  RuntimeReferenceBinding   `json:"-"`
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
	Status                 string
	PlanDigest             string
	TargetIdentity         string
	ExecutionID            string
	PendingStep            string
	RecoveryGuidance       string
	AppliedSteps           int
	AuthorizationState     AuthorizationState
	SourceDigest           string
	AuthorizationExpiresAt time.Time
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
	old, _ := r.Store.Load(obj.Name)
	st := old.Status
	if obj.Spec.Suspend {
		st.ObservedGeneration = obj.Spec.Generation
		if obj.MetadataGeneration > 0 {
			st.ObservedGeneration = obj.MetadataGeneration
		}
		st = condition(st, Ready, "Suspended", "reconciliation is suspended", obj.Spec.Generation, now)
		_ = r.Store.Save(obj.Name, Record{Status: st, AppliedKey: old.AppliedKey, ApplyingKey: old.ApplyingKey})
		return st, nil
	}
	if old.AppliedKey == key && (st.AuthorizationExpiresAt.IsZero() || now.Before(st.AuthorizationExpiresAt)) {
		if len(st.Conditions) > 0 && st.Conditions[len(st.Conditions)-1].Type == Ready && st.Conditions[len(st.Conditions)-1].Reason == "Suspended" {
			reason, message := "Applied", "resource is converged"
			if obj.Spec.AdoptionPolicy == AdoptIfEquivalent {
				reason, message = "Adopted", "existing database matches desired schema; no SQL executed"
			}
			st = condition(st, Ready, reason, message, obj.Spec.Generation, now)
			_ = r.Store.Save(obj.Name, Record{Status: st, AppliedKey: old.AppliedKey, ApplyingKey: old.ApplyingKey})
		}
		return st, nil
	}
	if err := validate(obj.Spec); err != nil {
		var authorizationErr *AuthorizationError
		if errors.As(err, &authorizationErr) {
			st = condition(st, Authorization, string(authorizationErr.State), authorizationErr.Error(), obj.Spec.Generation, now)
		} else {
			st = condition(st, Failed, "Invalid", err.Error(), obj.Spec.Generation, now)
		}
		_ = r.Store.Save(obj.Name, Record{Status: st})
		return st, err
	}
	st = condition(st, Planning, "PlanReady", "resource accepted", obj.Spec.Generation, now)
	if obj.Spec.RequireApproval && !approved {
		st = condition(st, Approval, "AwaitingApproval", "approval is required", obj.Spec.Generation, now)
		_ = r.Store.Save(obj.Name, Record{Status: st, ApplyingKey: key})
		return st, nil
	}
	applyingReason, applyingMessage := "ApplyStarted", "applying artifact"
	if obj.Spec.AdoptionPolicy == AdoptIfEquivalent {
		applyingReason, applyingMessage = "AdoptionStarted", "verifying existing database against desired schema"
	}
	st = condition(st, Applying, applyingReason, applyingMessage, obj.Spec.Generation, now)
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
	if outcome.AuthorizationState == AuthorizationAccepted {
		st = condition(st, Authorization, string(AuthorizationAccepted), "bootstrap authorization accepted", obj.Spec.Generation, now)
	}
	if err != nil {
		var authorizationErr *AuthorizationError
		if errors.As(err, &authorizationErr) {
			st = condition(st, Authorization, string(authorizationErr.State), authorizationErr.Error(), obj.Spec.Generation, now)
			_ = r.Store.Save(obj.Name, Record{Status: st, ApplyingKey: key})
			return st, err
		}
		st.RetryCount++
		st = condition(st, Failed, "ApplyFailed", err.Error(), obj.Spec.Generation, now)
		_ = r.Store.Save(obj.Name, Record{Status: st, ApplyingKey: key})
		return st, err
	}
	st.ObservedGeneration = obj.Spec.Generation
	if obj.MetadataGeneration > 0 {
		st.ObservedGeneration = obj.MetadataGeneration
	}
	st.AppliedDigest = obj.Spec.ArtifactDigest
	if st.PlanDigest == "" {
		st.PlanDigest = obj.Spec.ArtifactDigest
	}
	st.RecoveryState = "none"
	st.AuthorizationExpiresAt = outcome.AuthorizationExpiresAt.UTC()
	st.AppliedFingerprint = appliedFingerprint(obj, outcome)
	readyReason, readyMessage := "Applied", "resource is converged"
	if obj.Spec.AdoptionPolicy == AdoptIfEquivalent {
		readyReason, readyMessage = "Adopted", "existing database matches desired schema; no SQL executed"
	}
	st = condition(st, Ready, readyReason, readyMessage, obj.Spec.Generation, now)
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
	if s.Source.Format != "" && s.Source.Format != "sql" && s.Source.Format != "hcl" {
		return errors.New("source format must be sql or hcl")
	}
	if s.Source.Format != "" && s.Source.Inline == "" && s.Source.SecretRef == nil && s.Source.ConfigMapRef == nil {
		return errors.New("source format applies only to inline, Secret, or ConfigMap content")
	}
	if s.AdoptionPolicy != "" {
		if s.AdoptionPolicy != AdoptIfEquivalent {
			return errors.New("adoptionPolicy must be IfEquivalent")
		}
		if s.Kind != Declarative {
			return errors.New("adoptionPolicy requires DeclarativeSchema")
		}
		if s.Source.Inline == "" && s.Source.SecretRef == nil && s.Source.ConfigMapRef == nil {
			return errors.New("adoptionPolicy requires an inline, Secret, or ConfigMap desired source")
		}
		if s.CreateDatabase || s.DatabaseTarget != nil || s.MaintenanceDatabaseURL != nil || s.BootstrapAuthority != nil || s.BootstrapAuthorization != nil {
			return errors.New("adoptionPolicy cannot create or bootstrap a database")
		}
	}
	if s.Source.RegistryDigest != "" && !digestRE.MatchString(s.Source.RegistryDigest) {
		return errors.New("registryDigest must be a sha256 digest")
	}
	if (s.DatabaseURL == nil) == (s.DatabaseIdentity == nil) {
		return errors.New("exactly one of databaseURL or databaseIdentity is required")
	}
	if s.DatabaseURL != nil && (s.DatabaseURL.Name == "" || s.DatabaseURL.Key == "") {
		return errors.New("databaseURL secret reference is incomplete")
	}
	if s.DatabaseIdentity != nil {
		if err := s.DatabaseIdentity.Validate(); err != nil {
			return fmt.Errorf("invalid databaseIdentity: %w", err)
		}
	}
	if s.CreateDatabase && s.BootstrapAuthority == nil {
		return errors.New("createDatabase requires bootstrapAuthority")
	}
	if s.PostgresVersion != 0 && (s.PostgresVersion < 14 || s.PostgresVersion > 18) {
		return errors.New("postgresVersion must be between 14 and 18")
	}
	if s.DatabaseTarget != nil {
		target := s.DatabaseTarget.Normalize()
		if err := target.Validate(); err != nil {
			return fmt.Errorf("invalid databaseTarget: %w", err)
		}
		if target.Mode == bootstrap.ManagedDatabase {
			if s.BootstrapAuthority == nil {
				return errors.New("managed databaseTarget requires bootstrapAuthority")
			}
			if s.MaintenanceDatabaseURL == nil || s.MaintenanceDatabaseURL.Name == "" || s.MaintenanceDatabaseURL.Key == "" {
				return errors.New("managed databaseTarget requires maintenanceDatabaseURL")
			}
		}
		if s.CreateDatabase && target.Mode != bootstrap.ManagedDatabase {
			return errors.New("createDatabase conflicts with external databaseTarget mode")
		}
	}
	if s.BootstrapAuthority != nil {
		if _, err := s.BootstrapAuthority.Validate(nil); err != nil {
			return fmt.Errorf("invalid bootstrapAuthority: %w", err)
		}
	}
	if ref := s.BootstrapAuthorization; ref != nil {
		if !exactNonEmpty(ref.ManifestSecretRef.Name, ref.ManifestSecretRef.Key, ref.PublicKeySecretRef.Name, ref.PublicKeySecretRef.Key, ref.Issuer, ref.Signer, ref.Purpose) {
			return &AuthorizationError{State: AuthorizationInvalid}
		}
		if s.Kind != Declarative || s.DatabaseTarget == nil || (s.Source.Inline == "" && s.Source.SecretRef == nil && s.Source.ConfigMapRef == nil) {
			return &AuthorizationError{State: AuthorizationInvalid}
		}
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

func exactNonEmpty(values ...string) bool {
	for _, value := range values {
		if value == "" || value != strings.TrimSpace(value) {
			return false
		}
	}
	return true
}

func applyKey(obj Resource) string {
	raw, _ := json.Marshal(struct {
		Kind                   ResourceKind               `json:"kind"`
		Generation             int64                      `json:"generation"`
		Source                 Source                     `json:"source"`
		ArtifactDigest         string                     `json:"artifact_digest"`
		DatabaseURL            *SecretKeyRef              `json:"database_url"`
		DatabaseIdentity       *workloadidentity.Binding  `json:"database_identity,omitempty"`
		MaintenanceDatabaseURL *SecretKeyRef              `json:"maintenance_database_url,omitempty"`
		DatabaseTarget         *bootstrap.DatabaseTarget  `json:"database_target,omitempty"`
		BootstrapAuthority     *bootstrap.Contract        `json:"bootstrap_authority,omitempty"`
		BootstrapAuthorization *BootstrapAuthorizationRef `json:"bootstrap_authorization,omitempty"`
		CreateDatabase         bool                       `json:"create_database,omitempty"`
		AdoptionPolicy         AdoptionPolicy             `json:"adoption_policy,omitempty"`
		PostgresVersion        int                        `json:"postgres_version,omitempty"`
		ConcurrentIndexes      bool                       `json:"concurrent_indexes,omitempty"`
		MetadataGeneration     int64                      `json:"metadata_generation"`
		SourceBinding          RuntimeReferenceBinding    `json:"source_binding,omitempty"`
		DatabaseBinding        RuntimeReferenceBinding    `json:"database_binding,omitempty"`
		MaintenanceBinding     RuntimeReferenceBinding    `json:"maintenance_binding,omitempty"`
		ManifestBinding        RuntimeReferenceBinding    `json:"manifest_binding,omitempty"`
		PublicKeyBinding       RuntimeReferenceBinding    `json:"public_key_binding,omitempty"`
	}{obj.Spec.Kind, obj.Spec.Generation, obj.Spec.Source, obj.Spec.ArtifactDigest, obj.Spec.DatabaseURL, obj.Spec.DatabaseIdentity, obj.Spec.MaintenanceDatabaseURL, obj.Spec.DatabaseTarget, obj.Spec.BootstrapAuthority, obj.Spec.BootstrapAuthorization, obj.Spec.CreateDatabase, obj.Spec.AdoptionPolicy, obj.Spec.PostgresVersion, obj.Spec.ConcurrentIndexes, obj.MetadataGeneration, obj.SourceBinding, obj.DatabaseBinding, obj.MaintenanceDatabaseBinding, obj.AuthorizationManifestBinding, obj.AuthorizationPublicKeyBinding})
	digest := sha256.Sum256(raw)
	return fmt.Sprintf("%s/%d/%s/%s", obj.Spec.Kind, obj.Spec.Generation, obj.Spec.ArtifactDigest, hex.EncodeToString(digest[:]))
}

func appliedFingerprint(obj Resource, outcome ApplyResult) string {
	raw, _ := json.Marshal(struct {
		ApplyKey     string `json:"apply_key"`
		PlanDigest   string `json:"plan_digest"`
		SourceDigest string `json:"source_digest"`
	}{applyKey(obj), outcome.PlanDigest, outcome.SourceDigest})
	digest := sha256.Sum256(raw)
	return "sha256:" + hex.EncodeToString(digest[:])
}

// UpdateCondition applies the operator's independent lifecycle/authorization
// condition semantics. Adapters use it for failures that happen before the
// reconciliation core, preserving the existing condition set.
func UpdateCondition(status Status, typ ConditionType, reason, message string, generation int64, at time.Time) Status {
	return condition(status, typ, reason, message, generation, at)
}
func condition(s Status, typ ConditionType, reason, msg string, g int64, at time.Time) Status {
	next := Condition{Type: typ, Status: "True", Reason: reason, Message: msg, ObservedGeneration: g, LastTransitionTime: at}
	conditions := make([]Condition, 0, len(s.Conditions)+1)
	for i := range s.Conditions {
		current := s.Conditions[i]
		if current.Type == typ {
			if current.Status == next.Status && current.Reason == next.Reason && current.Message == next.Message && current.ObservedGeneration == next.ObservedGeneration {
				next.LastTransitionTime = current.LastTransitionTime
			}
			continue
		}
		if typ != Authorization && current.Type != Authorization {
			current.Status = "False"
		}
		conditions = append(conditions, current)
	}
	s.Conditions = append(conditions, next)
	return s
}
