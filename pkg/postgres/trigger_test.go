package postgres

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
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
	if err := validateTriggerSpec(trigger, resources); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*schema.Resource){
		"statement smuggling": func(resource *schema.Resource) {
			resource.Spec = json.RawMessage(`{"definition":"CREATE TRIGGER items_audit BEFORE UPDATE ON app.items FOR EACH ROW EXECUTE FUNCTION app.audit_event(); DROP TABLE app.items","enabled":"O","columns":[]}`)
		},
		"wrong table": func(resource *schema.Resource) {
			resource.Spec = json.RawMessage(`{"definition":"CREATE TRIGGER items_audit BEFORE UPDATE ON app.other FOR EACH ROW EXECUTE FUNCTION app.audit_event()","enabled":"O","columns":[]}`)
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
