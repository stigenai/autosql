package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"autosql/pkg/bootstrap"
	"autosql/pkg/schema"
)

func TestPreflightProvisioningAggregatesCompleteCellBlockers(t *testing.T) {
	resources := make([]schema.Resource, 0, 260)
	for i := 0; i < 248; i++ {
		resource := renderResource(schema.KindSchema, schema.Name{Name: fmt.Sprintf("commented_%03d", i)}, `{}`)
		resource.Annotations = map[string]string{"comment": "comment"}
		resources = append(resources, resource)
	}
	ns := renderResource(schema.KindSchema, schema.Name{Name: "cell"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "cell", Name: "jobs", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	identity := renderResource(schema.KindColumn, schema.Name{Schema: "cell", Name: "id", Parent: table.ID}, `{"type":"bigint","not_null":true,"ordinal":1,"identity":"a"}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	state := renderResource(schema.KindColumn, schema.Name{Schema: "cell", Name: "state", Parent: table.ID}, `{"type":"text","not_null":false,"ordinal":2}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	defaultNow := renderResource(schema.KindColumn, schema.Name{Schema: "cell", Name: "created_at", Parent: table.ID}, `{"type":"timestamptz","not_null":true,"ordinal":4,"default":"now()"}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	unknown := renderResource(schema.KindColumn, schema.Name{Schema: "cell", Name: "payload", Parent: table.ID}, `{"type":"jsonb","not_null":false,"ordinal":5,"default":"'{}'::jsonb","future_semantic":"password=hidden"}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	unknown.Annotations = map[string]string{"future_annotation": "password=hidden"}
	function := renderResource(schema.KindFunction, schema.Name{Schema: "cell", Name: "lifecycle_state_to_v2(text)", Parent: ns.ID}, `{"name":"lifecycle_state_to_v2","identity_arguments":"text","result":"text","language":"sql","volatility":"i","security_definer":false,"leakproof":false,"parallel":"s","definition":"select $1"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	generated := renderResource(schema.KindColumn, schema.Name{Schema: "cell", Name: "state_v2", Parent: table.ID}, `{"type":"text","not_null":false,"ordinal":3,"generated":"s","default":"cell.lifecycle_state_to_v2(state)"}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: state.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: function.ID, Type: schema.DependencyReferences})
	resources = append(resources, ns, table, identity, state, generated, defaultNow, unknown, function)
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: resources}, Annotations: map[string]string{"dialect": "postgresql"}}
	if err := doc.Validate(); err != nil {
		t.Fatal(err)
	}
	commentCount := 0
	for _, resource := range doc.Graph.Resources {
		if resource.Annotations["comment"] != "" {
			commentCount++
		}
	}
	if commentCount != 248 {
		t.Fatalf("fixture comments=%d", commentCount)
	}

	first, err := PreflightProvisioning(context.Background(), doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PreflightProvisioning(context.Background(), doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := first.MarshalCanonical()
	b, _ := second.MarshalCanonical()
	if string(a) != string(b) {
		t.Fatalf("preflight is unstable:\n%s\n%s", a, b)
	}
	if first.Supported || len(first.Diagnostics) < 3 {
		t.Fatalf("report=%s", a)
	}
	want := map[string]bool{
		"unsupported_spec_key:future_semantic":     false,
		"unsupported_annotation:future_annotation": false,
		"renderability:":                           false,
	}
	for _, diagnostic := range first.Diagnostics {
		key := diagnostic.Class + ":" + diagnostic.Field
		if _, exists := want[key]; exists {
			want[key] = true
		}
		if diagnostic.ResourceID == "" || diagnostic.Name == "" || diagnostic.Kind == "" || diagnostic.Message == "" {
			t.Fatalf("incomplete diagnostic: %+v", diagnostic)
		}
		if diagnostic.Class == "unsupported_semantic" && diagnostic.Field == "generated" {
			t.Fatalf("safe stored-generated column remained a provisioning blocker: %+v", diagnostic)
		}
	}
	for key, found := range want {
		if !found {
			t.Errorf("missing %s in %s", key, a)
		}
	}
	if strings.Contains(string(a), "password=hidden") {
		t.Fatalf("diagnostics leaked a literal: %s", a)
	}
	var decoded ProvisioningReport
	if err := json.Unmarshal(a, &decoded); err != nil || len(decoded.Diagnostics) != len(first.Diagnostics) {
		t.Fatalf("canonical report did not round-trip: %v", err)
	}
}

func TestPreflightBootstrapProvisioningBindsEveryAuthorityBeforeSQL(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "cell"}, `{}`)
	role := renderResource(schema.KindRole, schema.Name{Name: "cell_app"}, `{"login":true}`)
	grant := renderResource(schema.KindGrant, schema.Name{Name: "cell_app.table.cell.jobs.SELECT"}, `{"grantee":"cell_app","object_type":"table","object_identity":"cell.jobs","privilege":"SELECT","grantable":false}`)
	extension := renderResource(schema.KindExtension, schema.Name{Name: "pgcrypto"}, `{"version":"1.3","schema":"public"}`)
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, role, grant, extension}}, Annotations: map[string]string{"dialect": "postgresql"}}
	contract := bootstrap.Contract{
		Identities: []bootstrap.Identity{
			{Name: "cluster", Subject: "bootstrap_admin", Authentication: bootstrap.CurrentSession, Capabilities: []bootstrap.Capability{bootstrap.CreateDatabase, bootstrap.ManageRoles, bootstrap.ManageExtensions, bootstrap.ManageGrants}},
			{Name: "owner", Subject: "cell_owner", Authentication: bootstrap.OIDC, Capabilities: []bootstrap.Capability{bootstrap.ManageSchema, bootstrap.TransferOwnership}},
		},
		Assignments: []bootstrap.Assignment{
			{Responsibility: bootstrap.DatabaseCreation, Identity: "cluster"},
			{Responsibility: bootstrap.RoleCreation, Identity: "cluster"},
			{Responsibility: bootstrap.ExtensionSetup, Identity: "cluster"},
			{Responsibility: bootstrap.SchemaObjects, Identity: "owner"},
			{Responsibility: bootstrap.GrantSetup, Identity: "cluster"},
			{Responsibility: bootstrap.OwnershipHandoff, Identity: "owner"},
		},
	}
	report, err := PreflightBootstrapProvisioning(context.Background(), doc, nil, contract, true)
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Authority) != 6 || len(report.AuthorityDiagnostics) != 0 {
		t.Fatalf("authority=%+v diagnostics=%v", report.Authority, report.AuthorityDiagnostics)
	}
	// These kinds remain read-only until their lifecycle stories land, so the
	// complete report is not yet supported even though authority is complete.
	if report.Supported {
		t.Fatal("read-only security resources unexpectedly passed renderability")
	}

	contract.Assignments = contract.Assignments[:4]
	report, err = PreflightBootstrapProvisioning(context.Background(), doc, nil, contract, true)
	if err != nil || report.Supported || len(report.AuthorityDiagnostics) != 2 {
		t.Fatalf("report=%+v err=%v", report, err)
	}
}

