package safety

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"autosql/pkg/schema"
)

type namedSecret string
type reportSecret struct {
	Credential namedSecret `json:"credential"`
	Nested     any         `json:"nested"`
}

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
	table := resource(schema.KindTable, "accounts", `{}`)
	column := resource(schema.KindColumn, "nickname", `{"type":"text","nullable":true}`)
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{change("table", schema.OperationCreate, nil, table), change("column", schema.OperationCreate, nil, column)}}
	got, err := (Runner{Analyzers: Builtins()}).Run(context.Background(), Input{Changes: cs, Target: Target{Version: 16}})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("got diagnostics: %#v", got)
	}
}

func TestNewColumnAndConstraintRisks(t *testing.T) {
	column := resource(schema.KindColumn, "status", `{"type":"text","nullable":false,"default":"pending"}`)
	constraint := resource(schema.KindCheckConstraint, "status_valid", `{"definition":"CHECK (status != '')"}`)
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{change("column", schema.OperationCreate, nil, column), change("constraint", schema.OperationCreate, nil, constraint)}}
	ds, err := (Runner{Analyzers: []Analyzer{CompatibilityAnalyzer{}}}).Run(context.Background(), Input{Changes: cs})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rules(ds), "AUTOSQL004,AUTOSQL005,AUTOSQL006"; got != want {
		t.Fatalf("rules=%s want=%s", got, want)
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

func TestCompatibilityRiskMatrix(t *testing.T) {
	tests := []struct {
		name, before, after string
		want                string
	}{
		{"text to bounded varchar", `{"type":"text"}`, `{"type":"varchar(32)"}`, RuleNarrowType},
		{"numeric precision", `{"type":"numeric(12,2)"}`, `{"type":"numeric(8,2)"}`, RuleNarrowType},
		{"numeric scale", `{"type":"numeric(12,4)"}`, `{"type":"numeric(12,2)"}`, RuleNarrowType},
		{"numeric integer digits", `{"type":"numeric(12,2)"}`, `{"type":"numeric(12,4)"}`, RuleNarrowType},
		{"not null", `{"type":"text","nullable":true}`, `{"type":"text","nullable":false}`, RuleNotNull},
		{"canonical not_null", `{"type":"text","not_null":false}`, `{"type":"text","not_null":true}`, RuleNotNull},
		{"default", `{"type":"text","default":"old"}`, `{"type":"text","default":"new"}`, RuleDefaultChange},
		{"constraint", `{"definition":"CHECK (n > 0)"}`, `{"definition":"CHECK (n > 10)"}`, RuleConstraintChange},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before, after := resource(schema.KindColumn, "value", tc.before), resource(schema.KindColumn, "value", tc.after)
			ds, err := (Runner{Analyzers: []Analyzer{CompatibilityAnalyzer{}}}).Run(context.Background(), Input{Changes: schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{change("alter", schema.OperationAlter, before, after)}}})
			if err != nil {
				t.Fatal(err)
			}
			found := false
			for _, d := range ds {
				found = found || d.Rule == tc.want
			}
			if !found {
				t.Fatalf("missing %s in %#v", tc.want, ds)
			}
		})
	}
}

func TestTruncateAndSQLLexing(t *testing.T) {
	r := resource(schema.KindTable, "accounts", `{}`)
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{change("table", schema.OperationCreate, nil, r)}}
	for _, tc := range []struct {
		sql  string
		want bool
	}{
		{"TRUNCATE TABLE accounts", true},
		{"SELECT 1; TRUNCATE TABLE accounts", true},
		{"SELECT 'TRUNCATE TABLE accounts'", false},
		{"-- TRUNCATE accounts\nSELECT 1", false},
		{"SELECT $$ TRUNCATE accounts $$", false},
		{"SELECT \"truncate\" FROM accounts", false},
		{"SELECT truncate() FROM accounts", false},
		{"SELECT truncate FROM keywords", false},
	} {
		ds, err := (Runner{Analyzers: []Analyzer{CompatibilityAnalyzer{}}}).Run(context.Background(), Input{Changes: cs, Statements: []Statement{{ChangeID: "table", SQL: tc.sql}}})
		if err != nil {
			t.Fatal(err)
		}
		if got := len(ds) > 0; got != tc.want {
			t.Fatalf("SQL %q finding=%v want=%v (%#v)", tc.sql, got, tc.want, ds)
		}
	}
}

