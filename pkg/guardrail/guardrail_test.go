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

func boundInput(t *testing.T, g Guardrail, environment string) (Input, string) {
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
	req := approval.Request{Plan: approval.Plan{Environment: environment, Author: "author", Risk: approval.RiskCritical}, RequestedBy: "deployer"}
	in := Input{Changes: changes, Safety: safety.Input{Statements: []safety.Statement{{ChangeID: "create-widgets", SQL: p.Statements[0]}}}, Policy: policy.Document{Version: policy.LanguageVersion}, PolicyIdentity: "policy/main@v1", Precheck: p, Approval: req}
	in.StatementBindings, err = BuildStatementBindings(changes, in.Safety.Statements)
	if err != nil {
		t.Fatal(err)
	}
	in.Approval.Plan.Digest, err = g.BundleDigest(in)
	if err != nil {
		t.Fatal(err)
	}
	return in, in.Approval.Plan.Digest
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
	in, _ := boundInput(t, g, "prod")
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
		"change digest":          {func(_ *Guardrail, in *Input, _ *audit) { in.Precheck.ChangeDigest = "sha256:other" }, ErrBinding},
		"plan digest":            {func(_ *Guardrail, in *Input, _ *audit) { in.Precheck.Statements[0] = "DROP TABLE secret" }, ErrBinding},
		"assertion digest":       {func(_ *Guardrail, in *Input, _ *audit) { in.Precheck.Assertions[0].PlanDigest = "sha256:other" }, ErrBinding},
		"safety SQL":             {func(_ *Guardrail, in *Input, _ *audit) { in.Safety.Statements[0].SQL = "SELECT 1" }, ErrBinding},
		"statement change":       {func(_ *Guardrail, in *Input, _ *audit) { in.StatementBindings[0].ChangeID = "other" }, ErrBinding},
		"statement hash":         {func(_ *Guardrail, in *Input, _ *audit) { in.StatementBindings[0].ChangeHash = "sha256:other" }, ErrBinding},
		"statement bound SQL":    {func(_ *Guardrail, in *Input, _ *audit) { in.StatementBindings[0].SQL = "SELECT 1" }, ErrBinding},
		"approval digest":        {func(_ *Guardrail, in *Input, _ *audit) { in.Approval.Plan.Digest = "sha256:other" }, ErrBinding},
		"environment":            {func(_ *Guardrail, in *Input, _ *audit) { in.Approval.Plan.Environment = "stage" }, ErrBinding},
		"configured environment": {func(g *Guardrail, _ *Input, _ *audit) { g.Config.Environment = "stage" }, ErrBinding},
		"author":                 {func(_ *Guardrail, in *Input, _ *audit) { in.Approval.Plan.Author = "other" }, ErrBinding},
		"requester":              {func(_ *Guardrail, in *Input, _ *audit) { in.Approval.RequestedBy = "other" }, ErrBinding},
		"policy identity":        {func(_ *Guardrail, in *Input, _ *audit) { in.PolicyIdentity = "policy/other" }, ErrBinding},
		"policy document":        {func(_ *Guardrail, in *Input, _ *audit) { in.Policy.Variables = map[string]any{"mode": "other"} }, ErrBinding},
		"schema resources": {func(_ *Guardrail, in *Input, _ *audit) {
			in.SchemaResources = []policy.Resource{{Kind: "table", Name: "other"}}
		}, ErrBinding},
		"migration resources": {func(_ *Guardrail, in *Input, _ *audit) {
			in.MigrationResources = []policy.Resource{{Kind: "drop", Name: "other"}}
		}, ErrBinding},
		"severity threshold": {func(g *Guardrail, _ *Input, _ *audit) { g.Config.FailOn = safety.SeverityWarning }, ErrBinding},
		"risk baseline":      {func(g *Guardrail, _ *Input, _ *audit) { g.Config.Risk.Baseline = approval.RiskMedium }, ErrBinding},
		"risk mapping": {func(g *Guardrail, _ *Input, _ *audit) {
			g.Config.Risk.BySeverity = map[safety.Severity]approval.Risk{safety.SeverityWarning: approval.RiskHigh}
		}, ErrBinding},
		"target engine":  {func(_ *Guardrail, in *Input, _ *audit) { in.Safety.Target.Engine = "postgresql" }, ErrBinding},
		"target version": {func(_ *Guardrail, in *Input, _ *audit) { in.Safety.Target.Version = 15 }, ErrBinding},
		"target statistics": {func(_ *Guardrail, in *Input, _ *audit) {
			in.Safety.Target.Statistics = map[string]safety.TableStatistics{"table": {EstimatedRows: 10}}
		}, ErrBinding},
		"threshold lock":    {func(_ *Guardrail, in *Input, _ *audit) { in.Safety.Thresholds.MaxLockLevel = safety.LockShare }, ErrBinding},
		"threshold rows":    {func(_ *Guardrail, in *Input, _ *audit) { in.Safety.Thresholds.MaxRowsScanned = 10 }, ErrBinding},
		"threshold rewrite": {func(_ *Guardrail, in *Input, _ *audit) { in.Safety.Thresholds.MaxRewriteBytes = 10 }, ErrBinding},
		"analyzers": {func(g *Guardrail, _ *Input, _ *audit) {
			g.Safety.Analyzers[0] = safety.AnalyzerFunc{ID: "other", Fn: func(context.Context, safety.Input) ([]safety.Diagnostic, error) { return nil, nil }}
		}, ErrBinding},
		"suppressions": {func(g *Guardrail, _ *Input, _ *audit) {
			g.Safety.Suppressions = []safety.Suppression{{Rule: "TEST", ObjectID: "object", Reason: "changed"}}
		}, ErrBinding},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			log := &eventLog{}
			g, db, a := harness(t, log)
			in, _ := boundInput(t, g, "prod")
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

func TestSuppressionFieldsAreBundleBound(t *testing.T) {
	for _, field := range []string{"rule", "object", "reason", "expiry"} {
		t.Run(field, func(t *testing.T) {
			log := &eventLog{}
			g, db, _ := harness(t, log)
			expiry := time.Unix(200, 0)
			g.Safety.Suppressions = []safety.Suppression{{Rule: "TEST", ObjectID: "object", Reason: "approved", ExpiresAt: &expiry}}
			in, _ := boundInput(t, g, "prod")
			in.Database = db
			if field == "rule" {
				g.Safety.Suppressions[0].Rule = "OTHER"
			} else if field == "object" {
				g.Safety.Suppressions[0].ObjectID = "other"
			} else if field == "reason" {
				g.Safety.Suppressions[0].Reason = "changed"
			} else {
				changed := expiry.Add(time.Second)
				g.Safety.Suppressions[0].ExpiresAt = &changed
			}
			_, err := g.Apply(context.Background(), in)
			if !errors.Is(err, ErrBinding) || db.tx != nil {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestStatementBindingsRejectReorderingAndStaleChangeHash(t *testing.T) {
	for _, mode := range []string{"reordered", "stale hash"} {
		t.Run(mode, func(t *testing.T) {
			log := &eventLog{}
			g, db, _ := harness(t, log)
			in, _ := boundInput(t, g, "prod")
			in.Database = db
			if mode == "reordered" {
				in.Precheck.Statements = append(in.Precheck.Statements, "ALTER TABLE widgets ADD COLUMN note text")
				in.Safety.Statements = append(in.Safety.Statements, safety.Statement{ChangeID: "create-widgets", SQL: in.Precheck.Statements[1]})
				rebind(t, g, &in)
				in.StatementBindings[0], in.StatementBindings[1] = in.StatementBindings[1], in.StatementBindings[0]
			} else {
				in.Changes.Changes[0].Annotations = map[string]string{"revision": "new"}
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
				// Deliberately approve the bundle containing the stale binding. Apply
				// must independently recompute the referenced change hash.
				in.Approval.Plan.Digest, err = g.BundleDigest(in)
				if err != nil {
					t.Fatal(err)
				}
			}
			_, err := g.Apply(context.Background(), in)
			if !errors.Is(err, ErrBinding) || db.tx != nil {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestApprovalAndAuditFailuresPrecedeDatabase(t *testing.T) {
	for _, name := range []string{"approval", "audit"} {
		t.Run(name, func(t *testing.T) {
			log := &eventLog{}
			g, db, a := harness(t, log)
			in, _ := boundInput(t, g, "prod")
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
			if name == "audit" && strings.Contains(formatErrorChain(err), "password") {
				t.Fatalf("audit detail leaked: %v", err)
			}
			if db.tx != nil {
				t.Fatalf("database began: %v", log.values)
			}
		})
	}
}

func TestPolicyViolationPreventsApprovalAndDatabase(t *testing.T) {
	log := &eventLog{}
	g, db, _ := harness(t, log)
	in, _ := boundInput(t, g, "prod")
	in.Policy = denyingPolicy()
	in.SchemaResources = []policy.Resource{{Kind: "table", Name: "widgets", Owner: "wrong"}}
	rebind(t, g, &in)
	in.Database = db
	result, err := g.Apply(context.Background(), in)
	if !errors.Is(err, ErrPolicy) || len(result.Violations) != 1 || db.tx != nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

type unstableAnalyzer struct{ calls int }

func (a *unstableAnalyzer) Name() string { a.calls++; return fmt.Sprintf("unstable-%d", a.calls) }
func (a *unstableAnalyzer) Analyze(context.Context, safety.Input) ([]safety.Diagnostic, error) {
	return nil, nil
}

func TestBundleRejectsMissingOrUnstableIdentities(t *testing.T) {
	for _, name := range []string{"no analyzers", "empty policy", "empty author", "empty requester", "duplicate analyzers", "unstable analyzer"} {
		t.Run(name, func(t *testing.T) {
			log := &eventLog{}
			g, db, _ := harness(t, log)
			in, _ := boundInput(t, g, "prod")
			in.Database = db
			switch name {
			case "no analyzers":
				g.Safety.Analyzers = nil
			case "empty policy":
				in.PolicyIdentity = ""
			case "empty author":
				in.Approval.Plan.Author = ""
			case "empty requester":
				in.Approval.RequestedBy = ""
			case "duplicate analyzers":
				g.Safety.Analyzers = append(g.Safety.Analyzers, g.Safety.Analyzers[0])
			case "unstable analyzer":
				g.Safety.Analyzers = []safety.Analyzer{&unstableAnalyzer{}}
			}
			_, err := g.Apply(context.Background(), in)
			if !errors.Is(err, ErrConfig) || db.tx != nil {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestRealBuiltinSafetyBlocksDestructiveChange(t *testing.T) {
	log := &eventLog{}
	g, db, _ := harness(t, log)
	in, _ := boundInput(t, g, "prod")
	r := *in.Changes.Changes[0].After
	in.Changes.Changes[0] = schema.Change{ID: "drop-widgets", Operation: schema.OperationDrop, ResourceID: r.ID, Before: &r}
	in.Safety.Statements[0].ChangeID = "drop-widgets"
	g.Safety = safety.Runner{Analyzers: safety.Builtins()}
	rebind(t, g, &in)
	in.Database = db
	result, err := g.Apply(context.Background(), in)
	if !errors.Is(err, ErrSafety) || len(result.Diagnostics) == 0 || db.tx != nil {
		t.Fatalf("result=%+v err=%v", result, err)
	}
}

func TestFailedLiveCheckAuditedButNeverMutates(t *testing.T) {
	log := &eventLog{}
	g, db, _ := harness(t, log)
	db.count = 1
	in, _ := boundInput(t, g, "prod")
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
	in, _ := boundInput(t, g, "prod")
	in.Database = db
	_, err := g.Apply(ctx, in)
	if !errors.Is(err, context.Canceled) || db.tx != nil {
		t.Fatalf("error=%v events=%v", err, log.values)
	}
}

func TestSuppressedDiagnosticDoesNotBlockButStillRaisesRisk(t *testing.T) {
	log := &eventLog{}
	g, db, _ := harness(t, log)
	d := diagnostic(safety.SeverityError)
	g.Safety = safety.Runner{Analyzers: []safety.Analyzer{safety.AnalyzerFunc{ID: "suppressed", Fn: func(context.Context, safety.Input) ([]safety.Diagnostic, error) {
		log.add("analyze")
		return []safety.Diagnostic{d}, nil
	}}}, Suppressions: []safety.Suppression{{Rule: d.Rule, ObjectID: d.Object.ID, Reason: "approved test"}}}
	in, _ := boundInput(t, g, "prod")
	in.Database = db
	result, err := g.Apply(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if result.Risk != approval.RiskHigh || result.Diagnostics[0].Suppressed == nil {
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
	in, digest := boundInput(t, g, "prod")
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
	in, _ := boundInput(t, g, "prod")
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
	rebind(t, g, &in)
	db.queryErr = errors.New("backend password=" + secret)
	in.Database = db
	result, err := g.Apply(context.Background(), in)
	combined := formatErrorChain(err) + fmt.Sprintf(" %+v", result)
	if strings.Contains(combined, secret) || strings.Contains(combined, "SELECT") {
		t.Fatalf("sensitive evidence leaked: %s", combined)
	}
}

func formatErrorChain(err error) string {
	var out strings.Builder
	for current := err; current != nil; current = errors.Unwrap(current) {
		fmt.Fprintf(&out, "%v|%+v|%#v\n", current, current, current)
	}
	return out.String()
}

func diagnostic(severity safety.Severity) safety.Diagnostic {
	changes := testChanges()
	r := changes.Changes[0].After
	return safety.Diagnostic{Rule: "TEST001", Severity: severity, Message: "risk", Object: safety.Object{ID: r.ID, Kind: r.Kind, Name: r.Name.String()}, Impact: "impact", Remediation: "fix", Confidence: safety.ConfidenceHigh}
}
func rebind(t *testing.T, g Guardrail, in *Input) {
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
	in.StatementBindings, err = BuildStatementBindings(in.Changes, in.Safety.Statements)
	if err != nil {
		t.Fatal(err)
	}
	in.Approval.Plan.Digest, err = g.BundleDigest(*in)
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
}
