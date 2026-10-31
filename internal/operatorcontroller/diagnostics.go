package operatorcontroller

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math/rand/v2"
	"sync"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/operator"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/metrics"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

const deterministicRecheck = 5 * time.Minute

var diagnosticReasons = []string{
	"Applied", "Adopted", "CompletionRecovered", "Suspended", "AwaitingApproval",
	"ArtifactExpired", "RecoveryEvidenceInvalid", "RecoveryStateDrifted", "RecoveryAuditUnavailable",
	"StatePersistenceFailed", "ReleaseAuthorizationInvalid", "DependencyUnavailable", "StatusPublicationFailed",
	"Invalid", "ApplyFailed", "Missing", "Stale", "Accepted", "InputsChanged", "NotFound", "other",
}

type operatorMetrics struct {
	reconciliations *prometheus.CounterVec
	duration        *prometheus.HistogramVec
	observations    *prometheus.CounterVec
	persistence     *prometheus.CounterVec
}

func newOperatorMetrics(reg prometheus.Registerer) *operatorMetrics {
	m := &operatorMetrics{
		reconciliations: prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autosql_operator_reconciliations_total", Help: "Reconciliation results, including failures that could not be published."}, []string{"outcome", "reason"}),
		duration:        prometheus.NewHistogramVec(prometheus.HistogramOpts{Name: "autosql_operator_reconcile_duration_seconds", Help: "Time spent reconciling a resource.", Buckets: prometheus.DefBuckets}, []string{"outcome"}),
		observations:    prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autosql_operator_completion_observations_total", Help: "Completion observation outcomes; not SQL executions."}, []string{"outcome"}),
		persistence:     prometheus.NewCounterVec(prometheus.CounterOpts{Name: "autosql_operator_state_persistence_failures_total", Help: "Failed checkpoint writes by required persistence stage."}, []string{"stage"}),
	}
	reg.MustRegister(m.reconciliations, m.duration, m.observations, m.persistence)
	for _, outcome := range []string{"success", "failure", "suspended"} {
		m.duration.WithLabelValues(outcome)
		for _, reason := range diagnosticReasons {
			m.reconciliations.WithLabelValues(outcome, reason).Add(0)
		}
	}
	for _, outcome := range []string{"recovered", "ineligible", "refused", "retryable"} {
		m.observations.WithLabelValues(outcome).Add(0)
	}
	for _, stage := range []string{"pre_apply", "post_apply", "recovery"} {
		m.persistence.WithLabelValues(stage).Add(0)
	}
	return m
}

var processMetrics = sync.OnceValue(func() *operatorMetrics { return newOperatorMetrics(metrics.Registry) })

type retryState struct {
	uid        string
	revision   uint64
	input      string
	failures   int
	cumulative int
	next       time.Time
	reason     string
	outcome    string
}

type warningState struct {
	identity string
	at       time.Time
}

type operatorDiagnostics struct {
	mu       sync.Mutex
	metrics  *operatorMetrics
	retries  map[types.NamespacedName]retryState
	warnings map[types.NamespacedName]warningState
}

func (c *Controller) signals() *operatorDiagnostics {
	c.diagnosticsMu.Lock()
	defer c.diagnosticsMu.Unlock()
	if c.diagnostics == nil {
		c.diagnostics = &operatorDiagnostics{metrics: processMetrics(), retries: map[types.NamespacedName]retryState{}, warnings: map[types.NamespacedName]warningState{}}
	}
	return c.diagnostics
}

func (c *Controller) now() time.Time {
	if c.Now != nil {
		return c.Now().UTC()
	}
	return time.Now().UTC()
}

type dependencyError struct{ category string }

func (e *dependencyError) Error() string { return e.category }

type reconcileReport struct {
	managed     bool
	phase       string
	status      operator.Status
	coreError   error
	observation *operator.CompletionResult
}

func (r *reconcileReport) observer(observe operator.CompletionFunc) operator.CompletionFunc {
	if observe == nil {
		return nil
	}
	return func(ctx context.Context, resource operator.Resource) operator.CompletionResult {
		result := observe(ctx, resource)
		r.observation = &result
		return result
	}
}

