package policy

import (
	"context"
	"errors"
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
