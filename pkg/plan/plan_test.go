package plan_test

import (
	"context"
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"testing"

	"autosql/pkg/guardrail"
	"autosql/pkg/plan"
	"autosql/pkg/plugin"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
)

func resource(kind schema.Kind, name schema.Name, spec string, deps ...schema.Dependency) schema.Resource {
	r := schema.Resource{Kind: kind, Name: name, Spec: json.RawMessage(spec), Dependencies: deps}
	r.ID = schema.StableID(kind, name)
	return r
}

func documents() (schema.Document, schema.Document) {
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	s := resource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	view := resource(schema.KindView, schema.Name{Schema: "app", Name: "users", Parent: s.ID}, `{"definition":"SELECT 1"}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{view, s}}}
	return empty, desired
}

func TestBuildDeterministicAndBound(t *testing.T) {
	current, desired := documents()
	var baseline []byte
	for seed := int64(0); seed < 50; seed++ {
		copy := desired
		copy.Graph.Resources = append([]schema.Resource(nil), desired.Graph.Resources...)
		rand.New(rand.NewSource(seed)).Shuffle(len(copy.Graph.Resources), func(i, j int) {
			copy.Graph.Resources[i], copy.Graph.Resources[j] = copy.Graph.Resources[j], copy.Graph.Resources[i]
		})
		p, err := plan.Build(context.Background(), postgres.New(), current, copy, plan.Options{})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := p.MarshalCanonical()
		if err != nil {
			t.Fatal(err)
		}
		if seed == 0 {
			baseline = raw
		} else if string(raw) != string(baseline) {
			t.Fatalf("seed %d changed plan", seed)
		}
		for _, step := range p.Steps {
			found := false
			for _, change := range p.Changes.Changes {
				found = found || step.ChangeID == change.ID
			}
			if !found {
				t.Fatalf("unbound step %+v", step)
			}
		}
	}
}

func TestUnsupportedReturnsZeroPlan(t *testing.T) {
	r := resource(schema.KindEnum, schema.Name{Schema: "public", Name: "status"}, `{"values":["new"]}`)
	current := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{r}}}
	r.Spec = json.RawMessage(`{"values":["new","done"]}`)
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{r}}}
	p, err := plan.Build(context.Background(), postgres.New(), current, desired, plan.Options{})
	if !errors.Is(err, plan.ErrUnsupportedTransition) || !reflect.DeepEqual(p, plan.Plan{}) {
		t.Fatalf("plan=%+v err=%v", p, err)
	}
}

func TestPlanValidationRejectsMutation(t *testing.T) {
	current, desired := documents()
	p, err := plan.Build(context.Background(), postgres.New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	p.Steps[0].SQL = "DROP SCHEMA app;"
	if !errors.Is(p.Validate(), plan.ErrInvalidPlan) {
		t.Fatal("mutated plan validated")
	}
}

func TestPlanIsInputIndependentAndGuardrailCompatible(t *testing.T) {
	current, desired := documents()
	p, err := plan.Build(context.Background(), postgres.New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	before, _ := p.MarshalCanonical()
	desired.Graph.Resources[0].Spec = json.RawMessage(`{"definition":"SELECT 2"}`)
	after, _ := p.MarshalCanonical()
	if string(before) != string(after) {
		t.Fatal("plan retained mutable input")
	}
	bindings, err := guardrail.BuildStatementBindings(p.Changes, p.SafetyStatements())
	if err != nil {
		t.Fatal(err)
	}
	if len(bindings) != len(p.Steps) {
		t.Fatalf("bindings=%d steps=%d", len(bindings), len(p.Steps))
	}
}

func TestNonManagedTransitionHasNoRendererOutput(t *testing.T) {
	r := resource(schema.KindRole, schema.Name{Name: "reader"}, `{}`)
	changes, err := schema.Diff(schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}, schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{r}}}, schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := postgres.New().Render(context.Background(), plugin.RenderRequest{Changes: changes})
	if err == nil || len(out) != 0 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestRendererDiscardsEarlierStatementsOnLaterFailure(t *testing.T) {
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	table := resource(schema.KindTable, schema.Name{Schema: "public", Name: "users"}, `{}`)
	trigger := resource(schema.KindTrigger, schema.Name{Schema: "public", Name: "users_trigger", Parent: table.ID}, `{}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{table, trigger}}}
	changes, err := schema.Diff(empty, desired, schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	out, err := postgres.New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: empty, Desired: desired})
	if err == nil || len(out) != 0 {
		t.Fatalf("partial output=%+v err=%v", out, err)
	}
}

func TestValidateRecomputesAllDerivedTopology(t *testing.T) {
	mutations := map[string]func(*plan.Plan){
		"planner version":   func(p *plan.Plan) { p.PlannerVersion = "9" },
		"step id":           func(p *plan.Plan) { p.Steps[0].ID = "step:wrong" },
		"step lock":         func(p *plan.Plan) { p.Steps[0].Lock = "mystery" },
		"phase id":          func(p *plan.Plan) { p.Phases[0].ID = "phase:wrong" },
		"phase coverage":    func(p *plan.Plan) { p.Phases[0].StepIDs = nil },
		"phase transaction": func(p *plan.Plan) { p.Phases[0].Transaction = "mystery" },
		"change coverage":   func(p *plan.Plan) { p.Steps = p.Steps[:len(p.Steps)-1] },
		"step order":        func(p *plan.Plan) { p.Steps[0], p.Steps[1] = p.Steps[1], p.Steps[0] },
	}
	for name, mutate := range mutations {
		t.Run(name, func(t *testing.T) {
			current, desired := documents()
			p, err := plan.Build(context.Background(), postgres.New(), current, desired, plan.Options{})
			if err != nil {
				t.Fatal(err)
			}
			mutate(&p)
			if !errors.Is(p.Validate(), plan.ErrInvalidPlan) {
				t.Fatal("mutated plan validated")
			}
		})
	}
}
