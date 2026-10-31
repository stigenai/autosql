package operator

import "time"

type PersistenceStage string

const (
	PersistencePreApply  PersistenceStage = "pre_apply"
	PersistencePostApply PersistenceStage = "post_apply"
	PersistenceRecovery  PersistenceStage = "recovery"
)

// StatePersistenceError exposes a bounded stage without leaking a filesystem
// path, secret, or arbitrary backend error into controller status and logs.
type StatePersistenceError struct{ Stage PersistenceStage }

func (*StatePersistenceError) Error() string { return "operator state persistence failed" }

func resourceGeneration(obj Resource) int64 {
	if obj.MetadataGeneration > 0 {
		return obj.MetadataGeneration
	}
	return obj.Spec.Generation
}

func (r *Reconciler) saveCheckpoint(obj Resource, candidate, previous Record, stage PersistenceStage, now time.Time) (Status, error) {
	if err := r.Store.Save(obj.Name, candidate); err == nil {
		return candidate.Status, nil
	}
	// These fields describe successful completion. A failed write cannot
	// advance them, even when SQL succeeded and its execution IDs are known.
	status := candidate.Status
	status.ObservedGeneration = previous.Status.ObservedGeneration
	status.AppliedDigest = previous.Status.AppliedDigest
	status.AppliedFingerprint = previous.Status.AppliedFingerprint
	status.AuthorizationExpiresAt = previous.Status.AuthorizationExpiresAt
	status.CompletionObservedAt = previous.Status.CompletionObservedAt
	if stage == PersistencePostApply || stage == PersistenceRecovery {
		status.RecoveryState = "pending"
	}
	status.RetryCount++
	status.RecoveryGuidance = "restore writable operator state storage; retry completion observation"
	status = condition(status, Failed, "StatePersistenceFailed", status.RecoveryGuidance, resourceGeneration(obj), now)
	return status, &StatePersistenceError{Stage: stage}
}
