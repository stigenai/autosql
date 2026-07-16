package postgres

import (
	"strings"
	"testing"

	"autosql/pkg/schema"
)

func TestGrantLifecycleAndExactDependencies(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "cell"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "cell", Name: "jobs", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	role := roleFixture("cell_app")
	grant := renderResource(schema.KindGrant, schema.Name{Schema: "cell", Name: "cell_app:select:postgres", Parent: table.ID}, `{"grantor":"postgres","grantee":"cell_app","privilege":"SELECT","grantable":false}`, schema.Dependency{Target: table.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: role.ID, Type: schema.DependencyReferences})
	resources := map[string]schema.Resource{ns.ID: ns, table.ID: table, role.ID: role, grant.ID: grant}
	created, err := renderGrantCreate(grant, resources)
	if err != nil || created[0] != `SET LOCAL ROLE "postgres"; GRANT SELECT ON TABLE "cell"."jobs" TO "cell_app" GRANTED BY "postgres"; RESET ROLE` {
		t.Fatalf("create=%v err=%v", created, err)
	}
	grantable := grant
	grantable.Spec = []byte(`{"grantor":"postgres","grantee":"cell_app","privilege":"SELECT","grantable":true}`)
	altered, err := renderGrantAlter(grant, grantable, resources)
	if err != nil || !strings.Contains(altered[0], "WITH GRANT OPTION") {
		t.Fatalf("alter=%v err=%v", altered, err)
	}
	reduced, err := renderGrantAlter(grantable, grant, resources)
	if err != nil || !strings.Contains(reduced[0], "REVOKE GRANT OPTION FOR SELECT") {
		t.Fatalf("reduce=%v err=%v", reduced, err)
	}
	dropped, err := renderGrantDrop(grant, resources)
	if err != nil || !strings.Contains(dropped[0], "REVOKE SELECT") {
		t.Fatalf("drop=%v err=%v", dropped, err)
	}
	bad := grant
	bad.Dependencies = bad.Dependencies[:1]
	if _, err := renderGrantCreate(bad, resources); err == nil {
		t.Fatal("missing grantee dependency accepted")
	}
}

func TestGrantTargetClassesAreExplicit(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "cell"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "cell", Name: "jobs", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	column := renderResource(schema.KindColumn, schema.Name{Schema: "cell", Name: "payload", Parent: table.ID}, `{"type":"text","not_null":false,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	sequence := renderResource(schema.KindSequence, schema.Name{Schema: "cell", Name: "jobs_id_seq", Parent: ns.ID}, `{"start":1,"increment":1,"min":1,"max":9223372036854775807,"cache":1,"cycle":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	enum := renderResource(schema.KindEnum, schema.Name{Schema: "cell", Name: "status", Parent: ns.ID}, `{"values":["new"]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	routine := renderResource(schema.KindFunction, schema.Name{Schema: "cell", Name: "lookup(bigint)", Parent: ns.ID}, `{"name":"lookup","identity_arguments":"bigint"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	database := renderResource(schema.KindDatabase, schema.Name{Name: "cell"}, `{}`)
	resources := map[string]schema.Resource{ns.ID: ns, table.ID: table, column.ID: column, sequence.ID: sequence, enum.ID: enum, routine.ID: routine, database.ID: database}
	for _, fixture := range []struct {
		resource schema.Resource
		prefix   string
	}{{database, "DATABASE"}, {ns, "SCHEMA"}, {table, "TABLE"}, {column, "TABLE"}, {sequence, "SEQUENCE"}, {enum, "TYPE"}, {routine, "FUNCTION"}} {
		if target, _, err := grantTargetSQL(fixture.resource, resources); err != nil || !strings.HasPrefix(target, fixture.prefix+" ") {
			t.Errorf("%s target=%q err=%v", fixture.resource.Kind, target, err)
		}
	}
}
