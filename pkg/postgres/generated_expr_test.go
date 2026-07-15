package postgres

import (
	"context"
	"encoding/json"
	"reflect"
	"slices"
	"strings"
	"testing"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
	"autosql/pkg/source"
)

func generatedDependencyFixture() (schema.Document, schema.Resource, schema.Resource, schema.Resource) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "jobs", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	sourceColumn := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "state", Parent: table.ID}, `{"type":"text","not_null":false,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	function := renderResource(schema.KindFunction, schema.Name{Schema: "app", Name: "lifecycle_state_to_v2(text)", Parent: ns.ID}, `{"name":"lifecycle_state_to_v2","identity_arguments":"text","result":"text","language":"sql","volatility":"i","security_definer":false,"leakproof":false,"parallel":"s","definition":"CREATE FUNCTION app.lifecycle_state_to_v2(value text) RETURNS text LANGUAGE sql IMMUTABLE RETURN value"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	generated := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "state_v2", Parent: table.ID}, `{"type":"text","not_null":false,"ordinal":2,"default":"app.lifecycle_state_to_v2(state)","generated":"s"}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: sourceColumn.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: function.ID, Type: schema.DependencyReferences})
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, sourceColumn, function, generated}}}
	doc.Normalize()
	return doc, sourceColumn, function, generated
}

func TestGeneratedDependenciesAreExactAndRoundTripHCL(t *testing.T) {
	doc, sourceColumn, function, generated := generatedDependencyFixture()
	resources := resourceMapForRender(doc)
	if err := validateGeneratedDependencies(generated, resources); err != nil {
		t.Fatal(err)
	}
	hcl, err := source.FormatHCL(doc)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := source.LoadContext(context.Background(), source.Input{URI: "generated.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	reloaded.Normalize()
	got := resourceMapForRender(reloaded)[generated.ID]
	if !reflect.DeepEqual(got.Dependencies, generated.Dependencies) {
		t.Fatalf("dependencies did not round-trip: got=%+v want=%+v", got.Dependencies, generated.Dependencies)
	}

	for name, mutate := range map[string]func(*schema.Document, *schema.Resource){
		"missing source": func(_ *schema.Document, column *schema.Resource) {
			column.Dependencies = slices.DeleteFunc(column.Dependencies, func(dependency schema.Dependency) bool { return dependency.Target == sourceColumn.ID })
		},
		"missing routine": func(_ *schema.Document, column *schema.Resource) {
			column.Dependencies = slices.DeleteFunc(column.Dependencies, func(dependency schema.Dependency) bool { return dependency.Target == function.ID })
		},
		"extra": func(_ *schema.Document, column *schema.Resource) {
			column.Dependencies = append(column.Dependencies, schema.Dependency{Target: column.Name.Parent, Type: schema.DependencyReferences})
		},
		"renamed reference": func(_ *schema.Document, column *schema.Resource) {
			values := spec(*column)
			values["default"] = "app.lifecycle_state_to_v2(old_state)"
			column.Spec, _ = json.Marshal(values)
		},
		"ambiguous overload": func(document *schema.Document, column *schema.Resource) {
			ns := resources[function.Name.Parent]
			overload := renderResource(schema.KindFunction, schema.Name{Schema: "app", Name: "lifecycle_state_to_v2(character varying)", Parent: ns.ID}, `{"name":"lifecycle_state_to_v2","identity_arguments":"character varying","result":"text","language":"sql","volatility":"i","security_definer":false,"leakproof":false,"parallel":"s","definition":"CREATE FUNCTION app.lifecycle_state_to_v2(value character varying) RETURNS text LANGUAGE sql IMMUTABLE RETURN value"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
			column.Dependencies = append(column.Dependencies, schema.Dependency{Target: overload.ID, Type: schema.DependencyReferences})
			document.Graph.Resources = append(document.Graph.Resources, overload)
		},
		"declared edge cannot guess overload": func(document *schema.Document, column *schema.Resource) {
			ns := resources[function.Name.Parent]
			overload := renderResource(schema.KindFunction, schema.Name{Schema: "app", Name: "lifecycle_state_to_v2(character varying)", Parent: ns.ID}, `{"name":"lifecycle_state_to_v2","identity_arguments":"character varying","result":"text","language":"sql","volatility":"i","security_definer":false,"leakproof":false,"parallel":"s","definition":"CREATE FUNCTION app.lifecycle_state_to_v2(value character varying) RETURNS text LANGUAGE sql IMMUTABLE RETURN value"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
			for index := range column.Dependencies {
				if column.Dependencies[index].Target == function.ID {
					column.Dependencies[index].Target = overload.ID
				}
			}
			document.Graph.Resources = append(document.Graph.Resources, overload)
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := cloneGeneratedDocument(t, doc)
			var column *schema.Resource
			for index := range candidate.Graph.Resources {
				if candidate.Graph.Resources[index].ID == generated.ID {
					column = &candidate.Graph.Resources[index]
				}
			}
			mutate(&candidate, column)
			if err := validateGeneratedDependencies(*column, resourceMapForRender(candidate)); err == nil {
				t.Fatal("invalid generated dependency set was accepted")
			}
		})
	}
}

func cloneGeneratedDocument(t *testing.T, input schema.Document) schema.Document {
	t.Helper()
	raw, err := json.Marshal(input)
	if err != nil {
		t.Fatal(err)
	}
	var output schema.Document
	if err := json.Unmarshal(raw, &output); err != nil {
		t.Fatal(err)
	}
	return output
}

func TestGeneratedDependenciesOrderPrerequisites(t *testing.T) {
	doc, sourceColumn, function, generated := generatedDependencyFixture()
	empty := schema.Document{Version: schema.SchemaVersion}
	changes, err := schema.Diff(empty, doc, schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	changeByResource := map[string]schema.Change{}
	position := map[string]int{}
	for index, change := range changes.Changes {
		changeByResource[change.ResourceID] = change
		position[change.ResourceID] = index
	}
	generatedChange := changeByResource[generated.ID]
	wantDependencies := map[string]bool{changeByResource[sourceColumn.ID].ID: false, changeByResource[function.ID].ID: false}
	for _, dependency := range generatedChange.DependsOn {
		if _, ok := wantDependencies[dependency]; ok {
			wantDependencies[dependency] = true
		}
	}
	for dependency, found := range wantDependencies {
		if !found {
			t.Errorf("generated change missing prerequisite change %s: %+v", dependency, generatedChange.DependsOn)
		}
	}
	if position[generated.ID] <= position[sourceColumn.ID] || position[generated.ID] <= position[function.ID] {
		t.Fatalf("generated column was ordered before prerequisites: %+v", position)
	}
}

func generatedRenderRequest(t testing.TB, desired schema.Document) plugin.RenderRequest {
	t.Helper()
	current := schema.Document{Version: schema.SchemaVersion}
	for _, resource := range desired.Graph.Resources {
		if resource.Kind == schema.KindSchema || resource.Kind == schema.KindFunction {
			current.Graph.Resources = append(current.Graph.Resources, resource)
		}
	}
	current.Normalize()
	desired.Normalize()
	changes, err := schema.Diff(current, desired, schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	return plugin.RenderRequest{Changes: changes, Current: current, Desired: desired}
}

func TestStoredGeneratedColumnCreatePolicy(t *testing.T) {
	doc, _, function, generated := generatedDependencyFixture()
	request := generatedRenderRequest(t, doc)
	out, err := New().Render(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, statement := range out {
		joined += statement.SQL + "\n"
	}
	if !strings.Contains(joined, `"state_v2" text GENERATED ALWAYS AS (app.lifecycle_state_to_v2(state)) STORED`) {
		t.Fatalf("generated clause missing:\n%s", joined)
	}

	for name, mutate := range map[string]func(*schema.Document, *schema.Resource){
		"statement": func(_ *schema.Document, column *schema.Resource) { setGeneratedExpression(column, "state; SELECT 1") },
		"comment": func(_ *schema.Document, column *schema.Resource) {
			setGeneratedExpression(column, "state /* hidden */")
		},
		"subquery":  func(_ *schema.Document, column *schema.Resource) { setGeneratedExpression(column, "(SELECT state)") },
		"operator":  func(_ *schema.Document, column *schema.Resource) { setGeneratedExpression(column, "state || 'x'") },
		"sql value": func(_ *schema.Document, column *schema.Resource) { setGeneratedExpression(column, "CURRENT_TIMESTAMP") },
		"undeclared column": func(_ *schema.Document, column *schema.Resource) {
			setGeneratedExpression(column, "app.lifecycle_state_to_v2(missing)")
		},
		"unmodeled routine": func(_ *schema.Document, column *schema.Resource) { setGeneratedExpression(column, "lower(state)") },
		"non-stored": func(_ *schema.Document, column *schema.Resource) {
			values := spec(*column)
			values["generated"] = "v"
			column.Spec, _ = json.Marshal(values)
		},
		"identity combination": func(_ *schema.Document, column *schema.Resource) {
			values := spec(*column)
			values["identity"] = "a"
			column.Spec, _ = json.Marshal(values)
		},
		"volatile routine": func(document *schema.Document, _ *schema.Resource) {
			setRoutineField(document, function.ID, "volatility", "v")
		},
		"non-sql routine": func(document *schema.Document, _ *schema.Resource) {
			setRoutineField(document, function.ID, "language", "plpgsql")
		},
		"security definer": func(document *schema.Document, _ *schema.Resource) {
			setRoutineField(document, function.ID, "security_definer", true)
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := cloneGeneratedDocument(t, doc)
			var column *schema.Resource
			for index := range candidate.Graph.Resources {
				if candidate.Graph.Resources[index].ID == generated.ID {
					column = &candidate.Graph.Resources[index]
				}
			}
			mutate(&candidate, column)
			request := generatedRenderRequest(t, candidate)
			if rendered, renderErr := New().Render(context.Background(), request); renderErr == nil || len(rendered) != 0 {
				t.Fatalf("rendered=%+v err=%v", rendered, renderErr)
			}
		})
	}

	altered := cloneGeneratedDocument(t, doc)
	for index := range altered.Graph.Resources {
		if altered.Graph.Resources[index].ID == generated.ID {
			setGeneratedExpression(&altered.Graph.Resources[index], "app.lifecycle_state_to_v2('constant')")
		}
	}
	changes, err := schema.Diff(doc, altered, schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if rendered, renderErr := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: doc, Desired: altered}); renderErr == nil || len(rendered) != 0 {
		t.Fatalf("generated alteration rendered=%+v err=%v", rendered, renderErr)
	}
}

func setGeneratedExpression(column *schema.Resource, expression string) {
	values := spec(*column)
	values["default"] = expression
	column.Spec, _ = json.Marshal(values)
}

func setRoutineField(document *schema.Document, id, field string, value any) {
	for index := range document.Graph.Resources {
		resource := &document.Graph.Resources[index]
		if resource.ID == id {
			values := spec(*resource)
			values[field] = value
			resource.Spec, _ = json.Marshal(values)
		}
	}
}

func FuzzStoredGeneratedExpressionFailsWithoutPartialSQL(f *testing.F) {
	for _, seed := range []string{"app.lifecycle_state_to_v2(state)", "state", "state || 'x'", "state; DROP TABLE app.jobs", "(SELECT state)", "state /* comment */"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, expression string) {
		if len(expression) > 4096 {
			t.Skip()
		}
		doc, _, _, generated := generatedDependencyFixture()
		for index := range doc.Graph.Resources {
			if doc.Graph.Resources[index].ID == generated.ID {
				setGeneratedExpression(&doc.Graph.Resources[index], expression)
			}
		}
		request := generatedRenderRequest(t, doc)
		out, err := New().Render(context.Background(), request)
		if err != nil && len(out) != 0 {
			t.Fatalf("failed render returned partial SQL: %+v err=%v", out, err)
		}
		if err == nil {
			if _, classifyErr := analyzeGeneratedExpression(expression); classifyErr != nil {
				t.Fatalf("renderer accepted unclassified expression %q: %v", expression, classifyErr)
			}
			joined := ""
			for _, statement := range out {
				joined += statement.SQL
			}
			if !strings.Contains(joined, "GENERATED ALWAYS AS (") {
				t.Fatalf("successful render omitted generated clause: %+v", out)
			}
		}
	})
}
