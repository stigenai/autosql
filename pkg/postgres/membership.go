package postgres

import (
	"fmt"
	"sort"
	"strings"

	"autosql/pkg/schema"
)

func validateMembershipSpec(resource schema.Resource, resources map[string]schema.Resource, options map[string]string) error {
	s := spec(resource)
	if !allowedKeys(s, "parent", "member", "grantor", "admin", "inherit", "set") {
		return unsupported(resource, "unknown membership semantics")
	}
	parent, member := stringValue(s, "parent"), stringValue(s, "member")
	if parent == "" || member == "" || parent == member || protectedRole(parent) || protectedRole(member) {
		return unsupported(resource, "membership roles must be distinct declared non-system roles")
	}
	for _, key := range []string{"admin", "inherit", "set"} {
		if value, exists := s[key]; exists {
			if _, ok := value.(bool); !ok {
				return unsupported(resource, "membership "+key+" must be boolean")
			}
		}
	}
	if boolValue(s, "admin") && !enabled(options, "allow_membership_admin", false) {
		return unsupported(resource, "ADMIN OPTION requires allow_membership_admin=true")
	}
	if _, hasInherit := s["inherit"]; hasInherit {
		if err := requirePostgresMajor(resource, options, 16, "membership INHERIT option"); err != nil {
			return err
		}
	}
	if _, hasSet := s["set"]; hasSet {
		if err := requirePostgresMajor(resource, options, 16, "membership SET option"); err != nil {
			return err
		}
	}
	expected := []string{}
	for index, role := range []string{parent, member, stringValue(s, "grantor")} {
		if role == "" {
			continue
		}
		if index == 2 && protectedRole(role) {
			continue
		}
		id, err := roleOwnerDependency(role, resources)
		if err != nil {
			return unsupported(resource, "membership role is undeclared or ambiguous")
		}
		expected = append(expected, id)
	}
	expected = uniqueSorted(expected)
	actual := []string{}
	for _, dependency := range resource.Dependencies {
		if dependency.Type != schema.DependencyReferences {
			return unsupported(resource, "membership dependencies must reference roles")
		}
		actual = append(actual, dependency.Target)
	}
	actual = uniqueSorted(actual)
	if strings.Join(actual, "\x00") != strings.Join(expected, "\x00") {
		if options["__membership_role_rename"] == "true" && len(actual) == len(expected) {
			mapped := append([]string(nil), actual...)
			for index, target := range mapped {
				if replacement := options["__role_rename."+target]; replacement != "" {
					mapped[index] = replacement
				}
			}
			mapped = uniqueSorted(mapped)
			if strings.Join(mapped, "\x00") == strings.Join(expected, "\x00") {
				return nil
			}
		}
		return unsupported(resource, fmt.Sprintf("membership dependencies do not exactly match roles (actual=%v expected=%v)", actual, expected))
	}
	return nil
}

func renderMembershipGrant(resource schema.Resource, resources map[string]schema.Resource, options map[string]string) ([]string, error) {
	if err := validateMembershipSpec(resource, resources, options); err != nil {
		return nil, err
	}
	s := spec(resource)
	q := "GRANT " + quote(stringValue(s, "parent")) + " TO " + quote(stringValue(s, "member"))
	if _, modern := s["inherit"]; modern {
		q += fmt.Sprintf(" WITH ADMIN %t, INHERIT %t, SET %t", boolValue(s, "admin"), boolValue(s, "inherit"), boolValue(s, "set"))
	} else if boolValue(s, "admin") {
		q += " WITH ADMIN OPTION"
	}
	if grantor := stringValue(s, "grantor"); grantor != "" {
		q += " GRANTED BY " + quote(grantor)
	}
	return []string{q}, nil
}

func renderMembershipRevoke(resource schema.Resource, resources map[string]schema.Resource, options map[string]string) ([]string, error) {
	validation := cloneOptions(options)
	validation["allow_membership_admin"] = "true"
	if _, exists := spec(resource)["inherit"]; exists {
		validation["postgres_version"] = "16"
	}
	if err := validateMembershipSpec(resource, resources, validation); err != nil {
		return nil, err
	}
	s := spec(resource)
	q := "REVOKE " + quote(stringValue(s, "parent")) + " FROM " + quote(stringValue(s, "member"))
	if grantor := stringValue(s, "grantor"); grantor != "" {
		q += " GRANTED BY " + quote(grantor)
	}
	return []string{q}, nil
}

func validateMembershipCycles(resources map[string]schema.Resource) error {
	edges := map[string][]string{}
	resourceForEdge := map[string]schema.Resource{}
	for _, resource := range resources {
		if resource.Kind != schema.KindMembership {
			continue
		}
		member, parent := stringValue(spec(resource), "member"), stringValue(spec(resource), "parent")
		edges[member] = append(edges[member], parent)
		resourceForEdge[member+"\x00"+parent] = resource
	}
	for key := range edges {
		sort.Strings(edges[key])
	}
	visiting, visited := map[string]bool{}, map[string]bool{}
	var visit func(string) error
	visit = func(role string) error {
		if visiting[role] {
			return fmt.Errorf("membership graph contains a cycle through role %q", role)
		}
		if visited[role] {
			return nil
		}
		visiting[role] = true
		for _, parent := range edges[role] {
			if err := visit(parent); err != nil {
				return err
			}
		}
		visiting[role] = false
		visited[role] = true
		return nil
	}
	for role := range edges {
		if err := visit(role); err != nil {
			return err
		}
	}
	_ = resourceForEdge
	return nil
}

func membershipOptionsEqual(before, after schema.Resource) bool {
	bs, as := spec(before), spec(after)
	for _, key := range []string{"admin", "inherit", "set"} {
		if fmt.Sprint(bs[key]) != fmt.Sprint(as[key]) {
			return false
		}
	}
	return true
}
