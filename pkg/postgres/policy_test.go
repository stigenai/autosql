package postgres

import (
	"context"
	"strings"
	"testing"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
)

func policyFixture(t *testing.T) (schema.Document, schema.Resource) {
	t.Helper()
	ns := renderResource(schema.KindSchema, schema.Name{Name: "cell"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "cell", Name: "jobs", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":true,"force_row_security":true}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	tenant := renderResource(schema.KindColumn, schema.Name{Schema: "cell", Name: "tenant_id", Parent: table.ID}, `{"type":"uuid","not_null":true,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	owner := renderResource(schema.KindRole, schema.Name{Name: "cell_app"}, `{"login":true}`)
	policy := renderResource(schema.KindPolicy, schema.Name{Schema: "cell", Name: "tenant_select", Parent: table.ID}, `{"command":"r","permissive":true,"roles":["cell_app"],"using":"tenant_id::text = current_setting('app.tenant_id')"}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: tenant.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: owner.ID, Type: schema.DependencyReferences})
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, tenant, owner, policy}}, Annotations: map[string]string{"dialect": "postgresql"}}
	return doc, policy
}

func TestPolicyLifecycleAndExactDependencies(t *testing.T) {
	doc, policy := policyFixture(t)
	resources := resourceMapForRender(doc)
	parsed, err := parsePolicy(policy, resources)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(parsed.SQL, `CREATE POLICY "tenant_select" ON "cell"."jobs" AS PERMISSIVE FOR SELECT TO "cell_app" USING`) {
		t.Fatalf("sql=%s", parsed.SQL)
	}
	if len(parsed.Dependencies) != 3 {
		t.Fatalf("dependencies=%v", parsed.Dependencies)
	}
	created, err := renderCreate(policy, resources, nil)
	if err != nil || len(created) != 1 {
		t.Fatalf("create=%v err=%v", created, err)
	}
	dropped, err := renderDrop(policy, resources, nil)
	if err != nil || dropped[0] != `DROP POLICY "tenant_select" ON "cell"."jobs"` {
		t.Fatalf("drop=%v err=%v", dropped, err)
	}
	renamed := policy
	renamed.Name.Name = "tenant_read"
	renamed.ID = schema.StableID(renamed.Kind, renamed.Name)
	renames, err := renderRename(policy, renamed, resources)
	if err != nil || renames[0] != `ALTER POLICY "tenant_select" ON "cell"."jobs" RENAME TO "tenant_read"` {
		t.Fatalf("rename=%v err=%v", renames, err)
	}
	altered := policy
	altered.Spec = []byte(`{"command":"r","permissive":false,"roles":["cell_app"],"using":"tenant_id::text = current_setting('app.other_tenant')"}`)
	replacement, err := renderAlter(policy, altered, resources, nil)
	if err != nil || len(replacement) != 2 || !strings.HasPrefix(replacement[0], "DROP POLICY") || !strings.HasPrefix(replacement[1], "CREATE POLICY") {
		t.Fatalf("alter=%v err=%v", replacement, err)
	}
}

func TestPolicyFailsClosedOnIncompleteOrUnprovableAccess(t *testing.T) {
	doc, policy := policyFixture(t)
	resources := resourceMapForRender(doc)
	cases := map[string]string{
		"missing check":   `{"command":"w","permissive":true,"roles":["cell_app"],"using":"tenant_id IS NOT NULL"}`,
		"unknown column":  `{"command":"r","permissive":true,"roles":["cell_app"],"using":"other_tenant IS NOT NULL"}`,
		"subquery":        `{"command":"r","permissive":true,"roles":["cell_app"],"using":"tenant_id IN (SELECT tenant_id FROM cell.jobs)"}`,
		"undeclared role": `{"command":"r","permissive":true,"roles":["missing"],"using":"tenant_id IS NOT NULL"}`,
		"unsafe syntax":   `{"command":"r","permissive":true,"roles":["cell_app"],"using":"true); DROP TABLE cell.jobs; --"}`,
	}
	for name, specification := range cases {
		t.Run(name, func(t *testing.T) {
			candidate := policy
			candidate.Spec = []byte(specification)
			if _, err := parsePolicy(candidate, resources); err == nil {
				t.Fatal("unsafe policy accepted")
			}
		})
	}
}

func TestRenderPolicyDocumentKeepsRLSDenyFirst(t *testing.T) {
	doc, _ := policyFixture(t)
	// Roles are provisioned in a separate authority phase until the role story
	// lands; keep the role as dependency context but outside this render scope.
	doc.Graph.Resources = append(doc.Graph.Resources[:3], doc.Graph.Resources[4])
	doc.Graph.Resources[3].Spec = []byte(`{"command":"r","permissive":true,"roles":["public"],"using":"tenant_id::text = current_setting('app.tenant_id')"}`)
	doc.Graph.Resources[3].Dependencies = doc.Graph.Resources[3].Dependencies[:2]
	statements, err := RenderDocument(context.Background(), doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, statement := range statements {
		if statement.Kind == plugin.StatementExecutable {
			joined += statement.SQL + "\n"
		}
	}
	enable := strings.Index(joined, "ENABLE ROW LEVEL SECURITY")
	policy := strings.Index(joined, "CREATE POLICY")
	if enable < 0 || policy < 0 || enable > policy {
		t.Fatalf("RLS was not enabled deny-first:\n%s", joined)
	}
}
