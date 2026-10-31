package operatorcontroller

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/operator"
	"github.com/go-logr/logr"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/testutil"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func testDiagnostics(reg *prometheus.Registry) *operatorDiagnostics {
	return &operatorDiagnostics{metrics: newOperatorMetrics(reg), retries: map[types.NamespacedName]retryState{}, warnings: map[types.NamespacedName]warningState{}}
}

func TestManagedDeterministicRecheckAndReferenceInvalidation(t *testing.T) {
	obj, secret := completionObject()
	cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(obj, secret).WithStatusSubresource(obj).Build()
	now := time.Now().UTC()
	observations := 0
	events := record.NewFakeRecorder(20)
	c := &Controller{Client: cl, Store: operator.NewMemoryStore(), Recorder: events, Now: func() time.Time { return now }, diagnostics: testDiagnostics(prometheus.NewRegistry()),
		Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
			t.Fatal("deterministic refusal applied SQL")
			return operator.ApplyResult{}, nil
		},
		Probe: func(context.Context, operator.Resource) executor.ExecutionStartCheck {
			return executor.ExecutionStartCheck{Started: true}
		},
		Observe: func(context.Context, operator.Resource) operator.CompletionResult {
			observations++
			return operator.CompletionResult{Evidence: executor.CompletionEvidence{Outcome: executor.CompletionRefused, Reason: "history_invalid"}}
		},
	}
	ctx := context.Background()
	for i := 0; i < 4; i++ {
		result, err := c.reconcileManaged(ctx, requestFor(obj))
		if err != nil || result.RequeueAfter != 5*time.Minute-time.Duration(i)*time.Minute {
			t.Fatalf("retry policy: %+v %v", result, err)
		}
		now = now.Add(time.Minute)
	}
	if observations != 1 || len(events.Events) != 1 {
		t.Fatalf("repeated refusal: observations=%d events=%d", observations, len(events.Events))
	}
	requests := c.requestsForSecret(ctx, secret)
	if len(requests) != 1 {
		t.Fatal("reference watch did not find resource")
	}
	if _, err := c.reconcileManaged(ctx, requests[0]); err != nil {
		t.Fatal(err)
	}
	if observations != 2 || len(events.Events) != 1 {
		t.Fatal("reference change did not immediately retry with warning suppression")
	}
	now = now.Add(5 * time.Minute)
	if _, err := c.reconcileManaged(ctx, requests[0]); err != nil {
		t.Fatal(err)
	}
	if observations != 3 || len(events.Events) != 2 {
		t.Fatal("five minute recheck did not emit a new warning")
	}
	if err := cl.Get(ctx, requestFor(obj).NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	_ = unstructured.SetNestedField(obj.Object, true, "spec", "suspend")
	if err := cl.Update(ctx, obj); err != nil {
		t.Fatal(err)
	}
	if result, err := c.reconcileManaged(ctx, requestFor(obj)); err != nil || result.RequeueAfter != 0 || observations != 3 {
		t.Fatalf("suspension waited or observed: %+v %v", result, err)
	}
}

type diagnosticsStore struct {
	operator.Store
	fail bool
}

func (s *diagnosticsStore) Save(key string, value operator.Record) error {
	if s.fail {
		return errors.New("postgres://user:seeded-secret@db/private CREATE TABLE private_data")
	}
	return s.Store.Save(key, value)
}

