package postgres

import (
	"strings"
	"testing"

	"autosql/pkg/schema"
)

func membershipFixture(t *testing.T, modern bool) (map[string]schema.Resource, schema.Resource) {
	t.Helper()
	parent := roleFixture("cell_reader")
	member := roleFixture("cell_app")
	grantor := roleFixture("cell_owner")
	specification := `{"parent":"cell_reader","member":"cell_app","grantor":"cell_owner","admin":false}`
	if modern {
		specification = `{"parent":"cell_reader","member":"cell_app","grantor":"cell_owner","admin":false,"inherit":false,"set":true}`
	}
	membership := renderResource(schema.KindMembership, schema.Name{Name: "cell_app->cell_reader@cell_owner"}, specification,
		schema.Dependency{Target: parent.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: member.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: grantor.ID, Type: schema.DependencyReferences})
	return map[string]schema.Resource{parent.ID: parent, member.ID: member, grantor.ID: grantor, membership.ID: membership}, membership
}

func TestMembershipVersionedOptionsAndEscalationGuard(t *testing.T) {
	resources, membership := membershipFixture(t, true)
	if _, err := renderMembershipGrant(membership, resources, map[string]string{"postgres_version": "15"}); err == nil {
		t.Fatal("PostgreSQL 16 membership options accepted on PostgreSQL 15")
	}
	created, err := renderMembershipGrant(membership, resources, map[string]string{"postgres_version": "16"})
	if err != nil || len(created) != 1 || !strings.Contains(created[0], "WITH ADMIN false, INHERIT false, SET true") {
		t.Fatalf("grant=%v err=%v", created, err)
	}
	admin := membership
	admin.Spec = []byte(`{"parent":"cell_reader","member":"cell_app","grantor":"cell_owner","admin":true,"inherit":false,"set":true}`)
	if _, err = renderMembershipGrant(admin, resources, map[string]string{"postgres_version": "16"}); err == nil {
		t.Fatal("ADMIN escalation accepted without policy")
	}
	if _, err = renderMembershipGrant(admin, resources, map[string]string{"postgres_version": "16", "allow_membership_admin": "true"}); err != nil {
		t.Fatal(err)
	}
}

func TestMembershipCyclesFailBeforeRendering(t *testing.T) {
	a := roleFixture("cycle_a")
	b := roleFixture("cycle_b")
	ab := renderResource(schema.KindMembership, schema.Name{Name: "cycle_a->cycle_b@cycle_a"}, `{"parent":"cycle_b","member":"cycle_a","grantor":"cycle_a","admin":false}`, schema.Dependency{Target: a.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: b.ID, Type: schema.DependencyReferences})
	ba := renderResource(schema.KindMembership, schema.Name{Name: "cycle_b->cycle_a@cycle_b"}, `{"parent":"cycle_a","member":"cycle_b","grantor":"cycle_b","admin":false}`, schema.Dependency{Target: a.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: b.ID, Type: schema.DependencyReferences})
	resources := map[string]schema.Resource{a.ID: a, b.ID: b, ab.ID: ab, ba.ID: ba}
	if err := validateMembershipCycles(resources); err == nil {
		t.Fatal("membership cycle accepted")
	}
}
