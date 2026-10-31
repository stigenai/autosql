package operatorcontroller

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/operator"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/types"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"
	"sigs.k8s.io/controller-runtime/pkg/client/interceptor"
	"sigs.k8s.io/controller-runtime/pkg/reconcile"
)

func TestCompletionStatusRoundTripAndLegacyCompatibility(t *testing.T) {
	ctx := context.Background()
	obj := testObject()
	obj.SetUID("original-resource")
	obj.SetGeneration(7)
	cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithStatusSubresource(obj).WithObjects(obj).Build()
	key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
	if err := cl.Get(ctx, key, obj); err != nil {
		t.Fatal(err)
	}
	observed := time.Date(2026, 10, 30, 1, 2, 3, 456000000, time.FixedZone("offset", -4*60*60))
	status := operator.UpdateCondition(operator.Status{ObservedGeneration: 7, AppliedDigest: "sha256:" + strings.Repeat("a", 64), CompletionObservedAt: observed}, operator.Ready, "CompletionRecovered", "completed execution verified; no SQL executed", 7, observed)
	if err := writeStatus(ctx, cl, obj, status); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, key, obj); err != nil {
		t.Fatal(err)
	}
	decoded := statusFromObject(obj)
	if !decoded.CompletionObservedAt.Equal(observed) || decoded.ObservedGeneration != 7 || decoded.AppliedDigest != status.AppliedDigest {
		t.Fatalf("completion checkpoint changed: %+v", decoded)
	}
	encoded, _, _ := unstructured.NestedString(obj.Object, "status", "completionObservedAt")
	if encoded != observed.UTC().Format(time.RFC3339Nano) {
		t.Fatalf("timestamp was not canonical UTC: %s", encoded)
	}
	resource, _, err := resourceFromObject(obj)
	if err != nil || resource.UID != "original-resource" || resource.MetadataGeneration != 7 {
		t.Fatalf("lost publication identity: %+v %v", resource, err)
	}
	// A subsequent ordinary apply clears observation time; old resources that
	// never had the optional field continue to decode to a zero value.
	status.CompletionObservedAt = time.Time{}
	if err := writeStatus(ctx, cl, obj, status); err != nil {
		t.Fatal(err)
	}
	if err := cl.Get(ctx, key, obj); err != nil {
		t.Fatal(err)
	}
	if _, found, err := unstructured.NestedString(obj.Object, "status", "completionObservedAt"); err != nil || found {
		t.Fatalf("legacy status acquired observation timestamp: %v", err)
	}
	if !statusFromObject(obj).CompletionObservedAt.IsZero() {
		t.Fatal("missing observation timestamp is not zero")
	}
}

type failSecondCheckpoint struct {
	operator.Store
	saves int
}

func (s *failSecondCheckpoint) Save(key string, value operator.Record) error {
	s.saves++
	if s.saves == 2 {
		return errors.New("injected checkpoint failure")
	}
	return s.Store.Save(key, value)
}

func completionObject() (*unstructured.Unstructured, *corev1.Secret) {
	obj := testObject()
	obj.SetUID("original-resource")
	obj.SetGeneration(7)
	obj.SetAnnotations(map[string]string{"autosql.io/approved": "true"})
	_ = unstructured.SetNestedField(obj.Object, "DeclarativeSchema", "spec", "kind")
	secret := &corev1.Secret{ObjectMeta: metav1.ObjectMeta{Name: "db", Namespace: "app"}, Data: map[string][]byte{"url": []byte("postgres://user:seeded-secret@invalid/database")}}
	return obj, secret
}

