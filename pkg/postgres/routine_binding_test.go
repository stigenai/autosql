package postgres

import (
	"context"
	"strings"
	"testing"

	"autosql/pkg/schema"
	"autosql/pkg/source"
)

func TestRoutineSignatureAndRuntimeSearchPathAreSchemaBound(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "public"}, `{}`)
	state := renderResource(schema.KindEnum, schema.Name{Schema: "public", Name: "lifecycle_state", Parent: ns.ID}, `{"values":["active"]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	definition := "CREATE OR REPLACE FUNCTION public.lifecycle_state_to_v2(s lifecycle_state)\n RETURNS text\n LANGUAGE sql\nAS $function$\n  -- preserve this reviewed body line\n  SELECT s::lifecycle_state::text;\n$function$"
	routine := renderResource(schema.KindFunction, schema.Name{Schema: "public", Name: "lifecycle_state_to_v2(s lifecycle_state)", Parent: ns.ID}, `{"name":"lifecycle_state_to_v2","identity_arguments":"s lifecycle_state","arguments":"s lifecycle_state","result":"text","returns_set":false,"language":"sql","volatility":"i","strict":false,"security_definer":false,"leakproof":false,"parallel":"u","cost":100,"rows":0,"configuration":[],"owner":"postgres","definition":`+quotedJSON(definition)+`}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains}, schema.Dependency{Target: state.ID, Type: schema.DependencyUses})
	document := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, state, routine}}}
	normalized, err := New().Normalize(context.Background(), document)
	if err != nil {
		t.Fatal(err)
	}
	resources := resourceMapForRender(normalized)
	got := resources[routine.ID]
	canonical := stringValue(spec(got), "definition")
	if !strings.Contains(canonical, `s public.lifecycle_state`) || !strings.Contains(canonical, " SET search_path TO 'pg_catalog', 'public'") {
		t.Fatalf("routine header is not schema-bound:\n%s", canonical)
	}
	if !strings.Contains(canonical, "$function$\n  -- preserve this reviewed body line\n  SELECT s::lifecycle_state::text;\n$function$") {
		t.Fatalf("routine body changed during header binding:\n%s", canonical)
	}
	if stringValue(spec(got), "body_digest") != routineDefinitionDigest(canonical) {
		t.Fatal("routine digest was not rebound to canonical source")
	}
	signature, err := routineSignature(got, resources)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(signature, "public.lifecycle_state") {
		t.Fatalf("routine identity signature is not schema-bound: %s", signature)
	}
	options := map[string]string{"reviewed_routine_digests": stringValue(spec(got), "body_digest")}
	if _, err := validateRoutineSource(got, resources, options); err != nil {
		t.Fatalf("canonical reviewed routine rejected: %v", err)
	}

	hcl, err := source.FormatHCL(normalized)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := source.LoadContext(context.Background(), source.Input{URI: "schema-bound-routine.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err = New().Normalize(context.Background(), roundTrip)
	if err != nil {
		t.Fatal(err)
	}
	changes, err := schema.Diff(normalized, roundTrip, schema.DiffOptions{})
	if err != nil || len(changes.Changes) != 0 {
		t.Fatalf("schema-bound routine HCL round trip drifted: changes=%+v err=%v\n%s", changes.Changes, err, hcl)
	}
}

func TestRoutineSignatureTypeBindingRequiresExactDependency(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "cell"}, `{}`)
	state := renderResource(schema.KindEnum, schema.Name{Schema: "cell", Name: "state", Parent: ns.ID}, `{"values":["active"]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	routine := renderResource(schema.KindFunction, schema.Name{Schema: "cell", Name: "f(s state)", Parent: ns.ID}, `{"name":"f","identity_arguments":"s state","arguments":"s state","result":"state","returns_set":false,"language":"sql","volatility":"v","strict":false,"security_definer":false,"leakproof":false,"parallel":"u","cost":100,"rows":0,"configuration":[],"owner":"postgres","definition":"CREATE FUNCTION cell.f(s state) RETURNS state LANGUAGE sql AS $$ SELECT s $$"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	resources := map[string]schema.Resource{ns.ID: ns, state.ID: state, routine.ID: routine}
	if _, _, err := schemaBindRoutineSource(routine, resources); err == nil {
		t.Fatal("routine custom type without an exact uses dependency was accepted")
	}
	if _, err := routineSignature(routine, resources); err == nil {
		t.Fatal("routine identity custom type without an exact uses dependency was accepted")
	}
}

func quotedJSON(value string) string {
	return `"` + strings.NewReplacer(`\`, `\\`, `"`, `\"`, "\n", `\n`, "\r", `\r`, "\t", `\t`).Replace(value) + `"`
}
