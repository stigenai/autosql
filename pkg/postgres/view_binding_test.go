package postgres

import (
	"context"
	"strings"
	"testing"

	"autosql/pkg/schema"
	"autosql/pkg/source"
)

func TestViewRelationsAndTypeCastsAreSchemaBound(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "cell"}, `{}`)
	status := renderResource(schema.KindEnum, schema.Name{Schema: "cell", Name: "status", Parent: ns.ID}, `{"values":["active"]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	table := renderResource(schema.KindTable, schema.Name{Schema: "cell", Name: "blocks", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	column := renderResource(schema.KindColumn, schema.Name{Schema: "cell", Name: "status", Parent: table.ID}, `{"type":"cell.status","not_null":false,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: status.ID, Type: schema.DependencyUses})
	view := renderResource(schema.KindMaterializedView, schema.Name{Schema: "cell", Name: "block_health_summary", Parent: ns.ID}, `{"definition":"SELECT status::status AS status FROM blocks"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains}, schema.Dependency{Target: table.ID, Type: schema.DependencyReferences})
	projection := projection(view, "status", "cell.status")
	projection.Dependencies = append(projection.Dependencies, schema.Dependency{Target: status.ID, Type: schema.DependencyUses})
	document := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, status, table, column, view, projection}}}

	normalized, err := New().Normalize(context.Background(), document)
	if err != nil {
		t.Fatal(err)
	}
	var normalizedView schema.Resource
	for _, resource := range normalized.Graph.Resources {
		if resource.ID == view.ID {
			normalizedView = resource
		}
	}
	definition := stringValue(spec(normalizedView), "definition")
	if !strings.Contains(definition, "FROM cell.blocks") || !strings.Contains(definition, "::cell.status") {
		t.Fatalf("view definition is not schema-bound: %s", definition)
	}
	if !hasDependency(normalizedView, status.ID, schema.DependencyUses) {
		t.Fatalf("normalized view is missing exact type dependency: %+v", normalizedView.Dependencies)
	}
	statements, err := RenderDocument(context.Background(), normalized, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, statement := range statements {
		if strings.Contains(statement.SQL, "MATERIALIZED VIEW") {
			found = strings.Contains(statement.SQL, "FROM cell.blocks") && strings.Contains(statement.SQL, "::cell.status")
		}
	}
	if !found {
		t.Fatalf("schema-bound materialized view statement is missing: %+v", statements)
	}

	hcl, err := source.FormatHCL(normalized)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := source.LoadContext(context.Background(), source.Input{URI: "schema-bound-view.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err = New().Normalize(context.Background(), roundTrip)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := schema.Diff(normalized, roundTrip, schema.DiffOptions{})
	if err != nil || len(changes.Changes) != 0 {
		t.Fatalf("schema-bound view HCL round trip drifted: changes=%+v err=%v\n%s", changes.Changes, err, hcl)
	}
}

func TestViewSchemaBindingFailsClosed(t *testing.T) {
	cell := renderResource(schema.KindSchema, schema.Name{Name: "cell"}, `{}`)
	other := renderResource(schema.KindSchema, schema.Name{Name: "other"}, `{}`)
	cellTable := renderResource(schema.KindTable, schema.Name{Schema: "cell", Name: "blocks", Parent: cell.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: cell.ID, Type: schema.DependencyContains})
	otherTable := renderResource(schema.KindTable, schema.Name{Schema: "other", Name: "blocks", Parent: other.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: other.ID, Type: schema.DependencyContains})
	base := renderResource(schema.KindView, schema.Name{Schema: "cell", Name: "v", Parent: cell.ID}, `{"definition":"SELECT id FROM blocks"}`, schema.Dependency{Target: cell.ID, Type: schema.DependencyContains}, schema.Dependency{Target: cellTable.ID, Type: schema.DependencyReferences})
	resources := map[string]schema.Resource{cell.ID: cell, other.ID: other, cellTable.ID: cellTable, otherTable.ID: otherTable, base.ID: base}
	for name, mutate := range map[string]func(*schema.Resource){
		"missing relation": func(view *schema.Resource) {
			view.Dependencies = view.Dependencies[:1]
		},
		"ambiguous bare relation": func(view *schema.Resource) {
			view.Dependencies = append(view.Dependencies, schema.Dependency{Target: otherTable.ID, Type: schema.DependencyReferences})
		},
		"qualified mismatch": func(view *schema.Resource) {
			view.Spec = []byte(`{"definition":"SELECT id FROM other.blocks"}`)
		},
		"CTE collision": func(view *schema.Resource) {
			view.Spec = []byte(`{"definition":"WITH blocks AS (SELECT 1 AS id) SELECT id FROM blocks"}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := base
			candidate.Dependencies = append([]schema.Dependency(nil), base.Dependencies...)
			mutate(&candidate)
			if _, err := schemaBindViewDefinition(candidate, resources); err == nil {
				t.Fatal("unsafe view definition was accepted")
			}
		})
	}
}

func hasDependency(resource schema.Resource, target string, dependencyType schema.DependencyType) bool {
	for _, dependency := range resource.Dependencies {
		if dependency.Target == target && dependency.Type == dependencyType {
			return true
		}
	}
	return false
}