func TestDefaultVolatilityAndUnknownVersion(t *testing.T) {
	tests := []struct {
		name, value string
		version     int
		rewrite     bool
	}{
		{"literal fast default", `"ready"`, 16, false},
		{"statement timestamp stable", `"statement_timestamp()"`, 11, false},
		{"transaction timestamp stable", `"transaction_timestamp()"`, 11, false},
		{"current timestamp stable", `"CURRENT_TIMESTAMP"`, 11, false},
		{"current date stable", `"CURRENT_DATE"`, 16, false},
		{"stable fast default", `"now()"`, 16, false},
		{"volatile default", `"random()"`, 16, true},
		{"clock timestamp volatile", `"clock_timestamp()"`, 16, true},
		{"sequence volatile", `"nextval('seq')"`, 16, true},
		{"unknown expression", `"custom_default()"`, 16, true},
		{"unknown version fails conservatively", `"ready"`, 0, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := resource(schema.KindColumn, "state", `{"type":"text"}`)
			after := resource(schema.KindColumn, "state", `{"type":"text","default":`+tc.value+`}`)
			cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{change("default", schema.OperationAlter, before, after)}}
			ds, err := (Runner{Analyzers: []Analyzer{PostgreSQLAnalyzer{}}}).Run(context.Background(), Input{Changes: cs, Target: Target{Version: tc.version}})
			if err != nil {
				t.Fatal(err)
			}
			got := false
			for _, d := range ds {
				if d.Rule == RuleTableRewrite {
					got = true
					if tc.version == 0 && d.Confidence != ConfidenceLow {
						t.Fatalf("confidence=%s", d.Confidence)
					}
				}
			}
			if got != tc.rewrite {
				t.Fatalf("rewrite=%v want=%v (%#v)", got, tc.rewrite, ds)
			}
		})
	}
}

func TestEnumVersionSemantics(t *testing.T) {
	r := resource(schema.KindEnum, "mood", `{}`)
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{change("enum", schema.OperationCreate, nil, r)}}
	for _, tc := range []struct {
		version  int
		severity Severity
		contains string
	}{{11, SeverityError, "cannot safely"}, {12, SeverityWarning, "unavailable until commit"}, {16, SeverityWarning, "unavailable until commit"}, {0, SeverityError, "cannot safely"}} {
		ds, err := (Runner{Analyzers: []Analyzer{PostgreSQLAnalyzer{}}}).Run(context.Background(), Input{Changes: cs, Statements: []Statement{{ChangeID: "enum", SQL: "ALTER TYPE mood ADD VALUE 'happy'"}}, Target: Target{Version: tc.version}})
		if err != nil {
			t.Fatal(err)
		}
		if len(ds) != 1 || ds[0].Severity != tc.severity || !strings.Contains(ds[0].Message, tc.contains) {
			t.Fatalf("version %d: %#v", tc.version, ds)
		}
	}
}

