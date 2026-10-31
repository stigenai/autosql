package operatorcontroller

import (
	"context"
	"reflect"
	"time"

	"autosql/pkg/operator"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/util/retry"
)

func recoveredReady(status operator.Status) bool {
	for _, condition := range status.Conditions {
		if condition.Type == operator.Ready && condition.Status == "True" && condition.Reason == "CompletionRecovered" {
			return true
		}
	}
	return false
}

// publishStatus retries only status publication. It never repeats observation or
// apply on conflict. A new object/spec/reference discards the old result.
func (c *Controller) publishStatus(ctx context.Context, original *unstructured.Unstructured, observed operator.Resource, approved bool, status operator.Status) (bool, error) {
	if !recoveredReady(status) {
		return false, writeStatus(ctx, c.Client, original, status)
	}
	changed := false
	err := retry.RetryOnConflict(retry.DefaultRetry, func() error {
		latest := &unstructured.Unstructured{}
		latest.SetGroupVersionKind(GroupVersionKind)
		if err := c.read(ctx, types.NamespacedName{Namespace: original.GetNamespace(), Name: original.GetName()}, latest); err != nil {
			if apierrors.IsNotFound(err) {
				changed = true
				return nil
			}
			return &operator.CompletionError{Reason: "DependencyUnavailable", Retryable: true}
		}
		current, currentApproval, err := resourceFromObject(latest)
		if err != nil || !latest.GetDeletionTimestamp().IsZero() || !sameCompletionResource(observed, current) || approved != currentApproval {
			changed = true
			return nil
		}
		if err := c.resolveReferences(ctx, latest.GetNamespace(), &current); err != nil {
			return &operator.CompletionError{Reason: "DependencyUnavailable", Retryable: true}
		}
		if !sameCompletionReferences(observed, current) {
			changed = true
			return nil
		}
		now := time.Now().UTC()
		if c.Now != nil {
			now = c.Now().UTC()
		}
		if c.RevalidateCompletion == nil || !status.AuthorizationExpiresAt.After(now) || c.RevalidateCompletion(ctx, current) != nil {
			denied := &operator.CompletionError{Reason: "ReleaseAuthorizationInvalid"}
			failed := operator.UpdateCondition(statusFromObject(latest), operator.Failed, denied.Reason, "correct release authorization or publish a fresh approved release", latest.GetGeneration(), now)
			if err := writeStatus(ctx, c.Client, latest, failed); err != nil {
				return err
			}
			return denied
		}
		return writeStatus(ctx, c.Client, latest, status)
	})
	return changed, err
}

func sameCompletionResource(a, b operator.Resource) bool {
	return a.UID == b.UID && a.Name == b.Name && a.MetadataGeneration == b.MetadataGeneration && reflect.DeepEqual(a.Spec, b.Spec)
}

func sameCompletionReferences(a, b operator.Resource) bool {
	return a.SourceBinding == b.SourceBinding && a.DatabaseBinding == b.DatabaseBinding && a.MaintenanceDatabaseBinding == b.MaintenanceDatabaseBinding && a.AuthorizationManifestBinding == b.AuthorizationManifestBinding && a.AuthorizationPublicKeyBinding == b.AuthorizationPublicKeyBinding
}
