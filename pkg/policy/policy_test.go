package policy

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestPolicyTargetsPredicatesVariablesAndConventions(t *testing.T) {
	raw := []byte(`{
 "version":"v1", "variables":{"team":"data"},
 "predicates":{"owned":{"eq":["resource.owner","variables.team"]}},
 "rules":[
  {"name":"ownership","target":"schema","kinds":["table"],"assert":{"predicate":"owned"},"message":"table must be owned by data"},
  {"name":"naming","target":"all","assert":{"matches":["resource.name","^[a-z][a-z0-9_]*$"]},"message":"invalid name"},
  {"name":"secure","target":"migration","assert":{"eq":["resource.attributes.encrypted",true]},"message":"operation must preserve encryption"}
 ]}`)
	doc, err := Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	v, err := (Evaluator{}).Evaluate(context.Background(), *doc,
		[]Resource{{Kind: "table", Name: "Users", Owner: "app"}},
		[]Resource{{Kind: "add_column", Name: "secret", Attributes: map[string]any{"encrypted": false}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 3 {
		t.Fatalf("violations=%+v", v)
	}
}

func TestInvalidRuleHasSource(t *testing.T) {
	_, err := Parse([]byte("{\n \"version\":\"v1\",\n \"rules\":[{\"name\":\"x\",\"target\":\"wat\",\"message\":\"x\",\"assert\":{\"eq\":[1,1]}}]}"))
	var se *SourceError
	if !errors.As(err, &se) {
		t.Fatalf("got %T %v", err, err)
	}
	if se.Line < 2 || se.Column < 1 {
		t.Fatalf("location=%d:%d", se.Line, se.Column)
	}
}

func TestPackVersionAndDeterministicLimits(t *testing.T) {
	_, err := ParsePack([]byte(`{"name":"org","version":"2026.1","policies":{"version":"v1","rules":[]}}`))
	if err != nil {
		t.Fatal(err)
	}
	d := Document{Version: "v1", Rules: []Rule{{Name: "x", Target: "all", Message: "x", Assert: Expression{Eq: []any{1, 1}}}}}
	_, err = (Evaluator{Limits: Limits{MaxResources: 1}}).Evaluate(context.Background(), d, []Resource{{}, {}}, nil)
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("got %v", err)
	}
	clock := time.Unix(0, 0)
	calls := 0
	_, err = (Evaluator{Limits: Limits{Timeout: time.Second}, Now: func() time.Time {
		calls++
		if calls > 1 {
			return clock.Add(2 * time.Second)
		}
		return clock
	}}).Evaluate(context.Background(), d, []Resource{{}}, nil)
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestPredicateCycleRejected(t *testing.T) {
	_, err := Parse([]byte(`{"version":"v1","predicates":{"a":{"predicate":"b"},"b":{"predicate":"a"}},"rules":[]}`))
	if err == nil {
		t.Fatal("expected cycle error")
	}
}

func TestInvalidRegexRejectedAtSource(t *testing.T) {
	_, err := Parse([]byte("{\n\"version\":\"v1\",\"rules\":[{\"name\":\"x\",\"target\":\"all\",\"message\":\"x\",\"assert\":{\"matches\":[\"resource.name\",\"[\"]}}]}"))
	var se *SourceError
	if !errors.As(err, &se) || se.Line < 2 {
		t.Fatalf("got %T %v", err, err)
	}
}

func TestUnknownFieldAndWrongTypeHaveSource(t *testing.T) {
	for _, raw := range []string{
		"{\n\"version\":\"v1\",\"surprise\":true,\"rules\":[]}",
		"{\n\"version\":\"v1\",\"rules\":\"wrong\"}",
	} {
		_, err := Parse([]byte(raw))
		var se *SourceError
		if !errors.As(err, &se) || se.Line < 2 {
			t.Fatalf("got %T %v", err, err)
		}
	}
}

func TestSourceLocationIdentifiesExactIndexedRuleField(t *testing.T) {
	raw := strings.Join([]string{
		"{",
		`  "version":"v1",`,
		`  "rules":[`,
		`    {"name":"first","target":"schema","message":"ok","assert":{"eq":[1,1]}},`,
		`    {"name":"second","target":"invalid","message":"bad","assert":{"eq":[1,1]}}`,
		`  ]`,
		`}`,
	}, "\n")
	_, err := Parse([]byte(raw))
	var se *SourceError
	if !errors.As(err, &se) {
		t.Fatalf("got %T %v", err, err)
	}
	wantLine := 5
	wantColumn := strings.Index(strings.Split(raw, "\n")[wantLine-1], `"target"`) + 1
	if se.Path != "$.rules[1].target" || se.Line != wantLine || se.Column != wantColumn {
		t.Fatalf("location=%s %d:%d want %d:%d", se.Path, se.Line, se.Column, wantLine, wantColumn)
	}
}

func TestDecodeErrorIdentifiesExactIndexedRuleField(t *testing.T) {
	raw := strings.Join([]string{
		"{",
		`  "version":"v1",`,
		`  "rules":[`,
		`    {"name":"first","target":"schema","message":"ok","assert":{"eq":[1,1]}},`,
		`    {"name":"second","target":42,"message":"bad","assert":{"eq":[1,1]}}`,
		`  ]`,
		`}`,
	}, "\n")
	_, err := Parse([]byte(raw))
	var se *SourceError
	if !errors.As(err, &se) {
		t.Fatalf("got %T %v", err, err)
	}
	wantColumn := strings.Index(strings.Split(raw, "\n")[4], `"target"`) + 1
	if se.Path != "$.rules[1].target" || se.Line != 5 || se.Column != wantColumn {
		t.Fatalf("location=%s %d:%d want $.rules[1].target 5:%d", se.Path, se.Line, se.Column, wantColumn)
	}
}

func TestTypeSafeComparisonsAndMissingReferences(t *testing.T) {
	doc := Document{Version: "v1", Rules: []Rule{
		{Name: "missing-eq", Target: "all", Message: "missing", Assert: Expression{Eq: []any{"resource.attributes.absent", nil}}},
		{Name: "missing-ne", Target: "all", Message: "missing", Assert: Expression{Ne: []any{"resource.attributes.absent", "value"}}},
		{Name: "number-string", Target: "all", Message: "typed", Assert: Expression{Eq: []any{"resource.attributes.count", "1"}}},
		{Name: "number-number", Target: "all", Message: "numeric", Assert: Expression{Eq: []any{"resource.attributes.count", float64(1)}}},
	}}
	violations, err := (Evaluator{}).Evaluate(context.Background(), doc, []Resource{{Kind: "table", Name: "t", Attributes: map[string]any{"count": 1}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(violations) != 3 {
		t.Fatalf("violations=%+v", violations)
	}
}

func TestNumericComparisonDoesNotLoseIntegerPrecision(t *testing.T) {
	const a = uint64(1<<63 + 1)
	const b = uint64(1<<63 + 2)
	doc := Document{Version: "v1", Rules: []Rule{{Name: "exact", Target: "all", Message: "numbers differ", Assert: Expression{Eq: []any{"resource.attributes.value", b}}}}}
	v, err := (Evaluator{}).Evaluate(context.Background(), doc, []Resource{{Kind: "table", Name: "t", Attributes: map[string]any{"value": a}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 1 {
		t.Fatalf("large integers compared equal: %+v", v)
	}
}

func TestJSONPolicyNumbersPreserveAdjacentLargeIntegers(t *testing.T) {
	doc, err := Parse([]byte(`{"version":"v1","rules":[{"name":"exact","target":"all","message":"different","assert":{"eq":[9007199254740992,9007199254740993]}}]}`))
	if err != nil {
		t.Fatal(err)
	}
	for i, literal := range doc.Rules[0].Assert.Eq {
		if _, ok := literal.(json.Number); !ok {
			t.Fatalf("literal %d decoded as %T", i, literal)
		}
	}
	v, err := (Evaluator{}).Evaluate(context.Background(), *doc, []Resource{{Kind: "table", Name: "t"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(v) != 1 {
		t.Fatalf("adjacent large integers compared equal: %+v", v)
	}
}

func TestValidationAndRegexAreBounded(t *testing.T) {
	doc := Document{Version: "v1", Predicates: map[string]Expression{"p": {All: []Expression{{Eq: []any{1, 1}}, {Eq: []any{2, 2}}, {Eq: []any{3, 3}}}}}, Rules: []Rule{{Name: "r", Target: "schema", Kinds: []string{"table", "view"}, Message: "x", Assert: Expression{Predicate: "p"}}}}
	if _, err := (Evaluator{Limits: Limits{MaxSteps: 5}}).Evaluate(context.Background(), doc, nil, nil); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("validation not metered: %v", err)
	}
	clock := time.Unix(0, 0)
	calls := 0
	_, err := (Evaluator{Limits: Limits{MaxSteps: 1000, Timeout: time.Second}, Now: func() time.Time {
		calls++
		if calls > 3 {
			return clock.Add(2 * time.Second)
		}
		return clock
	}}).Evaluate(context.Background(), doc, nil, nil)
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("validation timeout not enforced: %v", err)
	}
	regexDoc := Document{Version: "v1", Rules: []Rule{{Name: "r", Target: "all", Message: "x", Assert: Expression{Matches: []any{"resource.name", "abcdef"}}}}}
	if _, err = (Evaluator{Limits: Limits{MaxPatternBytes: 4}}).Evaluate(context.Background(), regexDoc, []Resource{{Kind: "table", Name: "x"}}, nil); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("pattern bound: %v", err)
	}
	regexDoc.Rules[0].Assert.Matches = []any{"resource.name", ".*"}
	if _, err = (Evaluator{Limits: Limits{MaxTextBytes: 4}}).Evaluate(context.Background(), regexDoc, []Resource{{Kind: "table", Name: "too-long"}}, nil); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("text bound: %v", err)
	}
}

func TestLimitsCoverSkippedRulesKindsPredicatesAndRegex(t *testing.T) {
	predicate := Expression{Matches: []any{"resource.name", "^table_[0-9]+$"}}
	doc := Document{Version: "v1", Predicates: map[string]Expression{"named": predicate}, Rules: []Rule{
		{Name: "wrong-target", Target: "migration", Message: "x", Assert: Expression{Eq: []any{1, 1}}},
		{Name: "wrong-kind", Target: "schema", Kinds: []string{"view", "index", "column"}, Message: "x", Assert: Expression{Eq: []any{1, 1}}},
		{Name: "regex", Target: "schema", Kinds: []string{"table"}, Message: "x", Assert: Expression{Predicate: "named"}},
	}}
	resource := []Resource{{Kind: "table", Name: "table_1"}}
	if _, err := (Evaluator{Limits: Limits{MaxSteps: 12}}).Evaluate(context.Background(), doc, resource, nil); !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("skipped scans were not charged: %v", err)
	}

	clock := time.Unix(0, 0)
	calls := 0
	_, err := (Evaluator{Limits: Limits{MaxSteps: 1000, Timeout: time.Second}, Now: func() time.Time {
		calls++
		if calls > 14 {
			return clock.Add(2 * time.Second)
		}
		return clock
	}}).Evaluate(context.Background(), doc, resource, nil)
	if !errors.Is(err, ErrLimitExceeded) {
		t.Fatalf("regex/predicate timeout was not observed: %v (clock calls %d)", err, calls)
	}

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := (Evaluator{}).Evaluate(ctx, doc, resource, nil); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not observed: %v", err)
	}
}