func TestUnknownStatisticsFailClosedAndRewriteCapScope(t *testing.T) {
	r := resource(schema.KindIndex, "idx", `{}`)
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{change("idx", schema.OperationCreate, nil, r)}}
	ds, err := (Runner{Analyzers: []Analyzer{PostgreSQLAnalyzer{}}}).Run(context.Background(), Input{Changes: cs, Statements: []Statement{{ChangeID: "idx", SQL: "CREATE INDEX idx ON t (id)"}}, Target: Target{Version: 16}, Thresholds: Thresholds{MaxRowsScanned: 100, MaxRewriteBytes: 1}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Severity != SeverityError {
		t.Fatalf("got %#v", ds)
	}
	unproven, ok := ds[0].Properties["threshold_unproven"].([]string)
	if !ok || !reflect.DeepEqual(unproven, []string{"estimated_rows"}) {
		t.Fatalf("rewrite cap incorrectly applied or row cap not unproven: %#v", ds[0].Properties)
	}
}

func TestRewriteAndLockThresholds(t *testing.T) {
	before := resource(schema.KindColumn, "amount", `{"type":"bigint"}`)
	after := resource(schema.KindColumn, "amount", `{"type":"integer"}`)
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{change("alter", schema.OperationAlter, before, after)}}
	ds, err := (Runner{Analyzers: []Analyzer{PostgreSQLAnalyzer{}}}).Run(context.Background(), Input{Changes: cs, Target: Target{Version: 16, Statistics: map[string]TableStatistics{after.ID: {EstimatedRows: 10, TotalBytes: 2048}}}, Thresholds: Thresholds{MaxLockLevel: LockShareUpdateExclusive, MaxRewriteBytes: 1024}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 1 || ds[0].Severity != SeverityError {
		t.Fatalf("got %#v", ds)
	}
	got, ok := ds[0].Properties["threshold_exceeded"].([]string)
	if !ok || !reflect.DeepEqual(got, []string{"lock_level", "rewrite_bytes"}) {
		t.Fatalf("thresholds=%#v", ds[0].Properties)
	}
}

func TestOperationalSQLIgnoresCommentsAndLiterals(t *testing.T) {
	r := resource(schema.KindIndex, "idx", `{}`)
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{change("idx", schema.OperationCreate, nil, r)}}
	for _, sql := range []string{"SELECT 'CREATE INDEX idx ON t(id)'", "/* CREATE INDEX idx ON t(id) */ SELECT 1", "SELECT $$ DROP INDEX CONCURRENTLY idx $$", "SELECT create, index FROM keyword_columns", "SELECT create_index()", "CREATE TABLE index (id int)", "SELECT create FROM a; SELECT index FROM b", "SELECT alter, type FROM a; SELECT add, value FROM b"} {
		ds, err := (Runner{Analyzers: []Analyzer{PostgreSQLAnalyzer{}}}).Run(context.Background(), Input{Changes: cs, Statements: []Statement{{ChangeID: "idx", SQL: sql}}, Target: Target{Version: 16}})
		if err != nil {
			t.Fatal(err)
		}
		if len(ds) != 0 {
			t.Fatalf("SQL %q: %#v", sql, ds)
		}
	}
}

func TestOperationalCommandsAreMatchedPerStatement(t *testing.T) {
	r := resource(schema.KindIndex, "idx", `{}`)
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{change("idx", schema.OperationCreate, nil, r)}}
	ds, err := (Runner{Analyzers: []Analyzer{PostgreSQLAnalyzer{}}}).Run(context.Background(), Input{Changes: cs, Statements: []Statement{{ChangeID: "idx", SQL: "SELECT 1; CREATE UNIQUE INDEX idx ON t(id); DROP INDEX CONCURRENTLY old_idx"}}, Target: Target{Version: 16}})
	if err != nil {
		t.Fatal(err)
	}
	if got, want := rules(ds), "AUTOSQL103,AUTOSQL105"; got != want {
		t.Fatalf("rules=%s want=%s", got, want)
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

func TestConcurrentIndexNonTransactionalPlanIsAccepted(t *testing.T) {
	r := resource(schema.KindIndex, "idx_accounts", `{}`)
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{change("idx", schema.OperationCreate, nil, r)}}
	transactional := false
	ds, err := (Runner{Analyzers: Builtins()}).Run(context.Background(), Input{Changes: cs, Statements: []Statement{{ChangeID: "idx", SQL: "CREATE INDEX CONCURRENTLY idx_accounts ON accounts(id)", Transactional: &transactional}}, Target: Target{Version: 16}})
	if err != nil {
		t.Fatal(err)
	}
	if got := rules(ds); got != "" {
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
	r := resource(schema.KindTable, "x", `{"nested":{"value":"original"}}`)
	r.Annotations = map[string]string{"owner": "original"}
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{change("x", schema.OperationCreate, nil, r)}}
	source := &schema.SourceLocation{URI: "original.sql", Extra: map[string]json.RawMessage{"nested": json.RawMessage(`{"value":"original"}`)}}
	in := Input{Changes: cs, Statements: []Statement{{ChangeID: "x", SQL: "SELECT 1", Source: source}}, Target: Target{Statistics: map[string]TableStatistics{r.ID: {EstimatedRows: 7}}}}
	mutator := AnalyzerFunc{ID: "a-mutator", Fn: func(_ context.Context, got Input) ([]Diagnostic, error) {
		got.Changes.Changes[0].After.Annotations["owner"] = "mutated"
		got.Changes.Changes[0].After.Spec[0] = '['
		got.Statements[0].SQL = "mutated"
		got.Statements[0].Source.URI = "mutated.sql"
		got.Statements[0].Source.Extra["nested"][0] = '['
		got.Target.Statistics[r.ID] = TableStatistics{EstimatedRows: 999}
		return []Diagnostic{{Rule: "A", Severity: SeverityInfo, Message: "a", Object: Object{ID: r.ID, Kind: r.Kind, Name: "x"}, Impact: "none", Remediation: "none", Confidence: ConfidenceHigh}}, nil
	}}
	observer := AnalyzerFunc{ID: "z-observer", Fn: func(_ context.Context, got Input) ([]Diagnostic, error) {
		if got.Changes.Changes[0].After.Annotations["owner"] != "original" || !json.Valid(got.Changes.Changes[0].After.Spec) || got.Statements[0].SQL != "SELECT 1" || got.Statements[0].Source.URI != "original.sql" || !json.Valid(got.Statements[0].Source.Extra["nested"]) || got.Target.Statistics[r.ID].EstimatedRows != 7 {
			return nil, errors.New("analyzer input was contaminated")
		}
		return []Diagnostic{{Rule: "Z", Severity: SeverityInfo, Message: "z", Object: Object{ID: r.ID, Kind: r.Kind, Name: "x"}, Impact: "none", Remediation: "none", Confidence: ConfidenceHigh}}, nil
	}}
	got, err := (Runner{Analyzers: []Analyzer{observer, mutator}}).Run(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if rules(got) != "A,Z" {
		t.Fatalf("got %s", rules(got))
	}
	if r.Annotations["owner"] != "original" || !json.Valid(r.Spec) || source.URI != "original.sql" || in.Target.Statistics[r.ID].EstimatedRows != 7 {
		t.Fatal("runner mutated caller input")
	}
	again, err := (Runner{Analyzers: []Analyzer{mutator, observer}}).Run(context.Background(), in)
	if err != nil || !reflect.DeepEqual(got, again) {
		t.Fatalf("order-dependent result: %#v, %v", again, err)
	}
}

func TestReportsRedactAndParse(t *testing.T) {
	d := Diagnostic{Rule: "X", Severity: SeverityError, Message: `password="two words" https://webuser:webpass@example.test/path`, Object: Object{ID: "id", Kind: schema.KindTable, Name: "users"}, Source: &schema.SourceLocation{URI: "mysql://dbuser:dbpass@host/db", Extra: map[string]json.RawMessage{"password": json.RawMessage(`"source secret"`)}}, Impact: "token='quoted token'", Remediation: "rotate", Confidence: ConfidenceHigh, Properties: map[string]any{
		"password": "top secret", "nested": []any{map[string]any{"api_key": "key value", "note": "pwd=inside"}, json.RawMessage(`{"token":"raw token","list":["secret=deep value"]}`)},
		"struct": any(&reportSecret{Credential: namedSecret("struct secret"), Nested: &reportSecret{Credential: namedSecret("pointer secret")}}),
		"named":  namedSecret("token=named secret"),
	}}
	forbidden := []string{"two words", "webuser:webpass", "dbuser:dbpass", "quoted token", "top secret", "key value", "inside", "raw token", "deep value", "source secret", "struct secret", "pointer secret", "named secret"}
	for _, f := range []string{"human", "json", "sarif"} {
		out, err := ReportString(f, []Diagnostic{d})
		if err != nil {
			t.Fatal(err)
		}
		for _, secret := range forbidden {
			if strings.Contains(out, secret) {
				t.Fatalf("%s leaked %q: %s", f, secret, out)
			}
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

func TestCompatibilityReportGolden(t *testing.T) {
	before := resource(schema.KindColumn, "value", `{"type":"text","nullable":true,"default":"old","definition":"CHECK (value != '')"}`)
	after := resource(schema.KindColumn, "value", `{"type":"varchar(8)","nullable":false,"default":"new","definition":"CHECK (length(value) > 2)"}`)
	oldName, newName := resource(schema.KindColumn, "legacy", `{}`), resource(schema.KindColumn, "current", `{}`)
	table := resource(schema.KindTable, "events", `{}`)
	cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{
		change("alter", schema.OperationAlter, before, after),
		change("rename", schema.OperationRename, oldName, newName),
		change("table", schema.OperationCreate, nil, table),
	}}
	ds, err := (Runner{Analyzers: []Analyzer{CompatibilityAnalyzer{}}}).Run(context.Background(), Input{Changes: cs, Statements: []Statement{{ChangeID: "table", SQL: "TRUNCATE TABLE events"}}})
	if err != nil {
		t.Fatal(err)
	}
	got, err := ReportString("human", ds)
	if err != nil {
		t.Fatal(err)
	}
	want, err := os.ReadFile("testdata/compatibility.golden")
	if err != nil {
		t.Fatal(err)
	}
	if got != string(want) {
		t.Fatalf("report mismatch\n--- got ---\n%s--- want ---\n%s", got, want)
	}
}
