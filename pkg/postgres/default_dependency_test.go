package postgres

import (
	"context"
	"strings"
	"testing"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
)

func TestUserDefinedDefaultsRequireExactTypeDependencies(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "widgets", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	status := renderResource(schema.KindEnum, schema.Name{Schema: "app", Name: "job_status", Parent: ns.ID}, `{"values":["pending","done"]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	other := renderResource(schema.KindEnum, schema.Name{Schema: "app", Name: "other_status", Parent: ns.ID}, `{"values":["pending"]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	domain := renderResource(schema.KindDomain, schema.Name{Schema: "app", Name: "positive_int", Parent: ns.ID}, `{"base_type":"integer","not_null":false,"constraints":["CHECK (VALUE > 0)"]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})

	tests := []struct {
		name, typ, value string
		target           schema.Resource
		want             bool
	}{
		{"qualified enum", "app.job_status", "'pending'::app.job_status", status, true},
		{"canonical unqualified enum", "job_status", "'pending'::job_status", status, true},
		{"invalid enum label", "app.job_status", "'missing'::app.job_status", status, false},
		{"mismatched enum cast", "app.job_status", "'pending'::app.other_status", status, false},
		{"qualified domain cast", "app.positive_int", "(5)::app.positive_int", domain, true},
		{"domain base literal", "app.positive_int", "5", domain, true},
		{"domain invalid base literal", "app.positive_int", "'secret'", domain, false},
	}
	for _, fixture := range tests {
		t.Run(fixture.name, func(t *testing.T) {
			column := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "value", Parent: table.ID}, `{"type":"`+fixture.typ+`","default":"`+strings.ReplaceAll(fixture.value, `"`, `\"`)+`","not_null":false,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: fixture.target.ID, Type: schema.DependencyUses})
			resources := []schema.Resource{ns, table, status, other, domain, column}
			desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: resources}}
			current := desired
			current.Graph.Resources = resources[:len(resources)-1]
			changes := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "create", Operation: schema.OperationCreate, ResourceID: column.ID, After: &column}}}
			out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired})
			if fixture.want && (err != nil || len(out) != 1) {
				t.Fatalf("statements=%+v err=%v", out, err)
			}
			if !fixture.want && (err == nil || len(out) != 0) {
				t.Fatalf("unsafe default rendered: %+v err=%v", out, err)
			}
			if !fixture.want && (strings.Contains(err.Error(), "array default") || !strings.Contains(err.Error(), fixture.target.Name.String())) {
				t.Fatalf("user-defined diagnostic was not actionable: %v", err)
			}
		})
	}
	ambiguous := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "ambiguous", Parent: table.ID}, `{"type":"app.job_status","default":"'pending'::app.job_status","not_null":false,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: status.ID, Type: schema.DependencyUses}, schema.Dependency{Target: other.ID, Type: schema.DependencyUses})
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, status, other, domain, ambiguous}}}
	current := desired
	current.Graph.Resources = desired.Graph.Resources[:len(desired.Graph.Resources)-1]
	changes := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "create", Operation: schema.OperationCreate, ResourceID: ambiguous.ID, After: &ambiguous}}}
	if out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired}); err == nil || len(out) != 0 {
		t.Fatalf("ambiguous uses dependencies rendered: out=%+v err=%v", out, err)
	}
}

func TestSequenceDefaultsRequireExactReferenceDependency(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "widgets", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	sequence := renderResource(schema.KindSequence, schema.Name{Schema: "app", Name: "widgets_id_seq", Parent: ns.ID}, `{"start":1,"increment":1}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	other := renderResource(schema.KindSequence, schema.Name{Schema: "app", Name: "other_seq", Parent: ns.ID}, `{"start":1,"increment":1}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	for _, fixture := range []struct {
		name, value string
		deps        []schema.Dependency
		want        bool
	}{
		{"exact", "nextval('app.widgets_id_seq'::regclass)", []schema.Dependency{{Target: sequence.ID, Type: schema.DependencyReferences}}, true},
		{"qualified function", "pg_catalog.nextval('app.widgets_id_seq'::pg_catalog.regclass)", []schema.Dependency{{Target: sequence.ID, Type: schema.DependencyReferences}}, true},
		{"renamed", "nextval('app.old_seq'::regclass)", []schema.Dependency{{Target: sequence.ID, Type: schema.DependencyReferences}}, false},
		{"mismatched", "nextval('app.other_seq'::regclass)", []schema.Dependency{{Target: sequence.ID, Type: schema.DependencyReferences}}, false},
		{"missing", "nextval('app.widgets_id_seq'::regclass)", nil, false},
		{"unqualified", "nextval('widgets_id_seq'::regclass)", []schema.Dependency{{Target: sequence.ID, Type: schema.DependencyReferences}}, false},
		{"composed", "nextval(('app.' || 'widgets_id_seq')::regclass)", []schema.Dependency{{Target: sequence.ID, Type: schema.DependencyReferences}}, false},
		{"ambiguous", "nextval('app.widgets_id_seq'::regclass)", []schema.Dependency{{Target: sequence.ID, Type: schema.DependencyReferences}, {Target: other.ID, Type: schema.DependencyReferences}}, false},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			deps := []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}
			deps = append(deps, fixture.deps...)
			column := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "id", Parent: table.ID}, `{"type":"bigint","default":"`+strings.ReplaceAll(fixture.value, `"`, `\"`)+`","not_null":false,"ordinal":1}`, deps...)
			resources := []schema.Resource{ns, table, sequence, other, column}
			desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: resources}}
			current := desired
			current.Graph.Resources = resources[:len(resources)-1]
			changes := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "create", Operation: schema.OperationCreate, ResourceID: column.ID, After: &column}}}
			out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired})
			if fixture.want && (err != nil || len(out) != 1) {
				t.Fatalf("statements=%+v err=%v", out, err)
			}
			if !fixture.want && (err == nil || len(out) != 0) {
				t.Fatalf("unsafe nextval rendered: %+v err=%v", out, err)
			}
		})
	}
}

func TestDefaultValidationIsScopedToMutationAndDependencyClosure(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "widgets", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	legacy := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "legacy", Parent: table.ID}, `{"type":"integer","default":"lower('do-not-leak')","not_null":false,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	added := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "added", Parent: table.ID}, `{"type":"text","default":"'safe'","not_null":false,"ordinal":2}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	current := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, legacy}}}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, legacy, added}}}
	create := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "create", Operation: schema.OperationCreate, ResourceID: added.ID, After: &added}}}
	if out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: create, Current: current, Desired: desired}); err != nil || len(out) != 1 {
		t.Fatalf("unrelated safe mutation was blocked: out=%+v err=%v", out, err)
	}
	changed := legacy
	changed.Spec = []byte(`{"type":"integer","default":"lower('do-not-leak')","not_null":true,"ordinal":1}`)
	desired.Graph.Resources = []schema.Resource{ns, table, changed}
	alter := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "alter", Operation: schema.OperationAlter, ResourceID: changed.ID, Before: &legacy, After: &changed}}}
	if out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: alter, Current: current, Desired: desired}); err == nil || len(out) != 0 || !strings.Contains(err.Error(), "app.legacy") || !strings.Contains(err.Error(), "normalized type \"integer\"") || !strings.Contains(err.Error(), "function lower") || strings.Contains(err.Error(), "do-not-leak") {
		t.Fatalf("scoped rejection diagnostic out=%+v err=%v", out, err)
	}
}

func TestChangedTypeValidatesDependentUnchangedDefault(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "widgets", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	beforeType := renderResource(schema.KindEnum, schema.Name{Schema: "app", Name: "status", Parent: ns.ID}, `{"values":["pending"]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	afterType := beforeType
	afterType.Spec = []byte(`{"values":["pending","done"]}`)
	column := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "state", Parent: table.ID}, `{"type":"app.status","default":"'missing'::app.status","not_null":false,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: beforeType.ID, Type: schema.DependencyUses})
	current := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, beforeType, column}}}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, afterType, column}}}
	changes := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "alter", Operation: schema.OperationAlter, ResourceID: afterType.ID, Before: &beforeType, After: &afterType}}}
	if out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired}); err == nil || len(out) != 0 || !strings.Contains(err.Error(), "label is not declared") {
		t.Fatalf("dependent invalid default was not rejected: out=%+v err=%v", out, err)
	}
}
