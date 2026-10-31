package executor

import (
	"context"
	"crypto/ed25519"
	"errors"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/guardrail"
	"autosql/pkg/plan"
	"autosql/pkg/plugin/sample"
	"autosql/pkg/precheck"
	"autosql/pkg/schema"

	"github.com/jackc/pgx/v5/pgconn"
)

type completionFixture struct {
	a        artifact.Artifact
	verified artifact.VerifiedObservation
	rows     [][]any
	events   []LifecycleEvent
	state    RuntimeState
	now      time.Time
}

func newCompletionFixture(t *testing.T) completionFixture {
	t.Helper()
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	desired := empty
	for _, name := range []string{"orders", "inventory"} {
		n := schema.Name{Name: name}
		desired.Graph.Resources = append(desired.Graph.Resources, schema.Resource{ID: schema.StableID(schema.KindSchema, n), Kind: schema.KindSchema, Name: n, Spec: []byte(`{}`)})
	}
	p, err := plan.Build(context.Background(), sample.Driver{}, empty, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	changeDigest, err := guardrail.ChangeDigest(p.Changes)
	if err != nil {
		t.Fatal(err)
	}
	checks := precheck.Plan{ID: "completion", ChangeDigest: changeDigest}
	for _, step := range p.Steps {
		checks.Statements = append(checks.Statements, step.SQL)
	}
	checks.Digest, err = precheck.Digest(checks)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	a, err := artifact.New(p, checks, created, created.Add(time.Hour), "revision", "test", "db", "sha256:"+strings.Repeat("a", 64), artifact.Approval{Identity: "reviewer", ApprovedAt: created}, map[string]string{"autosql.operator.mode": "transition"})
	if err != nil {
		t.Fatal(err)
	}
	pub, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Sign("key", private); err != nil {
		t.Fatal(err)
	}
	now := created.Add(2 * time.Hour)
	v, err := a.VerifyForObservation(artifact.VerifyPolicy{Now: func() time.Time { return now }, Expected: artifact.ExpectedBindings{PlanDigest: a.Plan.Digest, ChecksDigest: a.Checks.Digest, GuardrailDigest: a.GuardrailDigest, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: a.DatabaseIdentity, ApprovalIdentity: a.Approval.Identity}, Keys: map[string]artifact.KeyRecord{"key": {PublicKey: pub, Issuer: "issuer", Identity: "signer", Environment: "test", Purpose: "release", Status: "active", NotBefore: created, NotAfter: now.Add(time.Hour)}}, Issuer: "issuer", Identity: "signer", Purpose: "release"})
	if err != nil {
		t.Fatal(err)
	}
	f := completionFixture{a: a, verified: v, now: now, state: RuntimeState{Fingerprint: p.ToFingerprint, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: a.DatabaseIdentity}}
	add := func(typ, step string) {
		e := LifecycleEvent{Type: typ, StepID: step, ExecutionID: a.Digest, ArtifactDigest: a.Digest, PlanDigest: a.Plan.Digest, BundleDigest: a.GuardrailDigest, Target: "db/test", Attempt: 1, At: created.Add(time.Minute)}
		e.EventID = lifecycleEventID(e)
		f.events = append(f.events, e)
	}
	add("requested", "")
	add("lock_acquired", "")
	steps, _ := historyBindings(a)
	last := ""
	for _, phase := range p.Phases {
		add("transaction_started", "")
		last = phase.StepIDs[len(phase.StepIDs)-1]
		add("transaction_committed", last)
		for _, id := range phase.StepIDs {
			f.rows = append(f.rows, []any{id, stepHash(steps[id]), phase.ID, string(phase.Transaction), a.Digest, "db/test", a.Plan.Digest, a.GuardrailDigest, int64(1), "confirmed"})
			add("confirmed", id)
		}
	}
	add("completed", last)
	return f
}

// This reader has no mutation methods, so the observation path cannot execute
// SQL or initialize history through it.
type completionReader struct {
	rows            *valueRows
	queryErr        error
	state, final    RuntimeState
	stateErr        error
	queries, states int
	query           string
	args            []any
}

func (r *completionReader) Query(_ context.Context, query string, args ...any) (Rows, error) {
	r.queries++
	r.query, r.args = query, args
	if r.queryErr != nil {
		return nil, r.queryErr
	}
	rows := *r.rows
	rows.i = 0
	return &rows, nil
}
func (r *completionReader) State(context.Context) (RuntimeState, error) {
	r.states++
	if r.states > 1 && r.final.Fingerprint != "" {
		return r.final, r.stateErr
	}
	return r.state, r.stateErr
}

type completionAudit struct {
	events []LifecycleEvent
	err    error
}

func (a completionAudit) ReadExecution(context.Context, string) ([]LifecycleEvent, error) {
	return a.events, a.err
}

func TestCompletionObservationProvesExactDurableEvidence(t *testing.T) {
	f := newCompletionFixture(t)
	reader := &completionReader{rows: &valueRows{values: f.rows}, state: f.state}
	audit := &FileAudit{Path: filepath.Join(t.TempDir(), "lifecycle.jsonl")}
	for _, event := range f.events {
		if err := audit.AppendDurable(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	got := ObserveCompletion(context.Background(), f.verified, reader, audit, f.now)
	if got.Outcome != CompletionProven || got.ConfirmedSteps != len(f.a.Plan.Steps) || got.Attempt != 1 || got.ArtifactDigest != f.a.Digest || got.Fingerprint != f.state.Fingerprint || !got.ObservedAt.Equal(f.now) {
		t.Fatalf("completion=%+v", got)
	}
	if reader.queries != 2 || reader.states != 2 || strings.Contains(reader.query, "state='confirmed'") || !reflect.DeepEqual(reader.args, []any{f.a.Digest}) || !strings.HasPrefix(reader.query, "select ") {
		t.Fatalf("unexpected evidence access: %+v", reader)
	}
}

func TestCompletionObservationRefusesIncompleteOrConflictingEvidence(t *testing.T) {
	cases := map[string]func(*completionFixture, *completionReader, *completionAudit){
		"missing steps": func(_ *completionFixture, r *completionReader, _ *completionAudit) { r.rows.values = nil },
		"duplicate": func(f *completionFixture, r *completionReader, _ *completionAudit) {
			r.rows.values = append(r.rows.values, f.rows[0])
		},
		"intended":           func(_ *completionFixture, r *completionReader, _ *completionAudit) { r.rows.values[0][9] = "intended" },
		"ambiguous attempts": func(_ *completionFixture, r *completionReader, _ *completionAudit) { r.rows.values[1][8] = int64(2) },
		"zero attempt":       func(_ *completionFixture, r *completionReader, _ *completionAudit) { r.rows.values[0][8] = int64(0) },
		"unknown step":       func(_ *completionFixture, r *completionReader, _ *completionAudit) { r.rows.values[0][0] = "unknown" },
		"scan error": func(_ *completionFixture, r *completionReader, _ *completionAudit) {
			r.rows.scanErr = errors.New("password=secret")
		},
		"drift": func(_ *completionFixture, r *completionReader, _ *completionAudit) { r.state.Fingerprint = "other" },
		"drift during audit": func(f *completionFixture, r *completionReader, _ *completionAudit) {
			r.final = f.state
			r.final.Fingerprint = "other"
		},
		"wrong runtime target": func(_ *completionFixture, r *completionReader, _ *completionAudit) {
			r.state.DatabaseIdentity = "other"
		},
		"missing audit": func(_ *completionFixture, _ *completionReader, a *completionAudit) { a.events = nil },
		"missing completion": func(_ *completionFixture, _ *completionReader, a *completionAudit) {
			a.events = a.events[:len(a.events)-1]
		},
		"missing commit audit": func(_ *completionFixture, _ *completionReader, a *completionAudit) {
			a.events = append(a.events[:3], a.events[4:]...)
		},
		"uncertain execution": func(_ *completionFixture, _ *completionReader, a *completionAudit) {
			event := a.events[len(a.events)-1]
			event.Type = "uncertain"
			event.EventID = lifecycleEventID(event)
			a.events = append(a.events, event)
		},
		"unknown confirmed step": func(_ *completionFixture, _ *completionReader, a *completionAudit) {
			event := a.events[len(a.events)-1]
			event.Type, event.StepID = "confirmed", "unknown"
			event.EventID = lifecycleEventID(event)
			a.events = append(a.events, event)
		},
		"completion before steps": func(_ *completionFixture, _ *completionReader, a *completionAudit) {
			a.events[0], a.events[len(a.events)-1] = a.events[len(a.events)-1], a.events[0]
		},
		"audit attempt": func(_ *completionFixture, _ *completionReader, a *completionAudit) { a.events[0].Attempt = 2 },
		"audit binding": func(_ *completionFixture, _ *completionReader, a *completionAudit) { a.events[0].Target = "other" },
		"audit future time": func(f *completionFixture, _ *completionReader, a *completionAudit) {
			a.events[0].At = f.now.Add(time.Second)
		},
		"audit duplicate": func(_ *completionFixture, _ *completionReader, a *completionAudit) {
			a.events = append(a.events, a.events[0])
		},
		"audit corruption": func(_ *completionFixture, _ *completionReader, a *completionAudit) { a.err = ErrAuditEvidence },
	}
	for i, field := range []string{"step hash", "phase", "mode", "execution", "target", "plan", "bundle"} {
		cases[field] = func(_ *completionFixture, r *completionReader, _ *completionAudit) {
			r.rows.values[0][i+1] = "tampered"
		}
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			f := newCompletionFixture(t)
			r, a := &completionReader{rows: &valueRows{values: f.rows}, state: f.state}, &completionAudit{events: f.events}
			mutate(&f, r, a)
			got := ObserveCompletion(context.Background(), f.verified, r, a, f.now)
			if got.Outcome != CompletionRefused || got.ArtifactDigest != "" || strings.Contains(got.Reason, "secret") {
				t.Fatalf("bad evidence=%+v", got)
			}
		})
	}
}

func TestCompletionObservationClassifiesUnsupportedAndUnavailableEvidence(t *testing.T) {
	f := newCompletionFixture(t)
	for _, code := range []string{"42P01", "42703", "08006"} {
		r := &completionReader{queryErr: &pgconn.PgError{Code: code, Message: "password=secret"}}
		got := ObserveCompletion(context.Background(), f.verified, r, completionAudit{}, f.now)
		want := CompletionIneligible
		if code == "08006" {
			want = CompletionRetryable
		}
		if got.Outcome != want || r.states != 0 || strings.Contains(got.Reason, "secret") {
			t.Fatalf("code=%s result=%+v", code, got)
		}
	}
	for _, mode := range []string{"bootstrap", "adopt", "unknown"} {
		f.a.Metadata["autosql.operator.mode"] = mode
		if completionEligible(f.a) {
			t.Fatalf("unsupported mode %s accepted", mode)
		}
	}
	f.a.Metadata["autosql.operator.mode"] = "transition"
	f.a.Metadata["autosql.migration.manifest"] = "manifest"
	if completionEligible(f.a) {
		t.Fatal("versioned migration accepted")
	}
	delete(f.a.Metadata, "autosql.migration.manifest")
	f.a.Plan.Steps[0].Kind = plan.StepTopology
	if completionEligible(f.a) {
		t.Fatal("topology plan accepted")
	}
	f.a.Plan.Steps = nil
	if completionEligible(f.a) {
		t.Fatal("empty plan accepted")
	}
	r := &completionReader{}
	if got := ObserveCompletion(context.Background(), artifact.VerifiedObservation{}, r, completionAudit{}, f.now); got.Outcome != CompletionRefused || r.queries != 0 {
		t.Fatalf("unverified artifact accessed evidence: %+v", got)
	}
}

func TestCompletionObservationRejectsUnboundPostgreSQLTargetBeforeConnect(t *testing.T) {
	f := newCompletionFixture(t)
	base := PostgreSQLCompletionConfig{URL: "invalid connection string", DatabaseIdentity: f.a.DatabaseIdentity, Environment: f.a.TargetEnvironment, SourceRevision: f.a.SourceRevision, Schemas: []string{"app"}, Audit: completionAudit{}}
	for _, field := range []string{"database", "environment", "revision"} {
		config := base
		switch field {
		case "database":
			config.DatabaseIdentity = "other"
		case "environment":
			config.Environment = "other"
		case "revision":
			config.SourceRevision = "other"
		}
		got := ObservePostgreSQLCompletion(context.Background(), f.verified, config)
		if got.Outcome != CompletionRefused || got.Reason != "authorization_invalid" || got.ArtifactDigest != "" {
			t.Fatalf("unbound %s reached connection or proof: %+v", field, got)
		}
	}
}
