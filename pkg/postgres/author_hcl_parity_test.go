package postgres

import (
	"context"
	"encoding/json"
	"testing"

	"autosql/pkg/schema"
	"autosql/pkg/source"
)

func TestAuthorHCLRepresentativeCellGlobalAuthParity(t *testing.T) {
	cell := bootstrapExecutionDocument(t, "cell")

	global := bootstrapExecutionDocument(t, "global")
	globalNamespace := resourceByKindName(t, global, schema.KindSchema, "global")
	cellType := renderResource(schema.KindEnum, schema.Name{Schema: "global", Name: "cell_type", Parent: globalNamespace.ID}, `{"values":["shared","dedicated","isolated"]}`, schema.Dependency{Target: globalNamespace.ID, Type: schema.DependencyContains})
	global.Graph.Resources = append(global.Graph.Resources, cellType)

	auth := bootstrapExecutionDocument(t, "auth")
	authTable := resourceByKindName(t, auth, schema.KindTable, "items")
	reader := renderResource(schema.KindRole, schema.Name{Name: "auth_reader"}, `{"login":false,"superuser":false,"create_role":false,"create_database":false,"inherit":true}`)
	owner := renderResource(schema.KindRole, schema.Name{Name: "auth_owner"}, `{"login":false,"superuser":false,"create_role":false,"create_database":false,"inherit":true}`)
	grant := renderResource(schema.KindGrant, schema.Name{Schema: "auth", Name: "auth_reader_select", Parent: authTable.ID}, `{"grantor":"auth_owner","grantee":"auth_reader","privilege":"SELECT","grantable":false}`, schema.Dependency{Target: authTable.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: reader.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: owner.ID, Type: schema.DependencyReferences})
	auth.Graph.Resources = append(auth.Graph.Resources, reader, owner, grant)

	for name, document := range map[string]schema.Document{"cell": cell, "global": global, "auth": auth} {
		t.Run(name, func(t *testing.T) {
			document.Normalize()
			if err := document.Validate(); err != nil {
				t.Fatal(err)
			}
			formatted, err := source.FormatAuthorHCL(document)
			if err != nil {
				t.Fatal(err)
			}
			back, err := source.ParseHCL(name+"-author.hcl", formatted, nil)
			if err != nil {
				t.Fatal(err)
			}
			want, _ := json.Marshal(document)
			got, _ := json.Marshal(back)
			if string(want) != string(got) {
				t.Fatalf("%s author HCL changed the canonical graph", name)
			}
			plan, err := New().Normalize(context.Background(), back)
			if err != nil {
				t.Fatal(err)
			}
			if changes, diffErr := schema.Diff(document, plan, schema.DiffOptions{}); diffErr != nil || len(changes.Changes) != 0 {
				t.Fatalf("%s normalized author parity changes=%+v err=%v", name, changes.Changes, diffErr)
			}
		})
	}
}

func resourceByKindName(t *testing.T, document schema.Document, kind schema.Kind, name string) schema.Resource {
	t.Helper()
	for _, resource := range document.Graph.Resources {
		if resource.Kind == kind && resource.Name.Name == name {
			return resource
		}
	}
	t.Fatalf("missing %s %s", kind, name)
	return schema.Resource{}
}
