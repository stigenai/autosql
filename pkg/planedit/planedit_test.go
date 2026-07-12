package planedit

import (
	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/guardrail"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/precheck"
	"autosql/pkg/schema"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"
)

func fixture(t *testing.T) (artifact.Artifact, []byte, ed25519.PrivateKey) {
	t.Helper()
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	n := schema.Name{Name: "app"}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{{ID: schema.StableID(schema.KindSchema, n), Kind: schema.KindSchema, Name: n, Spec: []byte(`{}`)}}}}
	empty, _ = postgres.New().Normalize(context.Background(), empty)
	desired, _ = postgres.New().Normalize(context.Background(), desired)
	p, err := plan.Build(context.Background(), postgres.New(), empty, desired, plan.Options{})
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

func TestDraftValidateSurvivesReloadAndRejectsTampering(t *testing.T) {
	a, raw, _ := fixture(t)
	e, err := New(raw, a, a.Plan.Steps[0].SQL, "edit.sql", Editor{Identity: "editor", At: time.Now().UTC(), Reason: "review"})
	if err != nil {
		t.Fatal(err)
	}
	encoded, _ := json.Marshal(e)
	var loaded EditedArtifact
	if err = json.Unmarshal(encoded, &loaded); err != nil || loaded.Validate() != nil {
		t.Fatalf("reload=%v validate=%v", err, loaded.Validate())
	}
	for name, mutate := range map[string]func(*EditedArtifact){"digest": func(x *EditedArtifact) { x.Digest = "sha256:" + strings.Repeat("0", 64) }, "original": func(x *EditedArtifact) { x.OriginalGeneratedArtifact[0] ^= 1 }, "reason": func(x *EditedArtifact) { x.Provenance[0].Reason = "changed" }, "sql": func(x *EditedArtifact) { x.EditedSQL = "DROP TABLE secret" }} {
		t.Run(name, func(t *testing.T) {
			var x EditedArtifact
			_ = json.Unmarshal(encoded, &x)
			mutate(&x)
			if x.Validate() == nil {
				t.Fatal("tamper accepted")
			}
		})
	}
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
	cd, _ := guardrail.ChangeDigest(p.Changes)
	var statements []string
	for _, s := range p.Steps {
		if s.Kind == plan.StepExecutable {
			statements = append(statements, s.SQL)
		}
	}
	checks := precheck.Plan{ID: "checks", ChangeDigest: cd, Statements: statements}
	checks.Digest, _ = precheck.Digest(checks)
	return checks, "sha256:" + strings.Repeat("c", 64), b.err
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

func TestPipelineExplicitStageFailureMatrix(t *testing.T) {
	a, raw, _ := fixture(t)
	e, err := New(raw, a, a.Plan.Steps[0].SQL, "edit.sql", Editor{Identity: "editor", At: time.Now().UTC(), Reason: "review"})
	if err != nil {
		t.Fatal(err)
	}
	order := []string{"parse", "ast_bind", "rebind", "simulation", "fingerprint", "safety", "policy", "precheck", "guardrail"}
	for failAt := range order {
		t.Run(order[failAt], func(t *testing.T) {
			var stages, calls []string
			p := Pipeline{Simulator: sim{log: &calls, fp: a.Plan.ToFingerprint}, Safety: safe{log: &calls}, Binder: bind{log: &calls}, Stage: func(name string) error {
				stages = append(stages, name)
				if name == order[failAt] {
					return errors.New("injected")
				}
				return nil
			}}
			if _, err := p.Revalidate(context.Background(), e); err == nil {
				t.Fatal("injected failure accepted")
			}
			if !reflect.DeepEqual(stages, order[:failAt+1]) {
				t.Fatalf("stages=%v want=%v", stages, order[:failAt+1])
			}
			if failAt < 3 && len(calls) != 0 {
				t.Fatalf("later mutation-capable dependency called: %v", calls)
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
	approved := time.Now().UTC().Add(time.Minute)
	fresh, err := eligible.FreshArtifact(approved, approved.Add(time.Hour), a.SourceRevision, a.TargetEnvironment, a.DatabaseIdentity, artifact.Approval{Identity: "bob", ApprovedAt: approved, ProofDigest: "sha256:" + strings.Repeat("9", 64)})
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Digest == a.Digest || fresh.Signature.Value != "" || fresh.Approval.Identity == a.Approval.Identity || !IsEdited(fresh) {
		t.Fatalf("fresh=%+v", fresh)
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	if err = fresh.Sign("edited-key", priv); err != nil {
		t.Fatal(err)
	}
	raw, err = fresh.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := artifact.Parse(raw)
	if err != nil || parsed.EditProvenance == nil || string(parsed.EditProvenance.OriginalArtifact) != string(e.OriginalGeneratedArtifact) {
		t.Fatalf("published provenance err=%v", err)
	}
	tampered := parsed
	tampered.EditProvenance.OriginalArtifact[0] ^= 1
	if err = tampered.Sign("edited-key", priv); err == nil {
		t.Fatal("tampered provenance signed")
	}
	policy := artifact.VerifyPolicy{Now: func() time.Time { return approved }, Expected: artifact.ExpectedBindings{PlanDigest: fresh.Plan.Digest, ChecksDigest: fresh.Checks.Digest, GuardrailDigest: fresh.GuardrailDigest, SourceRevision: fresh.SourceRevision, Environment: fresh.TargetEnvironment, DatabaseIdentity: fresh.DatabaseIdentity, ApprovalIdentity: fresh.Approval.Identity}, Keys: map[string]artifact.KeyRecord{"edited-key": {PublicKey: pub, Issuer: "issuer", Identity: "signer", Environment: fresh.TargetEnvironment, Purpose: "plan-artifact", Status: "active", NotBefore: approved.Add(-time.Hour), NotAfter: approved.Add(time.Hour)}}, Issuer: "issuer", Identity: "signer", Purpose: "plan-artifact"}
	policy.ExpectedValidationAttestations = map[string]artifact.ValidationAttestation{}
	for _, att := range fresh.EditProvenance.Attestations {
		policy.ExpectedValidationAttestations[att.Stage] = att
	}
	v, err := fresh.VerifyTrusted(policy)
	if err != nil {
		t.Fatal(err)
	}
	for stage, att := range policy.ExpectedValidationAttestations {
		if att.Simulation != nil {
			copy := *att.Simulation
			copy.DevelopmentIdentity = "drifted/development"
			att.Simulation = &copy
			policy.ExpectedValidationAttestations[stage] = att
			if _, driftErr := fresh.VerifyTrusted(policy); driftErr == nil {
				t.Fatal("typed validation drift accepted")
			}
			copy.DevelopmentIdentity = fresh.EditProvenance.Attestations[1].Simulation.DevelopmentIdentity
			att.Simulation = &copy
			policy.ExpectedValidationAttestations[stage] = att
			break
		}
	}
	if _, err = executor.NewPostgreSQL(executor.Config{URL: "unused", State: func(context.Context, executor.Session) (executor.RuntimeState, error) {
		return executor.RuntimeState{}, nil
	}, NoEdits: true}, v); err == nil {
		t.Fatal("executor no-edits accepted edited artifact")
	}
	policy.NoEdits = true
	if _, err = fresh.VerifyTrusted(policy); err == nil {
		t.Fatal("typed provenance bypassed no-edits")
	}
}

func TestPostgreSQLParserRejectsUnsafeEditedStatements(t *testing.T) {
	a, raw, _ := fixture(t)
	cases := map[string]string{"syntax": "CREATE TABLE (", "transaction": "BEGIN", "session": "SET search_path=public", "role": "CREATE ROLE attacker", "advisory": "SELECT pg_advisory_lock(1)", "history": "DELETE FROM autosql_migration_history", "copy_meta": "COPY x FROM STDIN", "dml": "INSERT INTO t VALUES (1)", "procedural": "DO $$ BEGIN NULL; END $$", "function": "CREATE FUNCTION pwn() RETURNS void LANGUAGE SQL AS 'DELETE FROM t'", "wrong_object": "CREATE SCHEMA evil", "if_not_exists": "CREATE SCHEMA IF NOT EXISTS app", "authorization": "CREATE SCHEMA app AUTHORIZATION CURRENT_USER", "comment_spoof": "CREATE /* SCHEMA app */ SCHEMA evil"}
	for name, sql := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := New(raw, a, sql, "edit.sql", Editor{Identity: "editor", At: time.Now().UTC(), Reason: "review"}); err == nil {
				t.Fatal("unsafe edit accepted")
			}
		})
	}
}

func TestCommentCannotChangeTransactionMetadata(t *testing.T) {
	a, raw, _ := fixture(t)
	sql := "-- CONCURRENTLY\n" + a.Plan.Steps[0].SQL
	e, err := New(raw, a, sql, "edit.sql", Editor{Identity: "editor", At: time.Now().UTC(), Reason: "format only"})
	if err != nil {
		t.Fatal(err)
	}
	if e.CandidatePlan.Steps[0].Transaction != a.Plan.Steps[0].Transaction || e.CandidatePlan.Steps[0].Lock != a.Plan.Steps[0].Lock || e.CandidatePlan.Steps[0].Impact != a.Plan.Steps[0].Impact {
		t.Fatal("comment changed derived execution metadata")
	}
}
