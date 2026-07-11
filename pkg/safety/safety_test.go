package safety

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"autosql/pkg/schema"
)

func resource(kind schema.Kind, name, specJSON string) *schema.Resource {
	r := &schema.Resource{Kind: kind, Name: schema.Name{Schema: "public", Name: name}, Spec: json.RawMessage(specJSON)}
	r.ID = schema.StableID(kind, r.Name)
	return r
}
func change(id string, op schema.Operation, b, a *schema.Resource) schema.Change {
	r := a
	if r == nil {
		r = b
	}
	return schema.Change{ID: id, Operation: op, ResourceID: r.ID, Before: b, After: a}
}
func run(t *testing.T, cs ...schema.Change) []Diagnostic {
	t.Helper()
	ds, err := (Runner{Analyzers: Builtins()}).Run(context.Background(), Input{Changes: schema.ChangeSet{Version: schema.ChangeVersion, Changes: cs}, Target: Target{Engine: "postgresql", Version: 16}})
	if err != nil {
		t.Fatal(err)
	}
	return ds
}
func rules(ds []Diagnostic) string {
	a := make([]string, len(ds))
	for i, d := range ds {
		a[i] = d.Rule
	}
	return strings.Join(a, ",")
}

func TestSafeAdditiveChangePasses(t *testing.T) {
	r := resource(schema.KindTable, "accounts", `{}`)
	if got := run(t, change("c1", schema.OperationCreate, nil, r)); len(got) != 0 {
		t.Fatalf("got diagnostics: %#v", got)
	}
}
func TestDestructiveAndCompatibilityRules(t *testing.T) {
	drop := resource(schema.KindTable, "customers", `{}`)
	b := resource(schema.KindColumn, "code", `{"type":"varchar(100)","nullable":true,"default":"old"}`)
	a := resource(schema.KindColumn, "code", `{"type":"varchar(10)","nullable":false,"default":"new"}`)
	got := run(t, change("drop", schema.OperationDrop, drop, nil), change("alter", schema.OperationAlter, b, a))
	want := "AUTOSQL001,AUTOSQL003,AUTOSQL004,AUTOSQL005,AUTOSQL101,AUTOSQL102,AUTOSQL104"
	if s := rules(got); s != want {
		t.Fatalf("rules=%s want=%s", s, want)
	}
	if got[0].Impact == "" || got[0].Remediation == "" || got[0].Object.Name == "" {
		t.Fatalf("incomplete diagnostic: %#v", got[0])
	}
}

func TestRenameIsValidAndReported(t *testing.T) {
	before := resource(schema.KindColumn, "old_name", `{}`)
	after := resource(schema.KindColumn, "new_name", `{}`)
	ds := run(t, change("rename", schema.OperationRename, before, after))
	if got := rules(ds); got != "AUTOSQL007,AUTOSQL101" {
		t.Fatalf("rules=%s", got)
	}
}
func TestStatementRulesAndThresholds(t *testing.T) {
	r := resource(schema.KindIndex, "idx_accounts_email", `{}`)
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{change("idx", schema.OperationCreate, nil, r)}}
	ds, err := (Runner{Analyzers: Builtins()}).Run(context.Background(), Input{Changes: cs, Statements: []Statement{{ChangeID: "idx", SQL: "CREATE INDEX idx_accounts_email ON accounts(email)"}}, Target: Target{Version: 16, Statistics: map[string]TableStatistics{r.ID: {EstimatedRows: 10000}}}, Thresholds: Thresholds{MaxRowsScanned: 100}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Rule != RuleIndexBuild || ds[0].Severity != SeverityError {
		t.Fatalf("got %#v", ds)
	}
}
func TestMissingStatisticsIsConservative(t *testing.T) {
	b := resource(schema.KindColumn, "age", `{"type":"bigint"}`)
	a := resource(schema.KindColumn, "age", `{"type":"integer"}`)
	ds := run(t, change("c", schema.OperationAlter, b, a))
	found := false
	for _, d := range ds {
		if d.Rule == RuleTableRewrite {
			found = true
			if d.Confidence != ConfidenceMedium || len(d.Assumptions) < 2 {
				t.Fatalf("missing assumptions: %#v", d)
			}
		}
	}
	if !found {
		t.Fatal("rewrite risk not found")
	}
}

func TestPostgreSQLVersionSpecificConstantDefault(t *testing.T) {
	before := resource(schema.KindColumn, "state", `{"type":"text"}`)
	after := resource(schema.KindColumn, "state", `{"type":"text","default":"new"}`)
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{change("default", schema.OperationAlter, before, after)}}
	for _, tc := range []struct {
		version     int
		wantRewrite bool
	}{{10, true}, {11, false}, {16, false}} {
		ds, err := (Runner{Analyzers: []Analyzer{PostgreSQLAnalyzer{}}}).Run(context.Background(), Input{Changes: cs, Target: Target{Version: tc.version}})
		if err != nil {
			t.Fatal(err)
		}
		got := false
		for _, d := range ds {
			got = got || d.Rule == RuleTableRewrite
		}
		if got != tc.wantRewrite {
			t.Fatalf("PostgreSQL %d rewrite=%v want=%v", tc.version, got, tc.wantRewrite)
		}
	}
}