// reconcileManaged owns scheduling and public diagnostics. Reconcile remains a
// single-attempt adapter for embedded callers. Only this boundary is registered
// with controller-runtime: raw dependency errors never reach its error logger.
func (c *Controller) reconcileManaged(ctx context.Context, req reconcile.Request) (reconcile.Result, error) {
	started := time.Now()
	d := c.signals()
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(GroupVersionKind)
	report := &reconcileReport{managed: true, phase: "read"}
	err := c.Get(ctx, req.NamespacedName, obj)
	if client.IgnoreNotFound(err) == nil && err != nil {
		d.mu.Lock()
		delete(d.retries, req.NamespacedName)
		delete(d.warnings, req.NamespacedName)
		d.mu.Unlock()
		return reconcile.Result{}, nil
	}
	logger := ctrl.LoggerFrom(ctx).WithValues("namespace", req.Namespace, "name", req.Name, "generation", obj.GetGeneration(), "release_digest", releaseLogDigest(obj))
	logger.Info("reconciliation started")
	input := diagnosticInput(obj)
	now := c.now()
	d.mu.Lock()
	previous := d.retries[req.NamespacedName]
	d.mu.Unlock()
	if err != nil {
		input = previous.input
	}
	// Watch changes bypass the delay. Status-only updates and stale delayed queue
	// entries cannot turn deterministic refusals into frequent database probes.
	if err == nil && previous.input == input && now.Before(previous.next) {
		logger.Info("reconciliation result", "outcome", previous.outcome, "reason", previous.reason, "duration_seconds", time.Since(started).Seconds(), "deferred", true)
		return reconcile.Result{RequeueAfter: previous.next.Sub(now)}, nil
	}
	if err == nil && previous.uid != string(obj.GetUID()) {
		previous = retryState{revision: previous.revision}
	}
	if previous.input != input {
		previous.failures = 0
	}
	var result reconcile.Result
	if err == nil {
		status := statusFromObject(obj)
		if previous.cumulative > status.RetryCount {
			status.RetryCount = previous.cumulative
			_ = unstructured.SetNestedField(obj.Object, int64(status.RetryCount), "status", "retryCount")
		}
		report.status = status
		result, err = c.reconcileObject(ctx, obj, report)
	}
	// writeFailure publishes directly; preserve its safely bounded status too.
	if s := statusFromObject(obj); s.RetryCount > report.status.RetryCount {
		report.status = s
	}
	outcome, reason, deterministic := reconcileDiagnostic(report, err, result)
	c.emitAttempt(logger, obj, report, err, outcome, reason, deterministic, time.Since(started))
	result = c.scheduleAttempt(req.NamespacedName, obj, report, previous, result, outcome, reason, deterministic, now)

	if ctx.Err() != nil {
		return reconcile.Result{}, nil
	}
	return result, nil
}

func transientDelay(failures int, jitter float64) time.Duration {
	base := time.Second << min(max(failures-1, 0), 6)
	// Jitter is positive, bounded to 10%, and never exceeds the absolute cap.
	return min(60*time.Second, base+time.Duration(float64(base)*0.1*max(0, min(1, jitter))))
}

func reconcileDiagnostic(r *reconcileReport, err error, result reconcile.Result) (outcome, reason string, deterministic bool) {
	if err != nil {
		if reason, deterministic := errorDiagnostic(err); reason != "" {
			return "failure", reason, deterministic
		}
		if reason, deterministic := phaseDiagnostic(r.phase); reason != "" {
			return "failure", reason, deterministic
		}
		condition := activeCondition(r.status)
		if condition.Type == operator.Failed {
			return "failure", boundedReason(condition.Reason), deterministicReason(condition.Reason)
		}
		return "failure", "ApplyFailed", false
	}
	if result.Requeue {
		return "success", "InputsChanged", false
	}
	condition := activeCondition(r.status)
	switch condition.Type {
	case operator.Failed:
		return "failure", boundedReason(condition.Reason), deterministicReason(condition.Reason)
	case operator.Ready:
		if condition.Reason == "Suspended" {
			return "suspended", "Suspended", false
		}
		return "success", boundedReason(condition.Reason), false
	case operator.Approval:
		return "success", "AwaitingApproval", false
	default:
		return "success", "other", false
	}
}

