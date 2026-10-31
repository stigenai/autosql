package operator

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

type failingCheckpointStore struct {
	Store
	failAt, saves int
}

func (s *failingCheckpointStore) Save(key string, record Record) error {
	s.saves++
	if s.saves == s.failAt {
		return errors.New("write state password=seeded-store-secret")
	}
	return s.Store.Save(key, record)
}

func TestOperatorReliabilityCheckpointFailureDoesNotPublishSuccess(t *testing.T) {
	for _, failAt := range []int{1, 2} {
		t.Run(map[int]string{1: "before apply", 2: "after apply"}[failAt], func(t *testing.T) {
			store := &failingCheckpointStore{Store: NewMemoryStore(), failAt: failAt}
			previous := Status{ObservedGeneration: 2, AppliedDigest: "previous", AppliedFingerprint: "previous-fingerprint", AuthorizationExpiresAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
			if err := store.Store.Save("orders", Record{Status: previous, AppliedKey: "previous-key"}); err != nil {
				t.Fatal(err)
			}
			obj := resource()
			obj.MetadataGeneration = 7
			calls := 0
			r := Reconciler{Store: store, Apply: func(context.Context, Resource, string) (ApplyResult, error) {
				calls++
				return ApplyResult{PlanDigest: "new-plan", ExecutionID: "execution", AppliedSteps: 3}, nil
			}}
			status, err := r.Reconcile(context.Background(), obj, true)
			if err == nil || calls != failAt-1 {
				t.Fatalf("failed checkpoint: err=%v calls=%d", err, calls)
			}
			if status.AppliedDigest != previous.AppliedDigest || status.ObservedGeneration != previous.ObservedGeneration || status.AppliedFingerprint != previous.AppliedFingerprint || !status.AuthorizationExpiresAt.Equal(previous.AuthorizationExpiresAt) {
				t.Fatalf("failed save advanced successful checkpoint: %+v", status)
			}
			last := status.Conditions[len(status.Conditions)-1]
			if last.Type != Failed || last.Reason != "StatePersistenceFailed" || last.ObservedGeneration != 7 {
				t.Fatalf("failed save not visible for current generation: %+v", status)
			}
			for _, condition := range status.Conditions {
				if condition.Type == Ready && condition.Status == "True" || strings.Contains(condition.Message, "seeded-store-secret") {
					t.Fatalf("unsafe failure condition: %+v", condition)
				}
			}
			if strings.Contains(err.Error(), "seeded-store-secret") {
				t.Fatal("persistence error exposed store details")
			}
			stored, _ := store.Load(obj.Name)
			if stored.AppliedKey == applyKey(obj) || stored.Status.AppliedDigest != previous.AppliedDigest {
				t.Fatalf("failed save changed stored success: %+v", stored)
			}
		})
	}
}

func TestOperatorReliabilityNonApplyingCheckpointErrorsAreVisible(t *testing.T) {
	for _, state := range []string{"suspended", "invalid", "approval", "resumed"} {
		t.Run(state, func(t *testing.T) {
			obj := resource()
			store := &failingCheckpointStore{Store: NewMemoryStore(), failAt: 1}
			approved := true
			switch state {
			case "suspended":
				obj.Spec.Suspend = true
			case "invalid":
				obj.Spec.Kind = "invalid"
			case "approval":
				approved = false
			case "resumed":
				status := condition(Status{}, Ready, "Suspended", "suspended", obj.Spec.Generation, time.Now())
				if err := store.Store.Save(obj.Name, Record{Status: status, AppliedKey: applyKey(obj)}); err != nil {
					t.Fatal(err)
				}
			}
			r := Reconciler{Store: store, Apply: func(context.Context, Resource, string) (ApplyResult, error) {
				t.Fatal("non-applying checkpoint reached application")
				return ApplyResult{}, nil
			}}
			status, err := r.Reconcile(context.Background(), obj, approved)
			if err == nil || status.Conditions[len(status.Conditions)-1].Reason != "StatePersistenceFailed" {
				t.Fatalf("checkpoint failure hidden: status=%+v err=%v", status, err)
			}
		})
	}
}
