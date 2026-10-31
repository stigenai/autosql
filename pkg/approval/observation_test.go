package approval

import (
	"context"
	"errors"
	"testing"
	"time"
)

func TestObservationPreservesCurrentApprovalValidityWithoutAudit(t *testing.T) {
	now := time.Now().UTC()
	g, req, sink := base(now)
	req.Plan.ExpiresAt = now.Add(-time.Minute)
	for _, id := range []string{"alice", "bob"} {
		req.Approvals = append(req.Approvals, Approval{PlanDigest: req.Plan.Digest, Environment: req.Plan.Environment, Approver: id, ApprovedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Minute), Proof: "signed:" + id})
	}
	if decision, err := g.EvaluateForObservation(context.Background(), req); err != nil || !decision.Allowed || len(sink.records) != 0 {
		t.Fatalf("observation authorization: %+v %v writes=%d", decision, err, len(sink.records))
	}
	if _, err := g.Evaluate(context.Background(), req); !errors.Is(err, ErrDenied) {
		t.Fatal("observation relaxed execution expiry")
	}
	req.Approvals[0].ExpiresAt = now
	if _, err := g.EvaluateForObservation(context.Background(), req); !errors.Is(err, ErrDenied) {
		t.Fatal("expired approval accepted for observation")
	}
	if len(sink.records) != 0 {
		t.Fatal("observation appended an execution approval decision")
	}
}
