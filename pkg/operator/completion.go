package operator

import (
	"context"
	"errors"
	"time"

	"autosql/pkg/executor"
)

// CompletionFunc must authenticate current release, source, target, and
// approval bindings before inspecting execution evidence. It must never call
// Apply. AuthorizationExpiresAt bounds reuse of its successful checkpoint.
type CompletionFunc func(context.Context, Resource) CompletionResult

// ExecutionProbe distinguishes unstarted work without granting execution or completion authority.
type ExecutionProbe func(context.Context, Resource) executor.ExecutionStartCheck

type CompletionResult struct {
	EventID                string
	Evidence               executor.CompletionEvidence
	AuthorizationExpiresAt time.Time
}

// CompletionError carries only bounded diagnostics suitable for public status.
type CompletionError struct {
	Reason    string
	Retryable bool
}

func (e *CompletionError) Error() string { return e.Reason }

func ordinaryDeclarative(obj Resource) bool {
	s := obj.Spec
	return s.Kind == Declarative && s.AdoptionPolicy == "" && !s.CreateDatabase && s.DatabaseTarget == nil && s.MaintenanceDatabaseURL == nil && s.BootstrapAuthority == nil && s.BootstrapAuthorization == nil
}

func (r *Reconciler) recoverCompletion(ctx context.Context, obj Resource, old Record, now time.Time) (Status, error) {
	result := CompletionResult{Evidence: executor.CompletionEvidence{Outcome: executor.CompletionIneligible, Reason: "unsupported_plan"}}
	if ordinaryDeclarative(obj) && r.Observe != nil {
		result = r.Observe(ctx, obj)
	}
	// Observation may take time; use a fresh instant for its authorization and
	// timestamp checks, rather than the instant preceding database inspection.
	now = time.Now().UTC()
	if r.Now != nil {
		now = r.Now().UTC()
	}
	evidence := result.Evidence
	if evidence.Outcome == executor.CompletionProven && !validCompletion(obj, result, now) {
		evidence = executor.CompletionEvidence{Outcome: executor.CompletionRefused, Reason: "authorization_invalid"}
	}
	if evidence.Outcome != executor.CompletionProven {
		return r.completionFailure(obj, old, evidence, now)
	}
	status := old.Status
	status.ObservedGeneration = resourceGeneration(obj)
	status.AppliedDigest = evidence.ArtifactDigest
	status.PlanDigest = evidence.PlanDigest
	status.TargetIdentity = evidence.TargetIdentity
	status.OperationID = operationID(applyKey(obj))
	status.ExecutionID = evidence.ArtifactDigest
	status.RecoveryState = "none"
	status.PendingStep, status.RecoveryGuidance = "", ""
	status.AppliedSteps = evidence.ConfirmedSteps
	status.AppliedFingerprint = evidence.Fingerprint
	status.CompletionObservedAt = evidence.ObservedAt.UTC()
	status.AuthorizationExpiresAt = result.AuthorizationExpiresAt.UTC()
	status = condition(status, Ready, "CompletionRecovered", "completed execution verified; no SQL executed", resourceGeneration(obj), now)
	return r.saveCheckpoint(obj, Record{Status: status, AppliedKey: applyKey(obj), ApplyingKey: applyKey(obj)}, old, PersistenceRecovery, now)
}

func validCompletion(obj Resource, result CompletionResult, now time.Time) bool {
	e := result.Evidence
	return e.ArtifactDigest == obj.Spec.ArtifactDigest && e.PlanDigest != "" && e.TargetIdentity != "" && e.Environment != "" && e.Fingerprint != "" && e.Attempt > 0 && e.ConfirmedSteps > 0 && !e.ObservedAt.IsZero() && !e.ObservedAt.After(now) && result.AuthorizationExpiresAt.After(now)
}

func completionDiagnostic(e executor.CompletionEvidence) (string, string) {
	switch e.Reason {
	case "unsupported_plan":
		return "ArtifactExpired", "publish a fresh approved release or use the documented recovery procedure"
	case "authorization_invalid":
		return "ReleaseAuthorizationInvalid", "correct release authorization or publish a fresh approved release"
	case "state_drifted":
		return "RecoveryStateDrifted", "investigate managed schema drift and publish a fresh approved plan"
	case "audit_invalid", "audit_incomplete", "audit_unavailable", "evidence_reader_missing":
		return "RecoveryAuditUnavailable", "restore durable audit evidence; do not replay SQL to repair bookkeeping"
	}
	if e.Outcome == executor.CompletionRetryable {
		return "DependencyUnavailable", "restore the observation dependency; completion verification will retry"
	}
	return "RecoveryEvidenceInvalid", "inspect execution history and use the documented recovery procedure"
}

func (r *Reconciler) completionFailure(obj Resource, old Record, evidence executor.CompletionEvidence, now time.Time) (Status, error) {
	reason, guidance := completionDiagnostic(evidence)
	status := old.Status
	status.RetryCount++
	status.RecoveryGuidance = guidance
	status = condition(status, Failed, reason, guidance, resourceGeneration(obj), now)
	saved, saveErr := r.saveCheckpoint(obj, Record{Status: status, AppliedKey: old.AppliedKey, ApplyingKey: old.ApplyingKey}, old, PersistenceRecovery, now)
	return saved, errors.Join(&CompletionError{Reason: reason, Retryable: evidence.Outcome == executor.CompletionRetryable}, saveErr)
}
