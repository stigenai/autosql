package guardrail

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
)

func TestProjectInputsInvalidateApprovalBeforeAnyGateOrMutation(t *testing.T) {
	log := &eventLog{}
	g, db, _ := harness(t, log)
	in, legacy := boundInput(t, g, "prod")
	in.Database = db
	in.ProjectInputDigest = "sha256:" + strings.Repeat("a", 64)
	approved, err := g.BundleDigest(in)
	if err != nil || approved == legacy {
		t.Fatalf("project inputs did not enter approval: %v", err)
	}
	in.Approval.Plan.Digest = approved
	in.ProjectInputDigest = "sha256:" + strings.Repeat("b", 64)
	if _, err := g.Apply(context.Background(), in); !errors.Is(err, ErrBinding) || db.tx != nil || len(log.values) != 0 {
		t.Fatalf("stale project approval reached a gate or mutation: %v events=%v", err, log.values)
	}
	in.ProjectInputDigest = ""
	if _, err := g.Apply(context.Background(), in); !errors.Is(err, ErrBinding) || db.tx != nil {
		t.Fatalf("removing project identity reused approval: %v", err)
	}
	in.ProjectInputDigest = "untrusted-report"
	if _, err := g.BundleDigest(in); !errors.Is(err, ErrBinding) {
		t.Fatalf("malformed input identity: %v", err)
	}
	in.ProjectInputDigest = ""
	if digest, err := g.BundleDigest(in); err != nil || digest != legacy {
		t.Fatalf("legacy input digest changed: %v", err)
	}
	raw, err := json.Marshal(enforcementBundle{})
	if err != nil || strings.Contains(string(raw), "project_input_digest") {
		t.Fatal("legacy bundle gained a new serialized field")
	}
}