func errorDiagnostic(err error) (reason string, deterministic bool) {
	var publication *statusPublicationError
	var persistence *operator.StatePersistenceError
	var completion *operator.CompletionError
	var authorization *operator.AuthorizationError
	var dependency *dependencyError
	switch {
	case errors.As(err, &publication):
		return "StatusPublicationFailed", false
	case errors.As(err, &persistence):
		return "StatePersistenceFailed", false
	case errors.As(err, &completion):
		return boundedReason(completion.Reason), !completion.Retryable
	case errors.As(err, &authorization):
		return authorization.Reason(), true
	case errors.Is(err, artifact.ErrExpired):
		return "ArtifactExpired", true
	case errors.As(err, &dependency):
		return "DependencyUnavailable", false
	default:
		return "", false
	}
}

func phaseDiagnostic(phase string) (reason string, deterministic bool) {
	switch phase {
	case "status":
		return "StatusPublicationFailed", false
	case "read":
		return "DependencyUnavailable", false
	case "release":
		return "ReleaseAuthorizationInvalid", true
	case "input", "references":
		return "Invalid", true
	default:
		return "", false
	}
}

func activeCondition(status operator.Status) operator.Condition {
	for i := len(status.Conditions) - 1; i >= 0; i-- {
		c := status.Conditions[i]
		if c.Status == "True" && c.Type != operator.Authorization {
			return c
		}
	}
	return operator.Condition{}
}

func deterministicReason(reason string) bool {
	return reason != "ApplyFailed" && reason != "StatePersistenceFailed" && reason != "DependencyUnavailable" && reason != "StatusPublicationFailed"
}

func boundedReason(reason string) string {
	for _, allowed := range diagnosticReasons {
		if reason == allowed {
			return reason
		}
	}
	return "other"
}

func observationOutcome(outcome executor.CompletionOutcome) string {
	switch outcome {
	case executor.CompletionProven:
		return "recovered"
	case executor.CompletionIneligible:
		return "ineligible"
	case executor.CompletionRetryable:
		return "retryable"
	default:
		return "refused"
	}
}

func diagnosticGuidance(reason string) string {
	switch reason {
	case "ArtifactExpired", "ReleaseAuthorizationInvalid", "Missing", "Stale":
		return "correct authorization or publish a fresh approved release"
	case "RecoveryStateDrifted":
		return "investigate managed schema drift and publish a fresh approved plan"
	case "RecoveryEvidenceInvalid":
		return "inspect execution history and use the documented recovery procedure"
	case "RecoveryAuditUnavailable":
		return "restore durable audit evidence; do not replay SQL to repair bookkeeping"
	case "StatePersistenceFailed":
		return "restore writable operator state storage; retry completion observation"
	case "StatusPublicationFailed":
		return "restore Kubernetes status access; completion will be verified before publication"
	case "DependencyUnavailable":
		return "restore the referenced dependency; bounded retries continue"
	case "Invalid":
		return "correct the resource configuration"
	default:
		return "inspect operator diagnostics and execution evidence before retrying"
	}
}

func safeDigest(value string) string {
	if len(value) != 71 || value[:7] != "sha256:" {
		return ""
	}
	if _, err := hex.DecodeString(value[7:]); err != nil {
		return ""
	}
	return value
}
func releaseLogDigest(obj *unstructured.Unstructured) string {
	value, _, _ := unstructured.NestedString(obj.Object, "spec", "artifactDigest")
	return safeDigest(value)
}

func diagnosticInput(obj *unstructured.Unstructured) string {
	value, _ := json.Marshal([]any{obj.GetUID(), obj.GetGeneration(), obj.Object["spec"], obj.GetAnnotations()})
	digest := sha256.Sum256(value)
	return hex.EncodeToString(digest[:])
}

// Reference handlers invalidate only the affected resources. Values are never
// copied into retry keys, logs, metrics or events.
func (c *Controller) invalidateRetries(requests []reconcile.Request) {
	d := c.signals()
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, request := range requests {
		state := d.retries[request.NamespacedName]
		state.next = time.Time{}
		state.failures = 0
		state.revision++
		d.retries[request.NamespacedName] = state
	}
}