func TestValidateBootstrapAuthorityDoesNotSynthesizeRoutineOrExtensionApproval(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "cell"}, `{}`)
	extension := renderResource(schema.KindExtension, schema.Name{Schema: "cell", Name: "pgcrypto", Parent: ns.ID}, `{"version":"1.3","schema":"cell"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, extension}}}
	contract := bootstrap.Contract{Identities: []bootstrap.Identity{{Name: "operator", Subject: "postgres", Authentication: bootstrap.CurrentSession, Capabilities: []bootstrap.Capability{bootstrap.CreateDatabase, bootstrap.ManageExtensions, bootstrap.ManageSchema, bootstrap.TransferOwnership}}}, Assignments: []bootstrap.Assignment{{Responsibility: bootstrap.DatabaseCreation, Identity: "operator"}, {Responsibility: bootstrap.ExtensionSetup, Identity: "operator"}, {Responsibility: bootstrap.SchemaObjects, Identity: "operator"}, {Responsibility: bootstrap.OwnershipHandoff, Identity: "operator"}}}
	bindings, err := ValidateBootstrapAuthority(doc, contract, true)
	if err != nil || len(bindings) != 4 {
		t.Fatalf("bindings=%+v err=%v", bindings, err)
	}
	if report, err := PreflightBootstrapProvisioning(context.Background(), doc, nil, contract, true); err != nil || report.Supported {
		t.Fatalf("authority-only validation synthesized extension approval: report=%+v err=%v", report, err)
	}
}
