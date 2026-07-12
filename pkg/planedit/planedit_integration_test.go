package planedit

import (
	"autosql/pkg/artifact"
	"autosql/pkg/guardrail"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/precheck"
	"autosql/pkg/schema"
	"autosql/pkg/simulate"
	"context"
	"crypto/ed25519"
	"os"
	"strings"
	"testing"
	"time"
)

type liveSimulator struct {
	url, id string
	from    schema.Document
}

func (s liveSimulator) Simulate(ctx context.Context, p plan.Plan) (string, error) {
	r, e := simulate.Run(ctx, simulate.PostgresFactory{}, simulate.Request{Config: simulate.Config{DevelopmentURL: s.url, DevelopmentIdentity: s.id, ProductionIdentity: "production.invalid:5432/prod", CleanupTimeout: 15 * time.Second}, From: s.from, Plan: p})
	return r.ToFingerprint, e
}
func TestLiveEditedPlanSimulationConvergesExactly(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	ctx := context.Background()
	from := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	from, _ = postgres.New().Normalize(ctx, from)
	name := schema.Name{Name: "edit_live"}
	to := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{{ID: schema.StableID(schema.KindSchema, name), Kind: schema.KindSchema, Name: name, Spec: []byte(`{}`)}}}}
	to, _ = postgres.New().Normalize(ctx, to)
	p, err := plan.Build(ctx, postgres.New(), from, to, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cd, _ := guardrail.ChangeDigest(p.Changes)
	checks := precheck.Plan{ID: "checks", ChangeDigest: cd, Statements: []string{p.Steps[0].SQL}}
	checks.Digest, _ = precheck.Digest(checks)
	now := time.Now().UTC()
	a, err := artifact.New(p, checks, now, now.Add(time.Hour), "rev", "test", "db", "sha256:"+strings.Repeat("a", 64), artifact.Approval{Identity: "alice", ApprovedAt: now}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(nil)
	if err = a.Sign("key", priv); err != nil {
		t.Fatal(err)
	}
	raw, _ := a.MarshalCanonical()
	edited, err := New(raw, a, "  "+p.Steps[0].SQL, "edited.sql", Editor{Identity: "editor", At: now, Reason: "review"})
	if err != nil {
		t.Fatal(err)
	}
	id, err := simulate.ResolvePostgresIdentity(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	var log []string
	eligible, err := (Pipeline{Simulator: liveSimulator{url: url, id: id, from: from}, Safety: safe{log: &log}, Binder: bind{log: &log}}).Revalidate(ctx, edited)
	if err != nil {
		t.Fatal(err)
	}
	if eligible.FinalFingerprint != p.ToFingerprint {
		t.Fatalf("got=%s want=%s", eligible.FinalFingerprint, p.ToFingerprint)
	}
}