func (c *Controller) recordBoundedEvent(obj *unstructured.Unstructured, eventType, reason, message string) {
	if c.Recorder == nil {
		return
	}
	d := c.signals()
	key := client.ObjectKeyFromObject(obj)
	now := c.now()
	identity := string(obj.GetUID()) + "/" + diagnosticInput(obj) + "/" + reason + "/" + message
	d.mu.Lock()
	previous := d.warnings[key]
	if previous.identity == identity && now.Sub(previous.at) < deterministicRecheck {
		d.mu.Unlock()
		return
	}
	d.warnings[key] = warningState{identity: identity, at: now}
	d.mu.Unlock()
	c.Recorder.Event(obj, eventType, reason, message)
}

func observationCategory(reason string) string {
	switch reason {
	case "authorization_invalid", "unsupported_plan", "evidence_reader_missing", "database_unavailable", "target_busy", "state_unavailable", "history_invalid", "audit_invalid", "audit_unavailable", "audit_incomplete", "state_drifted":
		return " (" + reason + ")"
	default:
		return ""
	}
}

type statusPublicationError struct{ cause error }

func (*statusPublicationError) Error() string   { return "operator status publication failed" }
func (e *statusPublicationError) Unwrap() error { return e.cause }

func (c *Controller) emitAttempt(logger logr.Logger, obj *unstructured.Unstructured, report *reconcileReport, err error, outcome, reason string, deterministic bool, duration time.Duration) {
	d := c.signals()
	d.metrics.reconciliations.WithLabelValues(outcome, reason).Inc()
	d.metrics.duration.WithLabelValues(outcome).Observe(duration.Seconds())
	var persistence *operator.StatePersistenceError
	if errors.As(errors.Join(report.coreError, err), &persistence) {
		switch persistence.Stage {
		case operator.PersistencePreApply, operator.PersistencePostApply, operator.PersistenceRecovery:
			d.metrics.persistence.WithLabelValues(string(persistence.Stage)).Inc()
		}
	}
	fields := []any{"outcome", outcome, "reason", reason, "duration_seconds", duration.Seconds()}
	if report.observation != nil {
		recovery := observationOutcome(report.observation.Evidence.Outcome)
		if recovery == "recovered" && outcome != "success" {
			recovery = "retryable"
			if deterministic {
				recovery = "refused"
			}
		}
		d.metrics.observations.WithLabelValues(recovery).Inc()
		fields = append(fields, "recovery_result", recovery)
		if id := safeDigest(report.observation.EventID); id != "" {
			fields = append(fields, "event_id", id)
		}
	}
	logger.Info("reconciliation result", fields...)
	if outcome == "failure" && obj.GetName() != "" {
		message := diagnosticGuidance(reason)
		if report.observation != nil {
			message += observationCategory(report.observation.Evidence.Reason)
		}
		c.recordBoundedEvent(obj, corev1.EventTypeWarning, reason, message)
	}
	if outcome == "success" {
		c.recordAuthorizationEvent(obj, report.status)
	}

}

func (c *Controller) scheduleAttempt(key types.NamespacedName, obj *unstructured.Unstructured, report *reconcileReport, previous retryState, result reconcile.Result, outcome, reason string, deterministic bool, now time.Time) reconcile.Result {

	next := retryState{uid: string(obj.GetUID()), revision: previous.revision, input: diagnosticInput(obj), cumulative: max(previous.cumulative, report.status.RetryCount), reason: reason, outcome: outcome}
	if report.phase == "read" {
		next.uid, next.input = previous.uid, previous.input
	}
	if outcome == "failure" {
		next.failures = previous.failures + 1
		if next.cumulative <= previous.cumulative {
			next.cumulative++
		}
		result = reconcile.Result{RequeueAfter: transientDelay(next.failures, rand.Float64())}
		if deterministic {
			result.RequeueAfter = deterministicRecheck
		}
	} else if reason == "AwaitingApproval" {
		result.RequeueAfter = deterministicRecheck
	}
	if result.RequeueAfter > 0 {
		next.next = now.Add(result.RequeueAfter)
	}
	d := c.signals()
	d.mu.Lock()
	if current := d.retries[key]; current.revision != previous.revision {
		next.revision = current.revision
		next.next = time.Time{}
		result = reconcile.Result{Requeue: true}
	}
	d.retries[key] = next
	d.mu.Unlock()
	return result
}
