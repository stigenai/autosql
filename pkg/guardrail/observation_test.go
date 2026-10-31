package guardrail

import (
	"context"
	"errors"
	"testing"
	"time"

	"autosql/pkg/approval"
)

func TestObservationAuthorizationUsesPolicyWithoutDatabaseOrAudit(t *testing.T) {
	log := &eventLog{}
	g, db, audit := harness(t, log)
	now := time.Now().UTC()
	g.Approval.Policy.Environments["prod"] = approval.EnvironmentPolicy{Allowed: true, Requirements: []approval.Requirement{{MinimumRisk: approval.RiskLow, ApproverCount: 1, Roles: []string{"dba"}}}}
	in, _ := boundInput(t, g, "prod")
	in.Database = db
	in.Approval.Plan.ExpiresAt = now.Add(-time.Minute)
	digest, err := g.BundleDigest(in)
	if err != nil {
		t.Fatal(err)
	}
	in.Approval.Plan.Digest = digest
	in.Approval.Approvals = []approval.Approval{{PlanDigest: digest, Environment: "prod", Approver: "dba", ApprovedAt: now.Add(-time.Hour), ExpiresAt: now.Add(time.Hour), Proof: "signed"}}
	if _, err := g.ObserveAuthorization(context.Background(), in); err != nil {
		t.Fatal(err)
	}
	if db.tx != nil || len(audit.events) != 0 {
		t.Fatal("observation invoked database or execution approval audit")
	}
	if _, err := g.Apply(context.Background(), in); !errors.Is(err, ErrApproval) {
		t.Fatalf("expired plan became executable: %v", err)
	}
	// Execution above may log its refusal; observation must add no further audit.
	before := len(audit.events)
	in.Approval.Approvals[0].ExpiresAt = now
	if _, err := g.ObserveAuthorization(context.Background(), in); !errors.Is(err, ErrApproval) {
		t.Fatalf("expired approval accepted: %v", err)
	}
	if db.tx != nil || len(audit.events) != before {
		t.Fatal("refused observation performed I/O")
	}
	in.PolicyIdentity = "changed-policy"
	if _, err := g.ObserveAuthorization(context.Background(), in); !errors.Is(err, ErrBinding) {
		t.Fatalf("changed policy bundle accepted: %v", err)
	}
}
