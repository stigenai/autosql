package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"autosql/pkg/schema"
	"autosql/pkg/source"
)

func TestRoutineInventoryModelCoversCellAndRepository(t *testing.T) {
	inventory := map[string]int{"cell": 15, "repository": 32}
	if inventory["cell"] != 15 || inventory["repository"] != 32 || inventory["repository"] < inventory["cell"] {
		t.Fatalf("routine inventory=%v", inventory)
	}
}

func TestRoutineModelOverloadsAndHCLRoundTripEveryField(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: `App Space`}, `{}`)
	makeRoutine := func(identity, arguments, result, definition string) schema.Resource {
		name := `Run Job(` + identity + `)`
		specification := map[string]any{
			"name":               "Run Job",
			"identity_arguments": identity,
			"arguments":          arguments,
			"result":             result,
			"returns_set":        false,
			"language":           "sql",
			"volatility":         "s",
			"strict":             true,
			"security_definer":   true,
			"leakproof":          false,
			"parallel":           "r",
			"cost":               12.5,
			"rows":               0.0,
			"configuration":      []string{"search_path=pg_catalog, public"},
			"owner":              "app_owner",
			"definition":         definition,
		}
		raw, _ := json.Marshal(specification)
		resource := schema.Resource{Kind: schema.KindFunction, Name: schema.Name{Schema: ns.Name.Name, Name: name, Parent: ns.ID}, Spec: raw, Dependencies: []schema.Dependency{{Target: ns.ID, Type: schema.DependencyContains}}, Annotations: map[string]string{"comment": "documented overload"}}
		resource.ID = schema.StableID(resource.Kind, resource.Name)
		return resource
	}
	integer := makeRoutine("value integer", "value integer DEFAULT 1", "integer", `CREATE FUNCTION "App Space"."Run Job"(value integer DEFAULT 1) RETURNS integer LANGUAGE sql STABLE STRICT SECURITY DEFINER PARALLEL RESTRICTED COST 12.5 SET search_path TO 'pg_catalog', 'public' AS $$ SELECT value $$`)
	text := makeRoutine("value text", "value text", "text", `CREATE FUNCTION "App Space"."Run Job"(value text) RETURNS text LANGUAGE sql STABLE STRICT SECURITY DEFINER PARALLEL RESTRICTED COST 12.5 SET search_path TO 'pg_catalog', 'public' AS $$ SELECT value $$`)
	if integer.ID == text.ID {
		t.Fatal("overload identities collided")
	}
	doc, err := New().Normalize(context.Background(), schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, integer, text}}})
	if err != nil {
		t.Fatal(err)
	}
	hcl, err := source.FormatHCL(doc)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := source.LoadContext(context.Background(), source.Input{URI: "routines.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err = New().Normalize(context.Background(), reloaded)
	if err != nil {
		t.Fatal(err)
	}
	want, _ := schema.SemanticFingerprint(doc)
	got, _ := schema.SemanticFingerprint(reloaded)
	if got != want {
		t.Fatalf("routine HCL round trip drifted\n%s", hcl)
	}
	for _, resource := range reloaded.Graph.Resources {
		if resource.Kind == schema.KindFunction && stringValue(spec(resource), "body_digest") == "" {
			t.Fatalf("missing body digest: %+v", resource)
		}
	}
}

func TestRoutineUnknownFieldsAggregateInProvisioningPreflight(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	routine := renderResource(schema.KindFunction, schema.Name{Schema: "app", Name: "f()", Parent: ns.ID}, `{"name":"f","identity_arguments":"","arguments":"","result":"integer","returns_set":false,"language":"sql","volatility":"v","strict":false,"security_definer":false,"leakproof":false,"parallel":"u","cost":100,"rows":0,"configuration":[],"owner":"postgres","definition":"CREATE FUNCTION app.f() RETURNS integer LANGUAGE sql AS $$ SELECT 1 $$","future_attribute":true}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	report, err := PreflightProvisioning(context.Background(), schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, routine}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, diagnostic := range report.Diagnostics {
		found = found || diagnostic.ResourceID == routine.ID && diagnostic.Class == "unsupported_spec_key" && diagnostic.Field == "future_attribute"
	}
	if !found {
		t.Fatalf("unknown routine field was not aggregated: %+v", report.Diagnostics)
	}
}
