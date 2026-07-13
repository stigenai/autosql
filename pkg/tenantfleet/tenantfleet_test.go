package tenantfleet

import (
	"context"
	"testing"
)

func tenant(id string) Tenant {
	return Tenant{ID: id, TargetID: "db-" + id, Environment: "prod", ConnectionRef: "env://DB_" + id, PolicyDigest: "sha256:policy", Active: true}
}
func TestTenantSnapshotAndIsolatedResume(t *testing.T) {
	s, e := Discover(context.Background(), []Discovery{StaticDiscovery{Tenants: []Tenant{tenant("b"), tenant("a")}}}, 10)
	if e != nil || len(s.Tenants) != 2 || s.Tenants[0].ID != "a" {
		t.Fatalf("snapshot=%+v err=%v", s, e)
	}
	x := &MemoryExecutor{Fail: map[string]bool{"a": true}}
	r, e := Execute(context.Background(), s, RolloutConfig{MaxConcurrent: 2, CanaryCount: 1, Overrides: map[string]PolicyOverride{"b": {TenantID: "b", PolicyDigest: "sha256:override", RequireApproval: true}}}, x)
	if e != nil || r.Status != "failed" || r.Results[1].State != StateSkipped {
		t.Fatalf("report=%+v err=%v", r, e)
	}
	r, e = Execute(context.Background(), s, RolloutConfig{MaxConcurrent: 1, CanaryCount: 1, Resume: map[string]Result{"a": {TenantID: "a", TargetID: "db-a", State: StatePassed}}}, &MemoryExecutor{})
	if e != nil || r.Results[0].State != StatePassed {
		t.Fatalf("resume=%+v err=%v", r, e)
	}
}
