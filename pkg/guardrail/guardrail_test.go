package guardrail

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/policy"
	"autosql/pkg/precheck"
	"autosql/pkg/safety"
	"autosql/pkg/schema"
)

type eventLog struct{ values []string }

func (l *eventLog) add(v string) { l.values = append(l.values, v) }

type authority struct {
	log  *eventLog
	seen bool
}

func (a *authority) ResolveActor(_ context.Context, id string) (approval.Identity, error) {
	if !a.seen {
		a.log.add("approval")
		a.seen = true
	}
	switch id {
	case "author":
		return approval.Identity{ID: id}, nil
	case "deployer":
		return approval.Identity{ID: id}, nil
	case "dba":
		return approval.Identity{ID: id, Roles: []string{"dba"}}, nil
	}
	return approval.Identity{}, errors.New("unknown actor")
}
func (a *authority) VerifyApproval(_ context.Context, v approval.Approval) (approval.VerifiedApproval, error) {
	id, err := a.ResolveActor(context.Background(), v.Approver)
	if err != nil || v.Proof != "signed" {
		return approval.VerifiedApproval{}, errors.New("bad proof")
	}
	return approval.VerifiedApproval{Identity: id, PlanDigest: v.PlanDigest, Environment: v.Environment, ApprovedAt: v.ApprovedAt, ExpiresAt: v.ExpiresAt}, nil
}

type audit struct {
	log    *eventLog
	fail   bool
	cancel context.CancelFunc
	events []approval.Event
}

func (a *audit) AppendDurable(_ context.Context, e approval.Event) (approval.AuditRecord, error) {
	a.log.add("audit")
	if a.fail {
		return approval.AuditRecord{}, errors.New("audit password=hidden")
	}
	a.events = append(a.events, e)
	if a.cancel != nil {
		a.cancel()
	}
	return approval.AuditRecord{Sequence: uint64(len(a.events)), Event: e}, nil
}

type database struct {
	log      *eventLog
	count    int64
	queryErr error
	tx       *transaction
}

func (d *database) Begin(context.Context) (precheck.Tx, error) {
	d.log.add("begin")
	d.tx = &transaction{db: d}
	return d.tx, nil
}

type transaction struct {
	db    *database
	execs int
}

func (t *transaction) AcquireLock(context.Context) error { t.db.log.add("lock"); return nil }
func (t *transaction) QueryCount(context.Context, string, ...any) (int64, error) {
	t.db.log.add("check")
	return t.db.count, t.db.queryErr
}
func (t *transaction) Exec(context.Context, string) error {
	t.db.log.add("statement")
	t.execs++
	return nil
}
func (t *transaction) Commit(context.Context) error   { t.db.log.add("commit"); return nil }
func (t *transaction) Rollback(context.Context) error { t.db.log.add("rollback"); return nil }

func testChanges() schema.ChangeSet {
	r := schema.Resource{Kind: schema.KindTable, Name: schema.Name{Schema: "public", Name: "widgets"}, Spec: json.RawMessage(`{}`)}
	r.ID = schema.StableID(r.Kind, r.Name)
	return schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "create-widgets", Operation: schema.OperationCreate, ResourceID: r.ID, After: &r}}}
}

func boundInput(t *testing.T, environment string) (Input, string) {
	t.Helper()
	changes := testChanges()
	changeDigest, err := ChangeDigest(changes)
	if err != nil {
		t.Fatal(err)
	}
	p := precheck.Plan{ID: "plan-1", ChangeDigest: changeDigest, Statements: []string{"ALTER TABLE widgets ADD COLUMN label text"}, Assertions: []precheck.Assertion{{Name: "no forbidden rows", Query: "SELECT count(*) FROM widgets WHERE label = $1", Args: []any{"forbidden"}, MaxAllowed: 0, ChangeDigest: changeDigest, Timeout: time.Second, Source: precheck.Source{File: "migration.sql", Line: 4, Column: 2}}}}
	p.Digest, err = precheck.Digest(p)
	if err != nil {
		t.Fatal(err)
	}
	p.Assertions[0].PlanDigest = p.Digest
	approvalDigest, err := ApprovalDigest(changeDigest, p.Digest, environment)
	if err != nil {
		t.Fatal(err)
	}
	req := approval.Request{Plan: approval.Plan{Digest: approvalDigest, Environment: environment, Author: "author", Risk: approval.RiskCritical}, RequestedBy: "deployer"}
	return Input{Changes: changes, Safety: safety.Input{Statements: []safety.Statement{{ChangeID: "create-widgets", SQL: p.Statements[0]}}}, Policy: policy.Document{Version: policy.LanguageVersion}, Precheck: p, Approval: req}, approvalDigest
}