func TestControllerCompletionConflictRetriesPublicationOnly(t *testing.T) {
	ctx := context.Background()
	obj, secret := completionObject()
	now := time.Now().UTC()
	publications, validations, applies, observations := 0, 0, 0, 0
	cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithStatusSubresource(obj).WithObjects(obj, secret).WithInterceptorFuncs(interceptor.Funcs{
		SubResourceUpdate: func(ctx context.Context, cl client.Client, subresource string, object client.Object, options ...client.SubResourceUpdateOption) error {
			if recoveredReady(statusFromObject(object.(*unstructured.Unstructured))) {
				publications++
				if publications < 3 {
					return apierrors.NewConflict(schema.GroupResource{Group: "autosql.io", Resource: "autosqlschemas"}, obj.GetName(), errors.New("concurrent status writer"))
				}
			}
			return cl.SubResource(subresource).Update(ctx, object, options...)
		},
	}).Build()
	c := &Controller{Client: cl, Reader: cl, Store: &failSecondCheckpoint{Store: operator.NewMemoryStore()}, Now: func() time.Time { return now },
		Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
			applies++
			return operator.ApplyResult{PlanDigest: "plan", AppliedSteps: 2}, nil
		},
		Observe: func(_ context.Context, resource operator.Resource) operator.CompletionResult {
			observations++
			return operator.CompletionResult{Evidence: executor.CompletionEvidence{Outcome: executor.CompletionProven, ArtifactDigest: resource.Spec.ArtifactDigest, PlanDigest: "plan", TargetIdentity: "db", Environment: "test", Fingerprint: "sha256:" + strings.Repeat("b", 64), Attempt: 1, ConfirmedSteps: 2, ObservedAt: now}, AuthorizationExpiresAt: now.Add(time.Hour)}
		},
		RevalidateCompletion: func(context.Context, operator.Resource) error { validations++; return nil },
	}
	request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}}
	_, err := c.Reconcile(ctx, request)
	var persistence *operator.StatePersistenceError
	if !errors.As(err, &persistence) {
		t.Fatalf("expected post-apply failure: %v", err)
	}
	if _, err = c.Reconcile(ctx, request); err != nil {
		t.Fatal(err)
	}
	if applies != 1 || observations != 1 || publications != 3 || validations != 3 {
		t.Fatalf("repeated execution on publication conflict: applies=%d observations=%d publications=%d validations=%d", applies, observations, publications, validations)
	}
	if err := cl.Get(ctx, request.NamespacedName, obj); err != nil {
		t.Fatal(err)
	}
	status := statusFromObject(obj)
	if !recoveredReady(status) || !status.CompletionObservedAt.Equal(now) {
		t.Fatalf("recovery not published: %+v", status)
	}
}

func TestCompletionPublicationDiscardsChangedBindings(t *testing.T) {
	for _, change := range []string{"uid", "generation", "digest", "target", "approval", "suspend", "secret-content", "secret-version"} {
		t.Run(change, func(t *testing.T) {
			ctx := context.Background()
			obj, secret := completionObject()
			cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithStatusSubresource(obj).WithObjects(obj, secret).Build()
			c := &Controller{Client: cl, Reader: cl, RevalidateCompletion: func(context.Context, operator.Resource) error {
				t.Fatal("changed input reached authorization/publication")
				return nil
			}}
			key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
			if err := cl.Get(ctx, key, obj); err != nil {
				t.Fatal(err)
			}
			observed, approved, err := resourceFromObject(obj)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.resolveReferences(ctx, obj.GetNamespace(), &observed); err != nil {
				t.Fatal(err)
			}
			latest := obj.DeepCopy()
			switch change {
			case "uid":
				latest.SetUID("replacement")
			case "generation":
				latest.SetGeneration(8)
			case "digest":
				_ = unstructured.SetNestedField(latest.Object, "sha256:"+strings.Repeat("e", 64), "spec", "artifactDigest")
			case "target":
				_ = unstructured.SetNestedField(latest.Object, "replacement", "spec", "databaseURL", "name")
			case "approval":
				latest.SetAnnotations(map[string]string{"autosql.io/approved": "false"})
			case "suspend":
				_ = unstructured.SetNestedField(latest.Object, true, "spec", "suspend")
			case "secret-content", "secret-version":
				if err := cl.Get(ctx, types.NamespacedName{Namespace: secret.Namespace, Name: secret.Name}, secret); err != nil {
					t.Fatal(err)
				}
				if change == "secret-content" {
					secret.Data["url"] = []byte("postgres://changed")
				} else {
					secret.SetAnnotations(map[string]string{"rotation": "new"})
				}
				if err := cl.Update(ctx, secret); err != nil {
					t.Fatal(err)
				}
			}
			if err := cl.Update(ctx, latest); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			status := operator.UpdateCondition(operator.Status{CompletionObservedAt: now, AuthorizationExpiresAt: now.Add(time.Hour)}, operator.Ready, "CompletionRecovered", "observed", 7, now)
			changed, err := c.publishStatus(ctx, obj, observed, approved, status)
			if !changed || err != nil {
				t.Fatalf("stale observation was not discarded: changed=%t err=%v", changed, err)
			}
			if err := cl.Get(ctx, key, latest); err != nil {
				t.Fatal(err)
			}
			if recoveredReady(statusFromObject(latest)) {
				t.Fatal("stale completion published Ready")
			}
		})
	}
}