func TestManagedFailedWritesRemainObservableAndRedacted(t *testing.T) {
	obj, secret := completionObject()
	_ = unstructured.SetNestedField(obj.Object, "VersionedMigration", "spec", "kind")
	failStatus := true
	cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(obj, secret).WithStatusSubresource(obj).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, name string, object client.Object, options ...client.SubResourceUpdateOption) error {
			if failStatus {
				return errors.New("seeded-secret status unavailable SELECT private_data")
			}
			return cl.SubResource(name).Update(ctx, object, options...)
		},
	}).Build()
	store := &diagnosticsStore{Store: operator.NewMemoryStore(), fail: true}
	reg := prometheus.NewRegistry()
	var output bytes.Buffer
	ctx := ctrl.LoggerInto(context.Background(), logr.FromSlogHandler(slog.NewJSONHandler(&output, nil)))
	now := time.Now().UTC()
	applies := 0
	c := &Controller{Client: cl, Store: store, Now: func() time.Time { return now }, diagnostics: testDiagnostics(reg), Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
		applies++
		return operator.ApplyResult{}, nil
	}}
	for i := 0; i < 10; i++ {
		result, err := c.reconcileManaged(ctx, requestFor(obj))
		if err != nil || result.RequeueAfter < time.Second || result.RequeueAfter > time.Minute {
			t.Fatalf("retry outside bounds: %+v %v", result, err)
		}
		now = now.Add(result.RequeueAfter)
	}
	if applies != 0 {
		t.Fatal("failed checkpoint ran SQL")
	}
	m := c.signals().metrics
	if testutil.ToFloat64(m.persistence.WithLabelValues("pre_apply")) != 10 || testutil.ToFloat64(m.reconciliations.WithLabelValues("failure", "StatusPublicationFailed")) != 10 {
		t.Fatal("unpublished failures not counted")
	}
	if strings.Contains(output.String(), "seeded-secret") || strings.Contains(output.String(), "private_data") || strings.Contains(output.String(), "postgres://") {
		t.Fatal("logs leaked raw errors")
	}
	if !strings.Contains(output.String(), `"reason":"StatusPublicationFailed"`) || !strings.Contains(output.String(), `"duration_seconds"`) {
		t.Fatal("missing structured result")
	}
	failStatus, store.fail = false, false
	if _, err := c.reconcileManaged(ctx, requestFor(obj)); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, requestFor(obj).NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	if status := statusFromObject(obj); status.RetryCount != 10 || status.AppliedDigest == "" {
		t.Fatalf("success lost cumulative failures: %+v", status)
	}
}

func TestManagedApplyErrorCannotLeakThroughStatusOrEvents(t *testing.T) {
	obj, secret := completionObject()
	cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(obj, secret).WithStatusSubresource(obj).Build()
	events := record.NewFakeRecorder(10)
	c := &Controller{Client: cl, Store: operator.NewMemoryStore(), Recorder: events, diagnostics: testDiagnostics(prometheus.NewRegistry()), Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
		return operator.ApplyResult{}, errors.New("postgres://user:seeded-secret@db SELECT private_data")
	}}
	if _, err := c.reconcileManaged(context.Background(), requestFor(obj)); err != nil {
		t.Fatal("raw error escaped to controller-runtime")
	}
	if err := cl.Get(context.Background(), requestFor(obj).NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	status, _ := json.Marshal(obj.Object["status"])
	if strings.Contains(string(status), "seeded-secret") || strings.Contains(string(status), "private_data") {
		t.Fatal("unsafe status")
	}
	if len(events.Events) != 1 {
		t.Fatalf("events=%d", len(events.Events))
	}
	if event := <-events.Events; strings.Contains(event, "seeded-secret") || strings.Contains(event, "private_data") {
		t.Fatal("unsafe event")
	}
}

func TestOperatorMetricLabelsAreBoundedAndInitialized(t *testing.T) {
	reg := prometheus.NewRegistry()
	newOperatorMetrics(reg)
	families, err := reg.Gather()
	if err != nil {
		t.Fatal(err)
	}
	if len(families) != 4 {
		t.Fatalf("metrics=%d", len(families))
	}
	for _, family := range families {
		for _, metric := range family.Metric {
			for _, label := range metric.Label {
				switch label.GetName() {
				case "reason":
					if boundedReason(label.GetValue()) != label.GetValue() {
						t.Fatal("unbounded reason")
					}
				case "outcome", "stage":
				default:
					t.Fatalf("unbounded label %s", label.GetName())
				}
			}
		}
	}
	if boundedReason("seeded-secret") != "other" {
		t.Fatal("unknown reason became a label")
	}
	for _, n := range []int{1, 2, 6, 7, 1000} {
		for _, jitter := range []float64{-1, 0, 0.5, 1, 2} {
			delay := transientDelay(n, jitter)
			if delay < time.Second || delay > time.Minute {
				t.Fatalf("delay=%s", delay)
			}
		}
	}
}