func harness(t *testing.T, log *eventLog) (Guardrail, *database, *audit) {
	t.Helper()
	analyzer := safety.AnalyzerFunc{ID: "order", Fn: func(context.Context, safety.Input) ([]safety.Diagnostic, error) { log.add("analyze"); return nil, nil }}
	policySeen := false
	ev := policy.Evaluator{Now: func() time.Time {
		if !policySeen {
			log.add("policy")
			policySeen = true
		}
		return time.Unix(100, 0)
	}}
	a := &audit{log: log}
	authority := &authority{log: log}
	gate := approval.Gate{Now: func() time.Time { return time.Unix(100, 0) }, Audit: a, Authority: authority, Policy: approval.Policy{Environments: map[string]approval.EnvironmentPolicy{"prod": {Allowed: true}}}}
	db := &database{log: log}
	return Guardrail{Config: Config{Environment: "prod", FailOn: safety.SeverityError, Risk: RiskConfig{Baseline: approval.RiskLow}}, Safety: safety.Runner{Analyzers: []safety.Analyzer{analyzer}}, Policy: ev, Approval: gate}, db, a
}

func TestSuccessfulProductionOrderAndDerivedRisk(t *testing.T) {
	log := &eventLog{}
	g, db, _ := harness(t, log)
	in, _ := boundInput(t, "prod")
	in.Database = db
	result, err := g.Apply(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"analyze", "policy", "approval", "audit", "begin", "lock", "check", "statement", "commit"}
	if fmt.Sprint(log.values) != fmt.Sprint(want) {
		t.Fatalf("events=%v want=%v", log.values, want)
	}
	if result.Risk != approval.RiskLow || in.Approval.Plan.Risk != approval.RiskCritical {
		t.Fatalf("derived=%v caller=%v", result.Risk, in.Approval.Plan.Risk)
	}
	if len(result.Checks) != 1 || !result.Checks[0].Passed {
		t.Fatalf("result=%+v", result)
	}
}

func TestEveryStaticFailurePreventsDatabaseWork(t *testing.T) {
	tests := map[string]struct {
		mutate func(*Guardrail, *Input, *audit)
		kind   error
	}{
		"changes mutated": {func(_ *Guardrail, in *Input, _ *audit) {
			in.Changes.Changes[0].Annotations = map[string]string{"changed": "true"}
		}, ErrBinding},
		"change digest":    {func(_ *Guardrail, in *Input, _ *audit) { in.Precheck.ChangeDigest = "sha256:other" }, ErrBinding},
		"plan digest":      {func(_ *Guardrail, in *Input, _ *audit) { in.Precheck.Statements[0] = "DROP TABLE secret" }, ErrBinding},
		"assertion digest": {func(_ *Guardrail, in *Input, _ *audit) { in.Precheck.Assertions[0].PlanDigest = "sha256:other" }, ErrBinding},
		"safety SQL":       {func(_ *Guardrail, in *Input, _ *audit) { in.Safety.Statements[0].SQL = "SELECT 1" }, ErrBinding},
		"approval digest":  {func(_ *Guardrail, in *Input, _ *audit) { in.Approval.Plan.Digest = "sha256:other" }, ErrBinding},
		"environment":      {func(_ *Guardrail, in *Input, _ *audit) { in.Approval.Plan.Environment = "stage" }, ErrBinding},
		"safety": {func(g *Guardrail, _ *Input, _ *audit) {
			g.Safety = safety.Runner{Analyzers: []safety.Analyzer{safety.AnalyzerFunc{ID: "unsafe", Fn: func(context.Context, safety.Input) ([]safety.Diagnostic, error) {
				return []safety.Diagnostic{diagnostic(safety.SeverityError)}, nil
			}}}}
		}, ErrSafety},
		"policy": {func(_ *Guardrail, in *Input, _ *audit) {
			in.Policy = denyingPolicy()
			in.SchemaResources = []policy.Resource{{Kind: "table", Name: "widgets", Owner: "wrong"}}
		}, ErrPolicy},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			log := &eventLog{}
			g, db, a := harness(t, log)
			in, _ := boundInput(t, "prod")
			in.Database = db
			tc.mutate(&g, &in, a)
			_, err := g.Apply(context.Background(), in)
			if !errors.Is(err, tc.kind) {
				t.Fatalf("error=%T %v", err, err)
			}
			for _, event := range log.values {
				if event == "begin" || event == "statement" {
					t.Fatalf("database work on failure: %v", log.values)
				}
			}
		})
	}
}