func TestCompletionPublicationRevalidatesAuthorization(t *testing.T) {
	for _, failure := range []string{"revoked", "expired", "unconfigured"} {
		t.Run(failure, func(t *testing.T) {
			ctx := context.Background()
			obj, secret := completionObject()
			cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithStatusSubresource(obj).WithObjects(obj, secret).Build()
			c := &Controller{Client: cl, Reader: cl, RevalidateCompletion: func(context.Context, operator.Resource) error { return errors.New("credential=seeded-secret") }}
			if failure == "unconfigured" {
				c.RevalidateCompletion = nil
			}
			if failure == "expired" {
				c.RevalidateCompletion = func(context.Context, operator.Resource) error {
					t.Fatal("expired authorization revalidated")
					return nil
				}
			}
			key := types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}
			if err := cl.Get(ctx, key, obj); err != nil {
				t.Fatal(err)
			}
			observed, approved, err := resourceFromObject(obj)
			if err != nil {
				t.Fatal(err)
			}
			if err := c.resolveReferences(ctx, obj.GetNamespace(), &observed); err != nil {
				t.Fatal(err)
			}
			now := time.Now().UTC()
			expiry := now.Add(time.Hour)
			if failure == "expired" {
				expiry = now.Add(-time.Second)
			}
			status := operator.UpdateCondition(operator.Status{CompletionObservedAt: now, AuthorizationExpiresAt: expiry}, operator.Ready, "CompletionRecovered", "observed", 7, now)
			if err := writeStatus(ctx, cl, obj, status); err != nil {
				t.Fatal(err)
			}
			changed, err := c.publishStatus(ctx, obj, observed, approved, status)
			var denied *operator.CompletionError
			if changed || !errors.As(err, &denied) || denied.Reason != "ReleaseAuthorizationInvalid" || strings.Contains(err.Error(), "seeded-secret") {
				t.Fatalf("unsafe authorization failure: changed=%t err=%v", changed, err)
			}
			if err := cl.Get(ctx, key, obj); err != nil {
				t.Fatal(err)
			}
			failed := statusFromObject(obj)
			deniedCondition := false
			for _, condition := range failed.Conditions {
				deniedCondition = deniedCondition || condition.Type == operator.Failed && condition.Status == "True" && condition.Reason == "ReleaseAuthorizationInvalid"
			}
			if recoveredReady(failed) || !deniedCondition {
				t.Fatal("unauthorized completion retained Ready or lost the refusal diagnostic")
			}
		})
	}
}

func TestControllerCompletionAuthenticatesBeforeReferences(t *testing.T) {
	for _, scenario := range []string{"invalid release", "expired approval", "elapsed release", "lost checkpoint"} {
		t.Run(scenario, func(t *testing.T) {
			ctx := context.Background()
			obj, secret := completionObject()
			digest, _, _ := unstructured.NestedString(obj.Object, "spec", "artifactDigest")
			if scenario == "lost checkpoint" {
				_ = unstructured.SetNestedField(obj.Object, digest, "status", "appliedDigest")
				_ = unstructured.SetNestedField(obj.Object, int64(7), "status", "observedGeneration")
			}
			cl := fake.NewClientBuilder().WithScheme(NewScheme()).WithStatusSubresource(obj).WithObjects(obj, secret).Build()
			spy := &runtimeReferenceReadSpy{Client: cl}
			observations, validations := 0, 0
			now := time.Now().UTC()
			c := &Controller{Client: spy, Reader: spy, Store: operator.NewMemoryStore(), Apply: func(context.Context, operator.Resource, string) (operator.ApplyResult, error) {
				t.Fatal("recovery called Apply")
				return operator.ApplyResult{}, nil
			},
				VerifyRelease: func(string) (artifact.VerifiedArtifact, error) {
					if scenario == "invalid release" {
						return artifact.VerifiedArtifact{}, artifact.ErrInvalid
					}
					if scenario == "lost checkpoint" {
						return artifact.VerifiedArtifact{}, nil
					}
					return artifact.VerifiedArtifact{}, artifact.ErrExpired
				},
				VerifyCompletionRelease: func(context.Context, string) error {
					validations++
					if spy.reads != 0 {
						t.Fatal("release authorization ran after reference read")
					}
					if scenario == "expired approval" {
						return errors.New("expired")
					}
					return nil
				},
				Observe: func(_ context.Context, resource operator.Resource) operator.CompletionResult {
					observations++
					if !resource.CompletionRequired {
						t.Fatal("lost state did not select observation only")
					}
					return operator.CompletionResult{Evidence: executor.CompletionEvidence{Outcome: executor.CompletionProven, ArtifactDigest: digest, PlanDigest: "plan", TargetIdentity: "db", Environment: "test", Fingerprint: "fingerprint", Attempt: 1, ConfirmedSteps: 1, ObservedAt: now}, AuthorizationExpiresAt: now.Add(time.Hour)}
				}, RevalidateCompletion: func(context.Context, operator.Resource) error { return nil },
			}
			request := reconcile.Request{NamespacedName: types.NamespacedName{Namespace: obj.GetNamespace(), Name: obj.GetName()}}
			_, err := c.Reconcile(ctx, request)
			if scenario == "invalid release" || scenario == "expired approval" {
				if err == nil || spy.reads != 0 || observations != 0 {
					t.Fatalf("denied recovery performed work: reads=%d observations=%d error=%v", spy.reads, observations, err)
				}
			} else if err != nil || observations != 1 || validations != 1 {
				t.Fatalf("recovery not selected: observations=%d validations=%d err=%v", observations, validations, err)
			}
		})
	}
}
