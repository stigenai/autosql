package postgres

import (
	"context"
	"encoding/json"
	"slices"
	"strings"
	"testing"

	"autosql/pkg/bootstrap"
	"autosql/pkg/plan"
	"autosql/pkg/schema"

	"github.com/hashicorp/hcl/v2/hclparse"
	"github.com/hashicorp/hcl/v2/hclsyntax"
)

// Compile-time security boundary: prepare returns review data, never the
// bootstrap.Plan accepted by ExecuteDatabaseBootstrapURL.
var _ func(context.Context, bootstrap.DatabaseTarget, schema.Document, BootstrapAuthorizationInventoryOptions) (BootstrapAuthorizationInventory, error) = PrepareBootstrapAuthorizationInventory

func TestPrepareBootstrapAuthorizationInventoryIsCompleteDeterministicAndPlanBound(t *testing.T) {
	desired, target := authorizationInventoryFixture(t)
	first, err := PrepareBootstrapAuthorizationInventory(context.Background(), target, desired, BootstrapAuthorizationInventoryOptions{Render: map[string]string{"concurrent_indexes": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	firstPlan, err := PlanDatabaseBootstrap(context.Background(), target, desired, plan.Options{Render: explicitlyAuthorizedRender(first, map[string]string{"concurrent_indexes": "true"})})
	if err != nil {
		t.Fatal(err)
	}
	if first.PlanDigest != firstPlan.Digest || len(first.Routines) != 4 || len(first.Extensions) != 2 {
		t.Fatalf("inventory=%+v plan=%s", first, firstPlan.Digest)
	}
	if first.Routines[0].Signature != `"app"."lookup"(value integer)` || first.Routines[0].SourceDigest == "" || len(first.Routines[0].Dependencies) == 0 {
		t.Fatalf("routine inventory=%+v", first.Routines)
	}
	for _, extension := range first.Extensions {
		if !extension.AllowlistRequired || !extension.ExactVersionRequired || !extension.SchemaPolicyRequired || !extension.ServerPackageRequired || extension.UntrustedExtensionAuthorizationRequired || extension.Version == "" || extension.Schema != "app" {
			t.Fatalf("extension inventory=%+v", extension)
		}
	}
	routines := map[string]BootstrapRoutineAuthorization{}
	for _, routine := range first.Routines {
		routines[routine.Name] = routine
	}
	if !routines["unsafe"].UnsafeLanguageAuthorizationRequired || routines["unsafe"].PrivilegedRoutineAuthorizationRequired || routines["unsafe"].TransactionControlAuthorizationRequired {
		t.Fatalf("unsafe-language gates=%+v", routines["unsafe"])
	}
	privileged := routines["privileged_refresh"]
	if privileged.UnsafeLanguageAuthorizationRequired || !privileged.PrivilegedRoutineAuthorizationRequired || !privileged.TransactionControlAuthorizationRequired {
		t.Fatalf("privileged procedure gates=%+v", privileged)
	}

	// Resource input order is not authority-bearing. Both the plan and its
	// review inventory must remain byte-identical.
	reordered := desired
	reordered.Graph.Resources = append([]schema.Resource(nil), desired.Graph.Resources...)
	slices.Reverse(reordered.Graph.Resources)
	second, err := PrepareBootstrapAuthorizationInventory(context.Background(), target, reordered, BootstrapAuthorizationInventoryOptions{Render: map[string]string{"concurrent_indexes": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	secondPlan, err := PlanDatabaseBootstrap(context.Background(), target, reordered, plan.Options{Render: explicitlyAuthorizedRender(second, map[string]string{"concurrent_indexes": "true"})})
	if err != nil {
		t.Fatal(err)
	}
	firstJSON, err := first.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	secondJSON, err := second.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(firstJSON, secondJSON) || firstPlan.Digest != secondPlan.Digest {
		t.Fatalf("inventory or plan is not deterministic\n%s\n%s", firstJSON, secondJSON)
	}
	firstHCL, err := first.MarshalHCL()
	if err != nil {
		t.Fatal(err)
	}
	secondHCL, err := second.MarshalHCL()
	if err != nil || !slices.Equal(firstHCL, secondHCL) {
		t.Fatalf("HCL inventory is not deterministic: err=%v", err)
	}
	if _, diagnostics := hclparse.NewParser().ParseHCL(firstHCL, "bootstrap-authorization.hcl"); diagnostics.HasErrors() {
		t.Fatalf("invalid HCL inventory: %s\n%s", diagnostics.Error(), firstHCL)
	}
}

func TestPrepareBootstrapAuthorizationInventoryReportsUntrustedExtensionWithoutWeakeningExecution(t *testing.T) {
	namespace := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	extension := renderResource(schema.KindExtension, schema.Name{Schema: "app", Name: "untrusted_tools", Parent: namespace.ID}, `{"version":"1.0","relocatable":true,"trusted":false,"superuser":true,"requires":[]}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{namespace, extension}}}
	desired.Normalize()
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: "db.internal", Port: 5432, TLSMode: "verify-full"}, MaintenanceDatabase: "postgres", Name: "untrusted_cell", Owner: "postgres", Encoding: "UTF8", Template: "template0", Tablespace: "pg_default", ConnectionLimit: -1, AllowConnections: true}

	inventory, err := PrepareBootstrapAuthorizationInventory(context.Background(), target, desired, BootstrapAuthorizationInventoryOptions{})
	if err != nil {
		t.Fatalf("real prepare rejected reportable untrusted extension gate: %v", err)
	}
	if len(inventory.Extensions) != 1 || !inventory.Extensions[0].UntrustedExtensionAuthorizationRequired || !inventory.Extensions[0].SuperuserRequired || inventory.PlanSummary.StepCount == 0 {
		t.Fatalf("inventory=%+v", inventory)
	}
	jsonInventory, err := inventory.MarshalCanonical()
	if err != nil || !strings.Contains(string(jsonInventory), `"untrusted_extension_authorization_required":true`) {
		t.Fatalf("JSON missing untrusted extension authorization: err=%v %s", err, jsonInventory)
	}
	hclInventory, err := inventory.MarshalHCL()
	if err != nil || !strings.Contains(string(hclInventory), `untrusted_extension_authorization_required = true`) {
		t.Fatalf("HCL missing untrusted extension authorization: err=%v %s", err, hclInventory)
	}

	// Prepare's synthetic permission must not leak into the ordinary plan or
	// execution path: the same graph remains fail-closed without authorization.
	render := map[string]string{"extension_allowlist": "untrusted_tools", "extension_version.untrusted_tools": "1.0", "extension_schemas.untrusted_tools": "app"}
	if _, err := PlanDatabaseBootstrap(context.Background(), target, desired, plan.Options{Render: render}); err == nil || !strings.Contains(err.Error(), "allow_untrusted_extensions") {
		t.Fatalf("execution plan did not preserve untrusted extension gate: %v", err)
	}
	render["allow_untrusted_extensions"] = "true"
	legacyPlan, err := PlanDatabaseBootstrap(context.Background(), target, desired, plan.Options{Render: render})
	if err != nil {
		t.Fatalf("explicitly authorized execution plan failed: %v", err)
	}
	if !hasBootstrapExtensionAuthorization(legacyPlan, desired.Graph.Resources[1].ID) {
		t.Fatal("legacy untrusted-extension authorization was not rebound to execution")
	}
	legacyPlan = legacyPlan.WithRuntimeAuthorization([]byte("forged"))
	if hasBootstrapExtensionAuthorization(legacyPlan, desired.Graph.Resources[1].ID) {
		t.Fatal("forged runtime authorization was accepted")
	}
}

func TestPrepareBootstrapAuthorizationInventoryOmitsSourcesAndSecretsByDefault(t *testing.T) {
	desired, target := authorizationInventoryFixture(t)
	inventory, err := PrepareBootstrapAuthorizationInventory(context.Background(), target, desired, BootstrapAuthorizationInventoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := inventory.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	text := string(raw)
	if strings.Contains(text, "CREATE FUNCTION") || strings.Contains(text, "SELECT value") || strings.Contains(text, "postgres://") || strings.Contains(text, "password") || strings.Contains(text, `"definition"`) {
		t.Fatalf("default inventory disclosed source or credentials: %s", text)
	}
	withSource, err := PrepareBootstrapAuthorizationInventory(context.Background(), target, desired, BootstrapAuthorizationInventoryOptions{IncludeRoutineSource: true})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withSource.Routines[0].Definition, "CREATE FUNCTION") {
		t.Fatalf("explicit source request did not include definitions: %+v", withSource.Routines)
	}
	jsonBytes, err := withSource.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	var decoded BootstrapAuthorizationInventory
	if err := json.Unmarshal(jsonBytes, &decoded); err != nil {
		t.Fatal(err)
	}
	assertRoutineSourceBindings(t, decoded.Routines)
	hclBytes, err := withSource.MarshalHCL()
	if err != nil {
		t.Fatal(err)
	}
	file, diagnostics := hclparse.NewParser().ParseHCL(hclBytes, "bootstrap-authorization.hcl")
	if diagnostics.HasErrors() {
		t.Fatal(diagnostics.Error())
	}
	body := file.Body.(*hclsyntax.Body)
	var hclRoutines []BootstrapRoutineAuthorization
	for _, block := range body.Blocks[0].Body.Blocks {
		if block.Type != "routine_review" {
			continue
		}
		definition, definitionDiagnostics := block.Body.Attributes["definition"].Expr.Value(nil)
		digest, digestDiagnostics := block.Body.Attributes["source_digest"].Expr.Value(nil)
		if definitionDiagnostics.HasErrors() || digestDiagnostics.HasErrors() {
			t.Fatalf("decode HCL source: %s %s", definitionDiagnostics.Error(), digestDiagnostics.Error())
		}
		hclRoutines = append(hclRoutines, BootstrapRoutineAuthorization{Definition: definition.AsString(), SourceDigest: digest.AsString()})
	}
	assertRoutineSourceBindings(t, hclRoutines)
	if !strings.Contains(string(jsonBytes), "token=current_setting") || !strings.Contains(string(hclBytes), "token=current_setting") {
		t.Fatal("legitimate token=current_setting source was altered")
	}
}

func TestPrepareBootstrapAuthorizationInventoryIsNonExecutableReviewMaterial(t *testing.T) {
	desired, target := authorizationInventoryFixture(t)
	inventory, err := PrepareBootstrapAuthorizationInventory(context.Background(), target, desired, BootstrapAuthorizationInventoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	raw, err := inventory.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{`"schema_plan":`, `"steps":`, `"phases":`, `"sql":`, "CREATE FUNCTION"} {
		if strings.Contains(string(raw), forbidden) {
			t.Fatalf("prepare leaked executable plan material %q: %s", forbidden, raw)
		}
	}
	if inventory.PlanSummary.SchemaPlanDigest == "" || inventory.PlanSummary.StepCount == 0 || inventory.PlanSummary.PhaseCount == 0 {
		t.Fatalf("review summary is incomplete: %+v", inventory.PlanSummary)
	}
	// Security invariant: the only returned value is an inventory. Go's type
	// system cannot pass it to ExecuteDatabaseBootstrapURL, whose third
	// argument must be a bootstrap.Plan built through explicit authorization.
}

func TestBootstrapAuthorizationInventoryRejectsNoncanonicalOrMismatchedDigests(t *testing.T) {
	desired, target := authorizationInventoryFixture(t)
	inventory, err := PrepareBootstrapAuthorizationInventory(context.Background(), target, desired, BootstrapAuthorizationInventoryOptions{IncludeRoutineSource: true})
	if err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*BootstrapAuthorizationInventory){
		"short plan":            func(value *BootstrapAuthorizationInventory) { value.PlanDigest = "sha256:abc" },
		"short document source": func(value *BootstrapAuthorizationInventory) { value.SourceDigest = "sha256:abc" },
		"uppercase plan":        func(value *BootstrapAuthorizationInventory) { value.PlanDigest = "sha256:" + strings.Repeat("A", 64) },
		"short source":          func(value *BootstrapAuthorizationInventory) { value.Routines[0].SourceDigest = "sha256:abc" },
		"uppercase source": func(value *BootstrapAuthorizationInventory) {
			value.Routines[0].SourceDigest = "sha256:" + strings.Repeat("B", 64)
		},
		"source mismatch": func(value *BootstrapAuthorizationInventory) { value.Routines[0].Definition += " " },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := inventory
			candidate.Routines = append([]BootstrapRoutineAuthorization(nil), inventory.Routines...)
			mutate(&candidate)
			if err := candidate.Validate(); err == nil {
				t.Fatalf("accepted invalid inventory: %+v", candidate)
			}
		})
	}
}

func TestExtensionRequiresSuperuserTruthTable(t *testing.T) {
	for _, test := range []struct {
		superuser, trusted, want bool
	}{
		{superuser: false, trusted: false, want: false},
		{superuser: false, trusted: true, want: false},
		{superuser: true, trusted: false, want: true},
		{superuser: true, trusted: true, want: false},
	} {
		if got := extensionRequiresSuperuser(test.superuser, test.trusted); got != test.want {
			t.Fatalf("superuser=%v trusted=%v got=%v want=%v", test.superuser, test.trusted, got, test.want)
		}
	}
}

func assertRoutineSourceBindings(t *testing.T, routines []BootstrapRoutineAuthorization) {
	t.Helper()
	if len(routines) == 0 {
		t.Fatal("no emitted routine sources")
	}
	for _, routine := range routines {
		if routine.Definition == "" || routineDefinitionDigest(routine.Definition) != routine.SourceDigest {
			t.Fatalf("source digest binding failed: %+v", routine)
		}
	}
}

func explicitlyAuthorizedRender(inventory BootstrapAuthorizationInventory, base map[string]string) map[string]string {
	render := cloneRenderOptions(base)
	var digests, extensions []string
	for _, routine := range inventory.Routines {
		digests = append(digests, routine.SourceDigest)
		if routine.UnsafeLanguageAuthorizationRequired {
			render["allow_unsafe_routine_languages"] = "true"
		}
		if routine.PrivilegedRoutineAuthorizationRequired {
			render["allow_privileged_routines"] = "true"
		}
		if routine.TransactionControlAuthorizationRequired {
			render["allow_transaction_control_procedures"] = "true"
		}
	}
	for _, extension := range inventory.Extensions {
		extensions = append(extensions, extension.Name)
		render["extension_version."+extension.Name] = extension.Version
		render["extension_schemas."+extension.Name] = extension.Schema
		if extension.UntrustedExtensionAuthorizationRequired {
			render["allow_untrusted_extensions"] = "true"
		}
	}
	render["reviewed_routine_digests"] = strings.Join(uniqueNonEmptySorted(digests), ",")
	render["extension_allowlist"] = strings.Join(uniqueNonEmptySorted(extensions), ",")
	return render
}

func authorizationInventoryFixture(t *testing.T) (schema.Document, bootstrap.DatabaseTarget) {
	t.Helper()
	namespace := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	pgcrypto := renderResource(schema.KindExtension, schema.Name{Schema: "app", Name: "pgcrypto", Parent: namespace.ID}, `{"version":"1.3","relocatable":true,"trusted":true,"superuser":false,"requires":[]}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	hstore := renderResource(schema.KindExtension, schema.Name{Schema: "app", Name: "hstore", Parent: namespace.ID}, `{"version":"1.8","relocatable":true,"trusted":true,"superuser":false,"requires":["pgcrypto"]}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains}, schema.Dependency{Target: pgcrypto.ID, Type: schema.DependencyUses})
	functionDefinition := `CREATE FUNCTION app.lookup(value integer) RETURNS integer LANGUAGE sql IMMUTABLE AS $$ SELECT value + length('token=current_setting') $$`
	functionSpec, err := json.Marshal(map[string]any{"name": "lookup", "identity_arguments": "value integer", "arguments": "value integer", "result": "integer", "returns_set": false, "language": "sql", "volatility": "i", "strict": false, "security_definer": false, "leakproof": false, "parallel": "s", "cost": 1, "rows": 0, "configuration": []string{}, "definition": functionDefinition, "body_digest": routineDefinitionDigest(functionDefinition)})
	if err != nil {
		t.Fatal(err)
	}
	function := schema.Resource{Kind: schema.KindFunction, Name: schema.Name{Schema: "app", Name: "lookup(value integer)", Parent: namespace.ID}, Spec: functionSpec, Dependencies: []schema.Dependency{{Target: namespace.ID, Type: schema.DependencyContains}, {Target: pgcrypto.ID, Type: schema.DependencyUses}}}
	function.ID = schema.StableID(function.Kind, function.Name)
	procedureDefinition := `CREATE PROCEDURE app.refresh() LANGUAGE plpgsql AS $$ BEGIN NULL; END $$`
	procedureSpec, err := json.Marshal(map[string]any{"name": "refresh", "identity_arguments": "", "arguments": "", "result": "", "returns_set": false, "language": "plpgsql", "volatility": "v", "strict": false, "security_definer": false, "leakproof": false, "parallel": "u", "cost": 100, "rows": 0, "configuration": []string{}, "definition": procedureDefinition, "body_digest": routineDefinitionDigest(procedureDefinition)})
	if err != nil {
		t.Fatal(err)
	}
	procedure := schema.Resource{Kind: schema.KindProcedure, Name: schema.Name{Schema: "app", Name: "refresh()", Parent: namespace.ID}, Spec: procedureSpec, Dependencies: []schema.Dependency{{Target: namespace.ID, Type: schema.DependencyContains}}}
	procedure.ID = schema.StableID(procedure.Kind, procedure.Name)
	unsafeDefinition := `CREATE FUNCTION app.unsafe() RETURNS integer LANGUAGE plpython3u AS $$ return 1 $$`
	unsafeSpec, err := json.Marshal(map[string]any{"name": "unsafe", "identity_arguments": "", "arguments": "", "result": "integer", "returns_set": false, "language": "plpython3u", "volatility": "v", "strict": false, "security_definer": false, "leakproof": false, "parallel": "u", "cost": 100, "rows": 0, "configuration": []string{}, "definition": unsafeDefinition, "body_digest": routineDefinitionDigest(unsafeDefinition)})
	if err != nil {
		t.Fatal(err)
	}
	unsafe := schema.Resource{Kind: schema.KindFunction, Name: schema.Name{Schema: "app", Name: "unsafe()", Parent: namespace.ID}, Spec: unsafeSpec, Dependencies: []schema.Dependency{{Target: namespace.ID, Type: schema.DependencyContains}}}
	unsafe.ID = schema.StableID(unsafe.Kind, unsafe.Name)
	privilegedDefinition := `CREATE PROCEDURE app.privileged_refresh() LANGUAGE plpgsql AS $$ BEGIN PERFORM pg_read_file('/tmp/input'); COMMIT; END $$`
	privilegedSpec, err := json.Marshal(map[string]any{"name": "privileged_refresh", "identity_arguments": "", "arguments": "", "result": "", "returns_set": false, "language": "plpgsql", "volatility": "v", "strict": false, "security_definer": false, "leakproof": false, "parallel": "u", "cost": 100, "rows": 0, "configuration": []string{}, "definition": privilegedDefinition, "body_digest": routineDefinitionDigest(privilegedDefinition)})
	if err != nil {
		t.Fatal(err)
	}
	privileged := schema.Resource{Kind: schema.KindProcedure, Name: schema.Name{Schema: "app", Name: "privileged_refresh()", Parent: namespace.ID}, Spec: privilegedSpec, Dependencies: []schema.Dependency{{Target: namespace.ID, Type: schema.DependencyContains}}}
	privileged.ID = schema.StableID(privileged.Kind, privileged.Name)
	document := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{namespace, pgcrypto, hstore, function, procedure, unsafe, privileged}}}
	document.Normalize()
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: "db.internal", Port: 5432, TLSMode: "verify-full"}, MaintenanceDatabase: "postgres", Name: "cell", Owner: "postgres", Encoding: "UTF8", Template: "template0", Tablespace: "pg_default", ConnectionLimit: -1, AllowConnections: true}
	return document, target
}