func TestOperatorHealthDoesNotDependOnLeadershipOrSchemaReadiness(t *testing.T) {
	request := httptest.NewRequest("GET", "/readyz", nil)
	if err := cacheReady(func(context.Context) bool { return true })(request); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := cacheReady(func(ctx context.Context) bool { <-ctx.Done(); return false })(request.WithContext(ctx)); err == nil {
		t.Fatal("unsynchronized cache reported ready")
	}
	defaults := (RunOptions{}).withDefaults()
	if defaults.MetricsBindAddress != ":8080" || defaults.HealthProbeBindAddress != ":8081" {
		t.Fatal(defaults)
	}
	disabled := (RunOptions{MetricsBindAddress: "0", HealthProbeBindAddress: "0"}).withDefaults()
	if disabled.MetricsBindAddress != "0" || disabled.HealthProbeBindAddress != "0" {
		t.Fatal("disabled endpoints reenabled")
	}
}

func TestReferenceChangeDuringReconcileCannotBeDelayed(t *testing.T) {
	obj, secret := completionObject()
	cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(obj, secret).WithStatusSubresource(obj).Build()
	c := &Controller{Client: cl, Store: operator.NewMemoryStore(), diagnostics: testDiagnostics(prometheus.NewRegistry())}
	c.Apply = func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
		c.invalidateRetries([]reconcile.Request{requestFor(obj)})
		return operator.ApplyResult{}, errors.New("unavailable")
	}
	result, err := c.reconcileManaged(context.Background(), requestFor(obj))
	if err != nil || !result.Requeue || !c.signals().retries[requestFor(obj).NamespacedName].next.IsZero() {
		t.Fatalf("reference event was lost: %+v %v", result, err)
	}
}

func TestManagedRunbookReasonsAndGuidance(t *testing.T) {
	for _, scenario := range []struct {
		name, reason, guidance string
		deterministic          bool
	}{
		{"success", "Applied", "", false},
		{"expired", "ArtifactExpired", "fresh approved release", true},
		{"authorization", "ReleaseAuthorizationInvalid", "authorization", true},
		{"persistence", "StatePersistenceFailed", "storage", false},
		{"dependency", "DependencyUnavailable", "dependency", false},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			obj, secret := completionObject()
			reg := prometheus.NewRegistry()
			store := &diagnosticsStore{Store: operator.NewMemoryStore(), fail: scenario.name == "persistence"}
			c := &Controller{Store: store, diagnostics: testDiagnostics(reg), Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
				return operator.ApplyResult{}, nil
			}}
			if scenario.name == "expired" || scenario.name == "authorization" {
				_ = unstructured.SetNestedField(obj.Object, "IfEquivalent", "spec", "adoptionPolicy")
				c.VerifyRelease = func(string) (artifact.VerifiedArtifact, error) {
					if scenario.name == "expired" {
						return artifact.VerifiedArtifact{}, artifact.ErrExpired
					}
					return artifact.VerifiedArtifact{}, errors.New("seeded-secret release rejected")
				}
			}
			objects := []client.Object{obj}
			if scenario.name != "dependency" {
				objects = append(objects, secret)
			}
			c.Client = fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(objects...).WithStatusSubresource(obj).Build()
			result, err := c.reconcileManaged(context.Background(), requestFor(obj))
			if err != nil {
				t.Fatal(err)
			}
			if scenario.deterministic && result.RequeueAfter != deterministicRecheck {
				t.Fatal("deterministic refusal retried too soon")
			}
			if err := c.Get(context.Background(), requestFor(obj).NamespacedName, obj); err != nil {
				t.Fatal(err)
			}
			condition := activeCondition(statusFromObject(obj))
			if condition.Reason != scenario.reason || !strings.Contains(condition.Message, scenario.guidance) {
				t.Fatalf("unactionable condition: %+v", condition)
			}
			outcome := "failure"
			if scenario.name == "success" {
				outcome = "success"
			}
			if count := testutil.ToFloat64(c.signals().metrics.reconciliations.WithLabelValues(outcome, scenario.reason)); count != 1 {
				t.Fatal("runbook reason missing from metrics")
			}
		})
	}
}

