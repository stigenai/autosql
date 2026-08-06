package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
	"autosql/pkg/source"
)

func triggerFixture() (schema.Resource, schema.Resource, schema.Resource, schema.Resource) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "items", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	function := renderResource(schema.KindFunction, schema.Name{Schema: "app", Name: "audit_event()", Parent: ns.ID}, `{"name":"audit_event","identity_arguments":"","arguments":"","result":"trigger","returns_set":false,"language":"plpgsql","volatility":"v","strict":false,"security_definer":false,"leakproof":false,"parallel":"u","cost":100,"rows":0,"configuration":[],"owner":"postgres","definition":"CREATE FUNCTION app.audit_event() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	trigger := renderResource(schema.KindTrigger, schema.Name{Schema: "app", Name: "items_audit", Parent: table.ID}, `{"definition":"CREATE TRIGGER items_audit BEFORE UPDATE ON app.items FOR EACH ROW EXECUTE FUNCTION app.audit_event()","enabled":"O","columns":[]}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: function.ID, Type: schema.DependencyReferences})
	return ns, table, function, trigger
}

func TestTriggerDefinitionAndDependencyValidationFailClosed(t *testing.T) {
	ns, table, function, trigger := triggerFixture()
	resources := map[string]schema.Resource{ns.ID: ns, table.ID: table, function.ID: function, trigger.ID: trigger}
	parsed, err := parseTriggerDefinition(trigger, resources)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parsed.SQL, " ON app.items ") {
		t.Fatalf("qualified trigger SQL=%q", parsed.SQL)
	}
	unqualified := trigger
	unqualified.Spec = json.RawMessage(`{"definition":"CREATE TRIGGER items_audit BEFORE UPDATE ON items FOR EACH ROW EXECUTE FUNCTION audit_event()","enabled":"O","columns":[]}`)
	parsed, err = parseTriggerDefinition(unqualified, resources)
	if err != nil || !strings.Contains(parsed.SQL, " ON app.items ") || !strings.Contains(parsed.SQL, "EXECUTE FUNCTION app.audit_event()") {
		t.Fatalf("unqualified trigger SQL=%q err=%v", parsed.SQL, err)
	}
	normalized, err := New().Normalize(context.Background(), schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, function, unqualified}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range normalized.Graph.Resources {
		if resource.Kind == schema.KindTrigger {
			definition := stringValue(spec(resource), "definition")
			if !strings.Contains(definition, " ON app.items ") || !strings.Contains(definition, "EXECUTE FUNCTION app.audit_event()") {
				t.Fatalf("normalized trigger definition=%q", definition)
			}
		}
	}
	hcl, err := source.FormatHCL(normalized)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := source.LoadContext(context.Background(), source.Input{URI: "schema-bound-trigger.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err = New().Normalize(context.Background(), roundTrip)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := schema.Diff(normalized, roundTrip, schema.DiffOptions{})
	if err != nil || len(changes.Changes) != 0 {
		t.Fatalf("schema-bound trigger HCL round trip drifted: changes=%+v err=%v\n%s", changes.Changes, err, hcl)
	}
	for name, mutate := range map[string]func(*schema.Resource){
		"statement smuggling": func(resource *schema.Resource) {
			resource.Spec = json.RawMessage(`{"definition":"CREATE TRIGGER items_audit BEFORE UPDATE ON app.items FOR EACH ROW EXECUTE FUNCTION app.audit_event(); DROP TABLE app.items","enabled":"O","columns":[]}`)
		},
		"wrong table": func(resource *schema.Resource) {
			resource.Spec = json.RawMessage(`{"definition":"CREATE TRIGGER items_audit BEFORE UPDATE ON app.other FOR EACH ROW EXECUTE FUNCTION app.audit_event()","enabled":"O","columns":[]}`)
		},
		"wrong schema": func(resource *schema.Resource) {
			resource.Spec = json.RawMessage(`{"definition":"CREATE TRIGGER items_audit BEFORE UPDATE ON other.items FOR EACH ROW EXECUTE FUNCTION app.audit_event()","enabled":"O","columns":[]}`)
		},
		"wrong function": func(resource *schema.Resource) {
			resource.Spec = json.RawMessage(`{"definition":"CREATE TRIGGER items_audit BEFORE UPDATE ON app.items FOR EACH ROW EXECUTE FUNCTION other_event()","enabled":"O","columns":[]}`)
		},
		"wrong function schema": func(resource *schema.Resource) {
			resource.Spec = json.RawMessage(`{"definition":"CREATE TRIGGER items_audit BEFORE UPDATE ON app.items FOR EACH ROW EXECUTE FUNCTION other.audit_event()","enabled":"O","columns":[]}`)
		},
		"unknown mode": func(resource *schema.Resource) {
			resource.Spec = json.RawMessage(`{"definition":"CREATE TRIGGER items_audit BEFORE UPDATE ON app.items FOR EACH ROW EXECUTE FUNCTION app.audit_event()","enabled":"X","columns":[]}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := trigger
			mutate(&candidate)
			if err := validateTriggerSpec(candidate, resources); err == nil {
				t.Fatal("unsafe trigger accepted")
			}
		})
	}
	missingFunction := trigger
	missingFunction.Dependencies = []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}
	if err := validateSemanticDependencies(missingFunction, resources); err == nil {
		t.Fatal("trigger without exact function dependency accepted")
	}
	otherFunction := function
	otherFunction.Name.Name = "other_event()"
	otherFunction.ID = schema.StableID(otherFunction.Kind, otherFunction.Name)
	otherFunction.Spec = json.RawMessage(strings.ReplaceAll(string(function.Spec), "audit_event", "other_event"))
	resources[otherFunction.ID] = otherFunction
	ambiguous := trigger
	ambiguous.Dependencies = append(append([]schema.Dependency(nil), trigger.Dependencies...), schema.Dependency{Target: otherFunction.ID, Type: schema.DependencyReferences})
	if _, err := parseTriggerDefinition(ambiguous, resources); err == nil {
		t.Fatal("trigger with ambiguous function dependency accepted")
	}
}

func TestGlobalTriggerFunctionInventoryIsSchemaBound(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "global"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "global", Name: "items", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	for triggerName, functionName := range map[string]string{
		"tenant_assignments_capacity_trigger": "update_cell_capacity",
		"cells_updated_at":                    "update_updated_at_column",
		"tenant_assignments_history_trigger":  "record_assignment_history",
	} {
		t.Run(triggerName, func(t *testing.T) {
			function := renderResource(schema.KindFunction, schema.Name{Schema: "global", Name: functionName + "()", Parent: ns.ID}, fmt.Sprintf(`{"name":%q,"identity_arguments":"","arguments":"","result":"trigger","returns_set":false,"language":"plpgsql","volatility":"v","strict":false,"security_definer":false,"leakproof":false,"parallel":"u","cost":100,"rows":0,"configuration":[],"owner":"postgres","definition":%q}`, functionName, "CREATE FUNCTION global."+functionName+"() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RETURN NEW; END $$"), schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
			trigger := renderResource(schema.KindTrigger, schema.Name{Schema: "global", Name: triggerName, Parent: table.ID}, fmt.Sprintf(`{"definition":%q,"enabled":"O","columns":[]}`, "CREATE TRIGGER "+triggerName+" BEFORE UPDATE ON items FOR EACH ROW EXECUTE FUNCTION "+functionName+"()"), schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: function.ID, Type: schema.DependencyReferences})
			resources := map[string]schema.Resource{ns.ID: ns, table.ID: table, function.ID: function, trigger.ID: trigger}
			parsed, err := parseTriggerDefinition(trigger, resources)
			if err != nil {
				t.Fatal(err)
			}
			if !strings.Contains(parsed.SQL, "EXECUTE FUNCTION global."+functionName+"()") {
				t.Fatalf("trigger function is not schema-bound: %s", parsed.SQL)
			}
		})
	}
}

func TestTriggerEnablementRenameRebuildAndDropSQL(t *testing.T) {
	ns, table, function, before := triggerFixture()
	current := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, function, before}}}
	after := before
	after.Spec = json.RawMessage(strings.Replace(string(before.Spec), `"enabled":"O"`, `"enabled":"A"`, 1))
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, function, after}}}
	changes, _ := schema.Diff(current, desired, schema.DiffOptions{})
	statements, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired})
	if err != nil || len(statements) != 1 || !strings.Contains(statements[0].SQL, "ENABLE ALWAYS TRIGGER") {
		t.Fatalf("enable statements=%+v err=%v", statements, err)
	}
	resources := resourceMapForRender(current)
	renamed := before
	renamed.Name.Name = "items_audit_v2"
	if sql, renameErr := renderRename(before, renamed, resources); renameErr != nil || len(sql) != 1 || !strings.Contains(sql[0], "RENAME TO") {
		t.Fatalf("rename=%v err=%v", sql, renameErr)
	}
	if sql, dropErr := renderDrop(before, resources, nil); dropErr != nil || len(sql) != 1 || !strings.HasPrefix(sql[0], "DROP TRIGGER") {
		t.Fatalf("drop=%v err=%v", sql, dropErr)
	}
	rebuilt := before
	rebuilt.Spec = json.RawMessage(strings.Replace(string(before.Spec), "BEFORE UPDATE", "AFTER UPDATE", 1))
	if sql, rebuildErr := renderAlter(before, rebuilt, resources, map[string]string{"allow_rebuild": "true"}); rebuildErr != nil || len(sql) != 2 {
		t.Fatalf("rebuild=%v err=%v", sql, rebuildErr)
	}
}