func TestApprovalAndAuditFailuresPrecedeDatabase(t *testing.T) {
	for _, name := range []string{"approval", "audit"} {
		t.Run(name, func(t *testing.T) {
			log := &eventLog{}
			g, db, a := harness(t, log)
			in, _ := boundInput(t, "prod")
			in.Database = db
			if name == "approval" {
				g.Approval.Policy.Environments["prod"] = approval.EnvironmentPolicy{Allowed: true, Requirements: []approval.Requirement{{MinimumRisk: approval.RiskLow, ApproverCount: 1, Roles: []string{"dba"}}}}
			} else {
				a.fail = true
			}
			_, err := g.Apply(context.Background(), in)
			if !errors.Is(err, ErrApproval) {
				t.Fatalf("error=%T %v", err, err)
			}
			if name == "audit" && strings.Contains(err.Error(), "password") {
				t.Fatalf("audit detail leaked: %v", err)
			}
			if db.tx != nil {
				t.Fatalf("database began: %v", log.values)
			}
		})
	}
}

func TestRealBuiltinSafetyBlocksDestructiveChange(t *testing.T) {
	log := &eventLog{}
	g, db, _ := harness(t, log)
	in, _ := boundInput(t, "prod")
	r := *in.Changes.Changes[0].After
	in.Changes.Changes[0] = schema.Change{ID: "drop-widgets", Operation: schema.OperationDrop, ResourceID: r.ID, Before: &r}
	rebind(t, &in, "prod")
	in.Safety.Statements[0].ChangeID = "drop-widgets"
	in.Database = db
	g.Safety = safety.Runner{Analyzers: safety.Builtins()}
	result, err := g.Apply(context.Background(), in)
	if !errors.Is(err, ErrSafety) || len(result.Diagnostics) == 0 || db.tx != nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestFailedLiveCheckAuditedButNeverMutates(t *testing.T) {
	log := &eventLog{}
	g, db, _ := harness(t, log)
	db.count = 1
	in, _ := boundInput(t, "prod")
	in.Database = db
	result, err := g.Apply(context.Background(), in)
	if !errors.Is(err, ErrPrecheck) || db.tx == nil || db.tx.execs != 0 {
		t.Fatalf("result=%+v err=%v execs=%v", result, err, db.tx)
	}
	want := []string{"analyze", "policy", "approval", "audit", "begin", "lock", "check", "rollback"}
	if fmt.Sprint(log.values) != fmt.Sprint(want) {
		t.Fatalf("events=%v", log.values)
	}
	if len(result.Checks) != 1 || result.Checks[0].Passed {
		t.Fatalf("checks=%+v", result.Checks)
	}
}

func TestCancellationAfterAuditPreventsDatabase(t *testing.T) {
	log := &eventLog{}
	g, db, a := harness(t, log)
	ctx, cancel := context.WithCancel(context.Background())
	a.cancel = cancel
	in, _ := boundInput(t, "prod")
	in.Database = db
	_, err := g.Apply(ctx, in)
	if !errors.Is(err, context.Canceled) || db.tx != nil {
		t.Fatalf("error=%v events=%v", err, log.values)
	}
}

func TestSuppressedDiagnosticDoesNotBlockOrRaiseRisk(t *testing.T) {
	log := &eventLog{}
	g, db, _ := harness(t, log)
	d := diagnostic(safety.SeverityError)
	g.Safety = safety.Runner{Analyzers: []safety.Analyzer{safety.AnalyzerFunc{ID: "suppressed", Fn: func(context.Context, safety.Input) ([]safety.Diagnostic, error) {
		log.add("analyze")
		return []safety.Diagnostic{d}, nil
	}}}, Suppressions: []safety.Suppression{{Rule: d.Rule, ObjectID: d.Object.ID, Reason: "approved test"}}}
	in, _ := boundInput(t, "prod")
	in.Database = db
	result, err := g.Apply(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if result.Risk != approval.RiskLow || result.Diagnostics[0].Suppressed == nil {
		t.Fatalf("result=%+v", result)
	}
}

func TestDerivedWarningRiskDrivesApprovalRequirement(t *testing.T) {
	log := &eventLog{}
	g, db, _ := harness(t, log)
	d := diagnostic(safety.SeverityWarning)
	g.Safety = safety.Runner{Analyzers: []safety.Analyzer{safety.AnalyzerFunc{ID: "warning", Fn: func(context.Context, safety.Input) ([]safety.Diagnostic, error) {
		log.add("analyze")
		return []safety.Diagnostic{d}, nil
	}}}}
	g.Approval.Policy.Environments["prod"] = approval.EnvironmentPolicy{Allowed: true, Requirements: []approval.Requirement{{MinimumRisk: approval.RiskMedium, ApproverCount: 1, Roles: []string{"dba"}}}}
	in, digest := boundInput(t, "prod")
	in.Database = db
	now := time.Unix(100, 0)
	in.Approval.Plan.Risk = approval.RiskLow
	in.Approval.Approvals = []approval.Approval{{PlanDigest: digest, Environment: "prod", Approver: "dba", ApprovedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), Proof: "signed"}}
	result, err := g.Apply(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if result.Risk != approval.RiskMedium {
		t.Fatalf("risk=%v", result.Risk)
	}
}

func TestErrorsAndResultsDoNotExposeSQLOrArguments(t *testing.T) {
	log := &eventLog{}
	g, db, _ := harness(t, log)
	in, _ := boundInput(t, "prod")
	secret := "ultra-secret-value"
	d := diagnostic(safety.SeverityWarning)
	d.Message = "password=" + secret
	d.Properties = map[string]any{"token": secret}
	g.Safety = safety.Runner{Analyzers: []safety.Analyzer{safety.AnalyzerFunc{ID: "secret-diagnostic", Fn: func(context.Context, safety.Input) ([]safety.Diagnostic, error) {
		return []safety.Diagnostic{d}, nil
	}}}}
	in.Precheck.Statements[0] = "SELECT '" + secret + "'"
	in.Safety.Statements[0].SQL = in.Precheck.Statements[0]
	in.Precheck.Assertions[0].Args = []any{secret}
	in.Precheck.Digest, _ = precheck.Digest(in.Precheck)
	in.Precheck.Assertions[0].PlanDigest = in.Precheck.Digest
	in.Approval.Plan.Digest, _ = ApprovalDigest(in.Precheck.ChangeDigest, in.Precheck.Digest, "prod")
	db.count = 1
	in.Database = db
	result, err := g.Apply(context.Background(), in)
	combined := fmt.Sprintf("%v %+v", err, result)
	if strings.Contains(combined, secret) || strings.Contains(combined, "SELECT") {
		t.Fatalf("sensitive evidence leaked: %s", combined)
	}
}

func diagnostic(severity safety.Severity) safety.Diagnostic {
	changes := testChanges()
	r := changes.Changes[0].After
	return safety.Diagnostic{Rule: "TEST001", Severity: severity, Message: "risk", Object: safety.Object{ID: r.ID, Kind: r.Kind, Name: r.Name.String()}, Impact: "impact", Remediation: "fix", Confidence: safety.ConfidenceHigh}
}
func rebind(t *testing.T, in *Input, environment string) {
	t.Helper()
	digest, err := ChangeDigest(in.Changes)
	if err != nil {
		t.Fatal(err)
	}
	in.Precheck.ChangeDigest = digest
	for i := range in.Precheck.Assertions {
		in.Precheck.Assertions[i].ChangeDigest = digest
		in.Precheck.Assertions[i].PlanDigest = ""
	}
	in.Precheck.Digest, err = precheck.Digest(in.Precheck)
	if err != nil {
		t.Fatal(err)
	}
	for i := range in.Precheck.Assertions {
		in.Precheck.Assertions[i].PlanDigest = in.Precheck.Digest
	}
	in.Approval.Plan.Digest, err = ApprovalDigest(digest, in.Precheck.Digest, environment)
	if err != nil {
		t.Fatal(err)
	}
}
func denyingPolicy() policy.Document {
	return policy.Document{Version: policy.LanguageVersion, Rules: []policy.Rule{{Name: "owner", Target: "schema", Kinds: []string{"table"}, Assert: policy.Expression{Eq: []any{"resource.owner", "required"}}, Message: "wrong owner", Severity: "error"}}}
}

func TestDigestIsCanonicalAndEnvironmentBound(t *testing.T) {
	a := testChanges()
	b := testChanges()
	b.Changes[0].Annotations = map[string]string{"a": "1", "b": "2"}
	c := testChanges()
	c.Changes[0].Annotations = map[string]string{"b": "2", "a": "1"}
	da, err := ChangeDigest(a)
	if err != nil {
		t.Fatal(err)
	}
	db, err := ChangeDigest(b)
	if err != nil {
		t.Fatal(err)
	}
	if da == db {
		t.Fatal("different exact changes shared a digest")
	}
	dc, err := ChangeDigest(c)
	if err != nil || dc != db {
		t.Fatalf("canonical map ordering changed digest: %s %s %v", db, dc, err)
	}
	x, _ := ApprovalDigest(da, "plan", "prod")
	y, _ := ApprovalDigest(da, "plan", "stage")
	z, _ := ApprovalDigest(da+"x", "plan", "prod")
	if x == y || x == z {
		t.Fatal("approval digest is not domain bound")
	}
}
