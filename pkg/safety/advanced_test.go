package safety

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"autosql/pkg/schema"
)

func advancedChange(kind schema.Kind, name string) schema.Change {
	r := &schema.Resource{Kind: kind, Name: schema.Name{Schema: "public", Name: name}, Spec: json.RawMessage(`{}`)}
	r.ID = schema.StableID(kind, r.Name)
	return schema.Change{ID: "change-" + name, Operation: schema.OperationCreate, ResourceID: r.ID, After: r}
}

func TestAdvancedAnalyzerFlagsUnsafeSQLAndProvenance(t *testing.T) {
	ch := advancedChange(schema.KindTable, "Users")
	ds, err := (Runner{Analyzers: []Analyzer{AdvancedAnalyzer{}}}).Run(context.Background(), Input{Changes: schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{ch}}, Statements: []Statement{{ChangeID: ch.ID, SQL: "CREATE TABLE copied AS SELECT * FROM users"}}, Provenance: Provenance{Agent: "migration-agent", AgentVersion: "1", Untrusted: true}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range ds {
		got[d.Rule] = true
		if d.Impact == "" || d.Remediation == "" {
			t.Fatalf("incomplete diagnostic: %#v", d)
		}
	}
	for _, rule := range []string{RuleNaming, RuleTableCopy, RuleGeneratedTest, RuleAgentProvenance} {
		if !got[rule] {
			t.Fatalf("missing %s in %#v", rule, ds)
		}
	}
	if strings.Contains(string(mustJSON(ds)), "password") {
		t.Fatal("unexpected sensitive output")
	}
}

func TestAdvancedAnalyzerDetectsDynamicSQLAndMergeConflict(t *testing.T) {
	a := advancedChange(schema.KindTable, "users")
	b := a
	b.ID = "second"
	b.Operation = schema.OperationAlter
	b.Before = a.After
	b.After = a.After
	ds, err := (Runner{Analyzers: []Analyzer{AdvancedAnalyzer{}}}).Run(context.Background(), Input{Changes: schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{a, b}}, Statements: []Statement{{ChangeID: a.ID, SQL: "EXECUTE 'ALTER TABLE ' || table_name"}}})
	if err != nil {
		t.Fatal(err)
	}
	got := map[string]bool{}
	for _, d := range ds {
		got[d.Rule] = true
	}
	if !got[RuleDynamicSQL] || !got[RuleMergeConflict] {
		t.Fatalf("rules=%v", got)
	}
}

func TestAllBuiltinsIncludesAdvancedAttestation(t *testing.T) {
	found := false
	for _, a := range AllBuiltins() {
		if a.Name() == "advanced-lint" {
			found = true
			if _, ok := a.(AttestedAnalyzer); !ok {
				t.Fatal("advanced analyzer must be attested")
			}
		}
	}
	if !found {
		t.Fatal("advanced analyzer missing")
	}
}

func mustJSON(v any) []byte { b, _ := json.Marshal(v); return b }
