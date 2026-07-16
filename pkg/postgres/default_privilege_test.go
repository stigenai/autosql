package postgres

import (
	"strings"
	"testing"

	"autosql/pkg/schema"
)

func TestDefaultPrivilegeLifecycleAndObjectClasses(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "cell"}, `{}`)
	role := roleFixture("cell_app")
	resource := renderResource(schema.KindDefaultPrivilege, schema.Name{Name: "postgres:cell:r:cell_app:select"}, `{"owner":"postgres","object_type":"r","schema":"cell","grantee":"cell_app","privilege":"SELECT","grantable":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: role.ID, Type: schema.DependencyReferences})
	resources := map[string]schema.Resource{ns.ID: ns, role.ID: role, resource.ID: resource}
	created, err := renderDefaultPrivilegeCreate(resource, resources)
	if err != nil || created[0] != `ALTER DEFAULT PRIVILEGES FOR ROLE "postgres" IN SCHEMA "cell" GRANT SELECT ON TABLES TO "cell_app"` {
		t.Fatalf("create=%v err=%v", created, err)
	}
	grantable := resource
	grantable.Spec = []byte(`{"owner":"postgres","object_type":"r","schema":"cell","grantee":"cell_app","privilege":"SELECT","grantable":true}`)
	altered, err := renderDefaultPrivilegeAlter(resource, grantable, resources)
	if err != nil || !strings.Contains(altered[0], "WITH GRANT OPTION") {
		t.Fatalf("alter=%v err=%v", altered, err)
	}
	reduced, err := renderDefaultPrivilegeAlter(grantable, resource, resources)
	if err != nil || !strings.Contains(reduced[0], "REVOKE GRANT OPTION FOR") {
		t.Fatalf("reduce=%v err=%v", reduced, err)
	}
	dropped, err := renderDefaultPrivilegeDrop(resource, resources)
	if err != nil || !strings.Contains(dropped[0], "REVOKE SELECT ON TABLES") {
		t.Fatalf("drop=%v err=%v", dropped, err)
	}
	for _, class := range []string{"r", "S", "f", "T", "n"} {
		if _, _, err := defaultPrivilegeClass(class); err != nil {
			t.Errorf("class %s: %v", class, err)
		}
	}
}