func TestManagedInputFailureWithUnpublishedStatusIsTransient(t *testing.T) {
	obj := testObject()
	delete(obj.Object, "spec")
	cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(obj).WithStatusSubresource(obj).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(context.Context, client.Client, string, client.Object, ...client.SubResourceUpdateOption) error {
			return errors.New("seeded-secret API write failed")
		},
	}).Build()
	c := &Controller{Client: cl, diagnostics: testDiagnostics(prometheus.NewRegistry())}
	result, err := c.reconcileManaged(context.Background(), requestFor(obj))
	if err != nil || result.RequeueAfter > 2*time.Second {
		t.Fatalf("unpublished status treated as deterministic: %+v %v", result, err)
	}
	if testutil.ToFloat64(c.signals().metrics.reconciliations.WithLabelValues("failure", "StatusPublicationFailed")) != 1 {
		t.Fatal("status failure was hidden")
	}
}

func TestManagedRecoveryCorrelatesAuditAndPersistenceStages(t *testing.T) {
	for _, stage := range []string{"post_apply", "recovery"} {
		t.Run(stage, func(t *testing.T) {
			obj, secret := completionObject()
			cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithObjects(obj, secret).WithStatusSubresource(obj).Build()
			now := time.Now().UTC()
			store := &failSecondCheckpoint{Store: operator.NewMemoryStore()}
			if stage == "recovery" {
				store.saves = 1
			}
			applies, observations := 0, 0
			var output bytes.Buffer
			ctx := ctrl.LoggerInto(context.Background(), logr.FromSlogHandler(slog.NewJSONHandler(&output, nil)))
			c := &Controller{Client: cl, Store: store, Now: func() time.Time { return now }, diagnostics: testDiagnostics(prometheus.NewRegistry()),
				Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
					applies++
					return operator.ApplyResult{}, nil
				},
				RevalidateCompletion: func(context.Context, operator.Resource) error { return nil },
				Observe: func(_ context.Context, r operator.Resource) operator.CompletionResult {
					observations++
					return operator.CompletionResult{Evidence: executor.CompletionEvidence{Outcome: executor.CompletionProven, ArtifactDigest: r.Spec.ArtifactDigest, PlanDigest: "plan", TargetIdentity: "target", Environment: "test", Fingerprint: "fingerprint", Attempt: 1, ConfirmedSteps: 1, ObservedAt: now}, AuthorizationExpiresAt: now.Add(time.Hour), EventID: executor.CompletionObservationID(r.UID, r.MetadataGeneration, r.Spec.ArtifactDigest, 1)}
				},
			}
			if stage == "recovery" {
				c.Probe = func(context.Context, operator.Resource) executor.ExecutionStartCheck {
					return executor.ExecutionStartCheck{Started: true}
				}
			}
			result, err := c.reconcileManaged(ctx, requestFor(obj))
			if err != nil {
				t.Fatal(err)
			}
			if testutil.ToFloat64(c.signals().metrics.persistence.WithLabelValues(stage)) != 1 {
				t.Fatal("persistence stage missing")
			}
			now = now.Add(result.RequeueAfter)
			if _, err := c.reconcileManaged(ctx, requestFor(obj)); err != nil {
				t.Fatal(err)
			}
			if err := cl.Get(ctx, requestFor(obj).NamespacedName, obj); err != nil {
				t.Fatal(err)
			}
			if !recoveredReady(statusFromObject(obj)) {
				t.Fatal("recovery was not published")
			}
			if testutil.ToFloat64(c.signals().metrics.observations.WithLabelValues("recovered")) != 1 {
				t.Fatal("successful observation missing")
			}
			id := executor.CompletionObservationID(string(obj.GetUID()), obj.GetGeneration(), releaseLogDigest(obj), 1)
			if !strings.Contains(output.String(), `"event_id":"`+id+`"`) {
				t.Fatal("audit event identity missing from result log")
			}
			if stage == "recovery" && (applies != 0 || observations != 2 || testutil.ToFloat64(c.signals().metrics.observations.WithLabelValues("retryable")) != 1) {
				t.Fatal("recovery failure was counted as recovered or replayed")
			}
			if stage == "post_apply" && (applies != 1 || observations != 1) {
				t.Fatal("post-apply failure replayed")
			}
		})
	}
}

func TestMain(m *testing.M) {
	ctrl.SetLogger(logr.FromSlogHandler(slog.NewJSONHandler(os.Stderr, nil)))
	os.Exit(m.Run())
}
