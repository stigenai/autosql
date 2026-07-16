package postgres

import (
	"context"
	"strings"
	"testing"

	"autosql/pkg/schema"
	"autosql/pkg/secret"
)

func roleFixture(name string) schema.Resource {
	return renderResource(schema.KindRole, schema.Name{Name: name}, `{"superuser":false,"inherit":true,"create_role":false,"create_database":false,"login":true,"replication":false,"bypass_rls":false,"connection_limit":20,"valid_until":"infinity","configuration":["search_path=cell, public","statement_timeout=5s"]}`)
}

func TestRoleLifecycleRequiresExplicitEscalationAndSafeRetirement(t *testing.T) {
	role := roleFixture("cell_app")
	created, err := renderRoleCreate(role, nil)
	if err != nil || len(created) != 3 || !strings.Contains(created[0], `CREATE ROLE "cell_app" WITH NOSUPERUSER INHERIT`) {
		t.Fatalf("create=%v err=%v", created, err)
	}
	escalated := role
	escalated.Spec = []byte(`{"superuser":false,"inherit":true,"create_role":true,"create_database":false,"login":true,"replication":false,"bypass_rls":false,"connection_limit":20,"valid_until":"infinity","configuration":["search_path=cell, public"]}`)
	if _, err = renderRoleAlter(role, escalated, nil); err == nil {
		t.Fatal("role escalation accepted without policy")
	}
	altered, err := renderRoleAlter(role, escalated, map[string]string{"allow_role_escalation": "true"})
	if err != nil || len(altered) != 2 {
		t.Fatalf("alter=%v err=%v", altered, err)
	}
	owner := roleFixture("cell_owner")
	resources := map[string]schema.Resource{role.ID: role, owner.ID: owner}
	if _, err = renderRoleDrop(role, resources, nil); err == nil {
		t.Fatal("role drop accepted without retirement policy")
	}
	dropped, err := renderRoleDrop(role, resources, map[string]string{"allow_role_drop": "true", "reassign_owned_to.cell_app": "cell_owner"})
	if err != nil || len(dropped) != 3 || !strings.HasPrefix(dropped[0], "REASSIGN OWNED") {
		t.Fatalf("drop=%v err=%v", dropped, err)
	}
}

func TestRoleRejectsProtectedRolesAndPasswordMaterial(t *testing.T) {
	for _, name := range []string{"postgres", "pg_monitor", "public"} {
		if _, err := renderRoleCreate(roleFixture(name), map[string]string{"allow_role_escalation": "true"}); err == nil {
			t.Fatalf("protected role %s accepted", name)
		}
	}
	role := roleFixture("cell_app")
	role.Spec = []byte(`{"superuser":false,"inherit":true,"create_role":false,"create_database":false,"login":true,"replication":false,"bypass_rls":false,"connection_limit":20,"password":"hidden"}`)
	if _, err := renderRoleCreate(role, nil); err == nil {
		t.Fatal("inline password accepted")
	}
	ref, err := secret.Parse("env://CELL_APP_PASSWORD")
	if err != nil {
		t.Fatal(err)
	}
	resolver := secret.NewResolver()
	resolver.Getenv = func(name string) (string, bool) { return "write-only-value", name == "CELL_APP_PASSWORD" }
	if err := ApplyRolePasswordChange(context.Background(), nil, resolver, RolePasswordChange{Role: "cell_app", Reference: ref}); err == nil || strings.Contains(err.Error(), "write-only-value") {
		t.Fatalf("runtime password boundary err=%v", err)
	}
}
