package planedit

import (
	"autosql/pkg/artifact"
	"autosql/pkg/guardrail"
	"autosql/pkg/plan"
	"autosql/pkg/plugin/sample"
	"autosql/pkg/precheck"
	"autosql/pkg/schema"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"errors"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (artifact.Artifact, []byte, ed25519.PrivateKey) {
	t.Helper()
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	n := schema.Name{Name: "app"}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{{ID: schema.StableID(schema.KindSchema, n), Kind: schema.KindSchema, Name: n, Spec: []byte(`{}`)}}}}
	p, err := plan.Build(context.Background(), sample.Driver{}, empty, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	cd, _ := guardrail.ChangeDigest(p.Changes)
	var sql []string
	for _, s := range p.Steps {
		if s.Kind == plan.StepExecutable {
			sql = append(sql, s.SQL)
		}
	}
	checks := precheck.Plan{ID: "checks", ChangeDigest: cd, Statements: sql}
	checks.Digest, err = precheck.Digest(checks)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	a, err := artifact.New(p, checks, now, now.Add(time.Hour), "rev", "test", "db", "sha256:"+strings.Repeat("a", 64), artifact.Approval{Identity: "alice", ApprovedAt: now}, map[string]string{})
	if err != nil {
		t.Fatal(err)
	}
	_, priv, _ := ed25519.GenerateKey(rand.Reader)
	if err = a.Sign("key", priv); err != nil {
		t.Fatal(err)
	}
	raw, err := a.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	return a, raw, priv
}

type sim struct {
	log *[]string
	err error
	fp  string
}

func (s sim) Simulate(context.Context, plan.Plan) (string, error) {
	*s.log = append(*s.log, "simulate")
	return s.fp, s.err
}

type safe struct {
	log *[]string
	err error
}

func (s safe) Analyze(context.Context, plan.Plan) error {
	*s.log = append(*s.log, "safety")
	return s.err
}

type bind struct {
	log *[]string
	err error
}

func (b bind) Bind(_ context.Context, p plan.Plan) (precheck.Plan, string, error) {
	*b.log = append(*b.log, "bind")
	return precheck.Plan{Digest: "checks"}, "bundle", b.err
}
func TestEditInvalidatesAuthorizationAndRetainsProvenance(t *testing.T) {
	a, raw, _ := fixture(t)
	sql := "  " + a.Plan.Steps[0].SQL + "\n"
	e, err := New(raw, a, sql, "edit.sql", Editor{Identity: "editor", At: time.Now().UTC(), Reason: "reviewed"})
	if err != nil {
		t.Fatal(err)
	}
	if e.Digest == a.Digest || string(e.OriginalGeneratedArtifact) != string(raw) || len(e.Provenance) != 1 || e.Provenance[0].ParentDigest != a.Plan.Digest {
		t.Fatalf("edit=%+v", e)
	}
	e2, err := e.Edit(sql+" ", "edit2.sql", Editor{Identity: "editor2", At: time.Now().UTC(), Reason: "followup"})
	if err != nil {
		t.Fatal(err)
	}
	if len(e2.Provenance) != 2 || e2.Provenance[1].ParentDigest != e.Digest || string(e2.OriginalGeneratedArtifact) != string(raw) {
		t.Fatal("provenance chain lost")
	}
}
func TestRevalidationFixedOrderAndFailureStopsLaterStages(t *testing.T) {
	a, raw, _ := fixture(t)
	e, err := New(raw, a, a.Plan.Steps[0].SQL, "edit.sql", Editor{Identity: "editor", At: time.Now().UTC(), Reason: "review"})
	if err != nil {
		t.Fatal(err)
	}
	for _, stage := range []string{"simulate", "safety", "bind"} {
		t.Run(stage, func(t *testing.T) {
			var log []string
			p := Pipeline{Simulator: sim{log: &log, fp: a.Plan.ToFingerprint}, Safety: safe{log: &log}, Binder: bind{log: &log}}
			switch stage {
			case "simulate":
				p.Simulator = sim{log: &log, err: errors.New("x")}
			case "safety":
				p.Safety = safe{log: &log, err: errors.New("x")}
			case "bind":
				p.Binder = bind{log: &log, err: errors.New("x")}
			}
			if _, err = p.Revalidate(context.Background(), e); err == nil {
				t.Fatal("failure accepted")
			}
			want := map[string]int{"simulate": 1, "safety": 2, "bind": 3}[stage]
			if len(log) != want {
				t.Fatalf("log=%v", log)
			}
		})
	}
}
func TestSuccessfulEditRequiresFreshApproval(t *testing.T) {
	a, raw, _ := fixture(t)
	e, _ := New(raw, a, " "+a.Plan.Steps[0].SQL, "edit.sql", Editor{Identity: "editor", At: time.Now().UTC(), Reason: "review"})
	var log []string
	eligible, err := (Pipeline{Simulator: sim{log: &log, fp: a.Plan.ToFingerprint}, Safety: safe{log: &log}, Binder: bind{log: &log}}).Revalidate(context.Background(), e)
	if err != nil {
		t.Fatal(err)
	}
	fresh, err := eligible.FreshArtifact(a.CreatedAt, a.ExpiresAt, a.SourceRevision, a.TargetEnvironment, a.DatabaseIdentity, artifact.Approval{Identity: "bob", ApprovedAt: a.CreatedAt})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Digest == a.Digest || fresh.Signature.Value != "" || fresh.Approval.Identity == a.Approval.Identity || !IsEdited(fresh) {
		t.Fatalf("fresh=%+v", fresh)
	}
}
