package integration_test

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"autosql/pkg/analytics"
	"autosql/pkg/approval"
	"autosql/pkg/auditlog"
	"autosql/pkg/drift"
	"autosql/pkg/inventory"
	"autosql/pkg/schema"
)

type integrationAuthority struct{}

func (integrationAuthority) ResolveActor(_ context.Context, id string) (approval.Identity, error) {
	if id == "author" {
		return approval.Identity{ID: "author", Roles: []string{"developer"}}, nil
	}
	return approval.Identity{ID: "deployer"}, nil
}
func (integrationAuthority) VerifyApproval(_ context.Context, a approval.Approval) (approval.VerifiedApproval, error) {
	if a.Proof != "signed:alice" {
		return approval.VerifiedApproval{}, errors.New("invalid proof")
	}
	return approval.VerifiedApproval{Identity: approval.Identity{ID: "alice", Roles: []string{"dba"}}, PlanDigest: a.PlanDigest, Environment: a.Environment, ApprovedAt: a.ApprovedAt, ExpiresAt: a.ExpiresAt}, nil
}

type integrationSink struct{ records []approval.AuditRecord }

func (s *integrationSink) Tail(context.Context) (*approval.AuditRecord, error) {
	if len(s.records) == 0 {
		return nil, nil
	}
	r := s.records[len(s.records)-1]
	return &r, nil
}
func (s *integrationSink) AppendDurable(_ context.Context, expected string, r approval.AuditRecord) error {
	actual := ""
	if len(s.records) > 0 {
		actual = s.records[len(s.records)-1].Hash
	}
	if actual != expected {
		return approval.ErrAuditConflict
	}
	s.records = append(s.records, r)
	return nil
}

type integrationInspector struct{ doc schema.Document }

func (i integrationInspector) Inspect(context.Context, drift.Target) (schema.Document, error) {
	return i.doc, nil
}

func TestControlPlaneBindingsAndRedaction(t *testing.T) {
	now := time.Unix(100, 0).UTC()
	artifactDigest := "sha256:" + strings.Repeat("a", 64)
	planDigest := "sha256:" + strings.Repeat("b", 64)
	gate := approval.Gate{Now: func() time.Time { return now }, Authority: integrationAuthority{}, Audit: &approval.Chain{Sink: &integrationSink{}}, Policy: approval.Policy{Environments: map[string]approval.EnvironmentPolicy{"prod": {Allowed: true, Requirements: []approval.Requirement{{MinimumRisk: approval.RiskLow, ApproverCount: 1, Roles: []string{"dba"}}}}}}}
	req := approval.Request{Plan: approval.Plan{Digest: planDigest, Environment: "prod", Author: "author", Risk: approval.RiskLow}, RequestedBy: "deployer", Approvals: []approval.Approval{{PlanDigest: planDigest, Environment: "prod", Approver: "alice", ApprovedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Proof: "signed:alice"}}}
	if err := gate.GuardedApply(context.Background(), req, func(context.Context) error { return nil }); err != nil {
		t.Fatal(err)
	}
	log := auditlog.New()
	if _, err := log.Append(auditlog.Event{ID: "run-1", At: now, Actor: "deployer", Action: "apply", Subject: artifactDigest, Result: auditlog.ResultSuccess, CorrelationID: "run-1", Detail: map[string]string{"status": "applied"}}); err != nil {
		t.Fatal(err)
	}
	if err := log.Verify(); err != nil {
		t.Fatal(err)
	}
	store := inventory.NewStore()
	target := inventory.Target{Project: "demo", Environment: "prod", ID: "db-1"}
	if _, err := store.Upsert(target, inventory.Observation{ReportID: "report-1", CurrentVersion: "v2", ExpectedVersion: "v2", SyncStatus: inventory.SyncCurrent, ObservedAt: now}); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(inventory.DeploymentEvent{ID: "event-1", RunID: "run-1", ArtifactDigest: artifactDigest, Project: "demo", Environment: "prod", Status: inventory.Passed, At: now, Targets: []inventory.TargetResult{{TargetID: "db-1", Status: inventory.Passed}}}); err != nil {
		t.Fatal(err)
	}
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	incident, err := drift.New().Check(context.Background(), integrationInspector{doc: doc}, drift.Target{ID: "db-1", ExpectedDigest: planDigest, Expected: doc, ReadOnly: true, MaxResources: 10})
	if err != nil || incident.Status != "in_sync" {
		t.Fatalf("drift=%+v err=%v", incident, err)
	}
	collector := analytics.Collector{Source: analytics.StaticSource{}, Now: func() time.Time { return now }}
	snapshot, err := collector.Collect(analytics.Request{TargetID: "db-1", ArtifactDigest: artifactDigest, SchemaDigest: planDigest, MaxTables: 10, MaxIndexes: 10, MaxConstraints: 10})
	if err != nil {
		t.Fatal(err)
	}
	if err := analytics.NewStore(time.Hour, 10).Append(snapshot); err != nil {
		t.Fatal(err)
	}
	if _, err := log.Append(auditlog.Event{ID: "secret", At: now, Actor: "deployer", Action: "apply", Subject: artifactDigest, Result: auditlog.ResultSuccess, CorrelationID: "run-2", Detail: map[string]string{"password": "hidden"}}); !errors.Is(err, auditlog.ErrSensitiveData) {
		t.Fatalf("redaction err=%v", err)
	}
}