func TestConcurrentIndexTransactionRestriction(t *testing.T) {
	r := resource(schema.KindIndex, "idx_accounts", `{}`)
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{change("idx", schema.OperationCreate, nil, r)}}
	ds, err := (Runner{Analyzers: Builtins()}).Run(context.Background(), Input{Changes: cs, Statements: []Statement{{ChangeID: "idx", SQL: "CREATE INDEX CONCURRENTLY idx_accounts ON accounts(id)"}}, Target: Target{Version: 16}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rules(ds); got != "AUTOSQL105" {
		t.Fatalf("rules=%s", got)
	}
}

func TestSuppressionScopeReasonAndExpiry(t *testing.T) {
	r := resource(schema.KindTable, "customers", `{}`)
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{change("drop", schema.OperationDrop, r, nil)}}
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	future := now.Add(time.Hour)
	runner := Runner{Analyzers: Builtins(), Suppressions: []Suppression{{Rule: RuleDropObject, ObjectID: r.ID, Reason: "approved archival", ExpiresAt: &future}}, Now: func() time.Time { return now }}
	ds, err := runner.Run(context.Background(), Input{Changes: cs, Target: Target{Version: 16}})
	if err != nil {
		t.Fatal(err)
	}
	if ds[0].Suppressed == nil {
		t.Fatal("expected suppression")
	}
	runner.Suppressions = []Suppression{{Rule: RuleDropObject, ObjectID: r.ID}}
	if _, err = runner.Run(context.Background(), Input{Changes: cs}); err == nil {
		t.Fatal("expected invalid suppression")
	}
}

func TestDeterministicIndependentAnalyzers(t *testing.T) {
	r := resource(schema.KindTable, "x", `{}`)
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{change("x", schema.OperationCreate, nil, r)}}
	a := AnalyzerFunc{ID: "z", Fn: func(context.Context, Input) ([]Diagnostic, error) {
		return []Diagnostic{{Rule: "Z", Severity: SeverityInfo, Message: "z", Object: Object{ID: r.ID, Kind: r.Kind, Name: "x"}, Impact: "none", Remediation: "none", Confidence: ConfidenceHigh}}, nil
	}}
	b := AnalyzerFunc{ID: "a", Fn: func(context.Context, Input) ([]Diagnostic, error) {
		return []Diagnostic{{Rule: "A", Severity: SeverityInfo, Message: "a", Object: Object{ID: r.ID, Kind: r.Kind, Name: "x"}, Impact: "none", Remediation: "none", Confidence: ConfidenceHigh}}, nil
	}}
	got, err := (Runner{Analyzers: []Analyzer{a, b}}).Run(context.Background(), Input{Changes: cs})
	if err != nil {
		t.Fatal(err)
	}
	if rules(got) != "A,Z" {
		t.Fatalf("got %s", rules(got))
	}
}

func TestReportsRedactAndParse(t *testing.T) {
	d := Diagnostic{Rule: "X", Severity: SeverityError, Message: "password=hunter2 postgres://user:pass@host/db", Object: Object{ID: "id", Kind: schema.KindTable, Name: "users"}, Impact: "token=abc", Remediation: "rotate", Confidence: ConfidenceHigh}
	for _, f := range []string{"human", "json", "sarif"} {
		out, err := ReportString(f, []Diagnostic{d})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(out, "hunter2") || strings.Contains(out, "user:pass") || strings.Contains(out, "token=abc") {
			t.Fatalf("%s leaked: %s", f, out)
		}
		if f != "human" {
			var v any
			if json.Unmarshal([]byte(out), &v) != nil {
				t.Fatalf("invalid %s", f)
			}
		}
	}
}

func TestHumanReportGolden(t *testing.T) {
	r := resource(schema.KindTable, "customers", `{}`)
	ds := run(t, change("drop", schema.OperationDrop, r, nil))
	got, err := ReportString("human", ds)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/destructive.golden")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("report mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}
