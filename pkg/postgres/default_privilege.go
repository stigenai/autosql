package postgres

import (
	"fmt"
	"strings"

	"autosql/pkg/schema"
)

type parsedDefaultPrivilege struct {
	Owner, Namespace, ObjectClass, Grantee, Privilege string
	Grantable                                         bool
}

func parseDefaultPrivilege(resource schema.Resource, resources map[string]schema.Resource) (parsedDefaultPrivilege, error) {
	s := spec(resource)
	if !allowedKeys(s, "owner", "object_type", "schema", "grantee", "privilege", "grantable") {
		return parsedDefaultPrivilege{}, unsupported(resource, "unknown default privilege semantics")
	}
	grantable, ok := s["grantable"].(bool)
	if !ok {
		return parsedDefaultPrivilege{}, unsupported(resource, "default privilege grantable must be boolean")
	}
	owner, namespace, grantee := stringValue(s, "owner"), stringValue(s, "schema"), stringValue(s, "grantee")
	if owner == "" || grantee == "" {
		return parsedDefaultPrivilege{}, unsupported(resource, "default privilege owner and grantee are required")
	}
	objectClass, privileges, err := defaultPrivilegeClass(stringValue(s, "object_type"))
	if err != nil {
		return parsedDefaultPrivilege{}, unsupported(resource, err.Error())
	}
	privilege := strings.ToUpper(strings.TrimSpace(stringValue(s, "privilege")))
	if !privileges[privilege] {
		return parsedDefaultPrivilege{}, unsupported(resource, "invalid privilege for default object class")
	}
	expected := []string{}
	for index, role := range []string{owner, grantee} {
		if index == 1 && strings.EqualFold(role, "PUBLIC") {
			continue
		}
		if protectedRole(role) {
			continue
		}
		id, e := roleOwnerDependency(role, resources)
		if e != nil {
			return parsedDefaultPrivilege{}, unsupported(resource, "default privilege role is undeclared or ambiguous")
		}
		expected = append(expected, id)
	}
	if namespace != "" {
		found := ""
		for id, candidate := range resources {
			if candidate.Kind == schema.KindSchema && candidate.Name.Name == namespace {
				if found != "" {
					return parsedDefaultPrivilege{}, unsupported(resource, "default privilege schema is ambiguous")
				}
				found = id
			}
		}
		if found == "" {
			return parsedDefaultPrivilege{}, unsupported(resource, "default privilege schema is undeclared")
		}
		expected = append(expected, found)
	}
	expected = uniqueSorted(expected)
	actual := []string{}
	for _, dependency := range resource.Dependencies {
		if dependency.Type != schema.DependencyReferences {
			return parsedDefaultPrivilege{}, unsupported(resource, "default privilege dependencies must be references")
		}
		actual = append(actual, dependency.Target)
	}
	actual = uniqueSorted(actual)
	if strings.Join(actual, "\x00") != strings.Join(expected, "\x00") {
		return parsedDefaultPrivilege{}, unsupported(resource, fmt.Sprintf("default privilege dependencies mismatch (actual=%v expected=%v)", actual, expected))
	}
	return parsedDefaultPrivilege{Owner: owner, Namespace: namespace, ObjectClass: objectClass, Grantee: grantee, Privilege: privilege, Grantable: grantable}, nil
}

func defaultPrivilegeClass(value string) (string, map[string]bool, error) {
	allow := func(values ...string) map[string]bool {
		out := map[string]bool{}
		for _, value := range values {
			out[value] = true
		}
		return out
	}
	switch value {
	case "r":
		return "TABLES", allow("SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", "MAINTAIN"), nil
	case "S":
		return "SEQUENCES", allow("USAGE", "SELECT", "UPDATE"), nil
	case "f":
		return "FUNCTIONS", allow("EXECUTE"), nil
	case "T":
		return "TYPES", allow("USAGE"), nil
	case "n":
		return "SCHEMAS", allow("USAGE", "CREATE"), nil
	default:
		return "", nil, fmt.Errorf("unsupported default privilege object class")
	}
}

func renderDefaultPrivilegeCreate(resource schema.Resource, resources map[string]schema.Resource) ([]string, error) {
	p, err := parseDefaultPrivilege(resource, resources)
	if err != nil {
		return nil, err
	}
	q := defaultPrivilegePrefix(p) + " GRANT " + p.Privilege + " ON " + p.ObjectClass + " TO " + grantRoleSQL(p.Grantee)
	if p.Grantable {
		q += " WITH GRANT OPTION"
	}
	return []string{q}, nil
}
func renderDefaultPrivilegeDrop(resource schema.Resource, resources map[string]schema.Resource) ([]string, error) {
	p, err := parseDefaultPrivilege(resource, resources)
	if err != nil {
		return nil, err
	}
	return []string{defaultPrivilegePrefix(p) + " REVOKE " + p.Privilege + " ON " + p.ObjectClass + " FROM " + grantRoleSQL(p.Grantee)}, nil
}
func renderDefaultPrivilegeAlter(before, after schema.Resource, resources map[string]schema.Resource) ([]string, error) {
	b, err := parseDefaultPrivilege(before, resources)
	if err != nil {
		return nil, err
	}
	a, err := parseDefaultPrivilege(after, resources)
	if err != nil {
		return nil, err
	}
	if b.Owner != a.Owner || b.Namespace != a.Namespace || b.ObjectClass != a.ObjectClass || b.Grantee != a.Grantee || b.Privilege != a.Privilege {
		return nil, unsupported(after, "default privilege identity change requires revoke and grant")
	}
	if b.Grantable == a.Grantable {
		return nil, unsupported(after, "default privilege alteration has no semantic change")
	}
	if a.Grantable {
		return renderDefaultPrivilegeCreate(after, resources)
	}
	return []string{defaultPrivilegePrefix(a) + " REVOKE GRANT OPTION FOR " + a.Privilege + " ON " + a.ObjectClass + " FROM " + grantRoleSQL(a.Grantee)}, nil
}
func defaultPrivilegePrefix(value parsedDefaultPrivilege) string {
	q := "ALTER DEFAULT PRIVILEGES FOR ROLE " + quote(value.Owner)
	if value.Namespace != "" {
		q += " IN SCHEMA " + quote(value.Namespace)
	}
	return q
}
