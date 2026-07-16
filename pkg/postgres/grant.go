package postgres

import (
	"fmt"
	"sort"
	"strings"

	"autosql/pkg/schema"
)

type parsedGrant struct {
	Target    schema.Resource
	ObjectSQL string
	Privilege string
	Grantee   string
	Grantor   string
	Grantable bool
}

func parseGrant(resource schema.Resource, resources map[string]schema.Resource) (parsedGrant, error) {
	s := spec(resource)
	if !allowedKeys(s, "grantor", "grantee", "privilege", "grantable") {
		return parsedGrant{}, unsupported(resource, "unknown grant semantics")
	}
	grantable, ok := s["grantable"].(bool)
	if !ok {
		return parsedGrant{}, unsupported(resource, "grantable must be boolean")
	}
	grantee, grantor := stringValue(s, "grantee"), stringValue(s, "grantor")
	if grantee == "" || grantor == "" {
		return parsedGrant{}, unsupported(resource, "grant requires grantee and grantor")
	}
	var target schema.Resource
	for _, dependency := range resource.Dependencies {
		candidate := resources[dependency.Target]
		if dependency.Type == schema.DependencyReferences && candidate.Kind != schema.KindRole {
			if target.ID != "" {
				return parsedGrant{}, unsupported(resource, "grant must reference exactly one target")
			}
			target = candidate
		}
	}
	if target.ID == "" {
		return parsedGrant{}, unsupported(resource, "grant target is missing")
	}
	objectSQL, privileges, err := grantTargetSQL(target, resources)
	if err != nil {
		return parsedGrant{}, unsupported(resource, err.Error())
	}
	privilege := strings.ToUpper(strings.TrimSpace(stringValue(s, "privilege")))
	if !privileges[privilege] {
		return parsedGrant{}, unsupported(resource, "privilege is invalid for target object class")
	}

	expected := []string{target.ID}
	for index, role := range []string{grantee, grantor} {
		if index == 0 && strings.EqualFold(role, "PUBLIC") {
			continue
		}
		if protectedRole(role) {
			continue
		}
		id, roleErr := roleOwnerDependency(role, resources)
		if roleErr != nil {
			return parsedGrant{}, unsupported(resource, "grant role is undeclared or ambiguous")
		}
		expected = append(expected, id)
	}
	expected = uniqueSorted(expected)
	actual := []string{}
	for _, dependency := range resource.Dependencies {
		if dependency.Type != schema.DependencyReferences {
			return parsedGrant{}, unsupported(resource, "grant dependencies must be references")
		}
		actual = append(actual, dependency.Target)
	}
	actual = uniqueSorted(actual)
	if strings.Join(actual, "\x00") != strings.Join(expected, "\x00") {
		return parsedGrant{}, unsupported(resource, fmt.Sprintf("grant dependencies do not exactly match target and roles (actual=%v expected=%v)", actual, expected))
	}
	return parsedGrant{Target: target, ObjectSQL: objectSQL, Privilege: privilege, Grantee: grantee, Grantor: grantor, Grantable: grantable}, nil
}

func grantTargetSQL(target schema.Resource, resources map[string]schema.Resource) (string, map[string]bool, error) {
	privileges := func(values ...string) map[string]bool {
		out := map[string]bool{}
		for _, value := range values {
			out[value] = true
		}
		return out
	}
	switch target.Kind {
	case schema.KindDatabase:
		return "DATABASE " + quote(target.Name.Name), privileges("CREATE", "CONNECT", "TEMPORARY"), nil
	case schema.KindSchema:
		return "SCHEMA " + quote(target.Name.Name), privileges("CREATE", "USAGE"), nil
	case schema.KindTable, schema.KindView, schema.KindMaterializedView:
		return "TABLE " + qualified(target.Name), privileges("SELECT", "INSERT", "UPDATE", "DELETE", "TRUNCATE", "REFERENCES", "TRIGGER", "MAINTAIN"), nil
	case schema.KindColumn:
		parent := resources[target.Name.Parent]
		if parent.Kind != schema.KindTable && parent.Kind != schema.KindView && parent.Kind != schema.KindMaterializedView {
			return "", nil, fmt.Errorf("column grant parent is invalid")
		}
		return "TABLE " + qualified(parent.Name), privileges("SELECT", "INSERT", "UPDATE", "REFERENCES"), nil
	case schema.KindSequence:
		return "SEQUENCE " + qualified(target.Name), privileges("USAGE", "SELECT", "UPDATE"), nil
	case schema.KindFunction, schema.KindProcedure:
		signature, err := routineSignature(target)
		if err != nil {
			return "", nil, err
		}
		return routineKeyword(target) + " " + signature, privileges("EXECUTE"), nil
	case schema.KindEnum, schema.KindDomain, schema.KindComposite:
		return "TYPE " + qualified(target.Name), privileges("USAGE"), nil
	default:
		return "", nil, fmt.Errorf("grant target object class is unsupported")
	}
}

func renderGrantCreate(resource schema.Resource, resources map[string]schema.Resource) ([]string, error) {
	grant, err := parseGrant(resource, resources)
	if err != nil {
		return nil, err
	}
	q := "GRANT " + grantPrivilegeSQL(grant) + " ON " + grant.ObjectSQL + " TO " + grantRoleSQL(grant.Grantee)
	if grant.Grantable {
		q += " WITH GRANT OPTION"
	}
	if grant.Grantor != "" {
		q += " GRANTED BY " + quote(grant.Grantor)
	}
	return []string{grantAsExactRole(q, grant.Grantor)}, nil
}

func renderGrantDrop(resource schema.Resource, resources map[string]schema.Resource) ([]string, error) {
	grant, err := parseGrant(resource, resources)
	if err != nil {
		return nil, err
	}
	q := "REVOKE " + grantPrivilegeSQL(grant) + " ON " + grant.ObjectSQL + " FROM " + grantRoleSQL(grant.Grantee)
	if grant.Grantor != "" {
		q += " GRANTED BY " + quote(grant.Grantor)
	}
	return []string{grantAsExactRole(q, grant.Grantor)}, nil
}

func renderGrantAlter(before, after schema.Resource, resources map[string]schema.Resource) ([]string, error) {
	b, err := parseGrant(before, resources)
	if err != nil {
		return nil, err
	}
	a, err := parseGrant(after, resources)
	if err != nil {
		return nil, err
	}
	if b.Target.ID != a.Target.ID || b.Privilege != a.Privilege || b.Grantee != a.Grantee || b.Grantor != a.Grantor {
		return nil, unsupported(after, "grant identity changes require revoke and grant")
	}
	if b.Grantable == a.Grantable {
		return nil, unsupported(after, "grant alteration has no semantic change")
	}
	if a.Grantable {
		return renderGrantCreate(after, resources)
	}
	q := "REVOKE GRANT OPTION FOR " + grantPrivilegeSQL(a) + " ON " + a.ObjectSQL + " FROM " + grantRoleSQL(a.Grantee)
	if a.Grantor != "" {
		q += " GRANTED BY " + quote(a.Grantor)
	}
	return []string{grantAsExactRole(q, a.Grantor)}, nil
}

// PostgreSQL requires GRANTED BY to name the current role. SET LOCAL confines
// the exact-grantor impersonation to the required transaction phase, and the
// explicit reset makes the statement safe for clients that inspect the
// session before commit. Runtime authority must still permit SET ROLE.
func grantAsExactRole(statement, grantor string) string {
	if grantor == "" {
		return statement
	}
	return "SET LOCAL ROLE " + quote(grantor) + "; " + statement + "; RESET ROLE"
}

func grantRoleSQL(role string) string {
	if strings.EqualFold(role, "PUBLIC") {
		return "PUBLIC"
	}
	return quote(role)
}

func grantPrivilegeSQL(grant parsedGrant) string {
	if grant.Target.Kind == schema.KindColumn {
		return grant.Privilege + " (" + quote(grant.Target.Name.Name) + ")"
	}
	return grant.Privilege
}

func grantExpectedDependencies(resource schema.Resource, resources map[string]schema.Resource) ([]string, error) {
	parsed, err := parseGrant(resource, resources)
	if err != nil {
		return nil, err
	}
	values := []string{parsed.Target.ID}
	for _, role := range []string{parsed.Grantee, parsed.Grantor} {
		if role == "" || strings.EqualFold(role, "PUBLIC") || protectedRole(role) {
			continue
		}
		id, _ := roleOwnerDependency(role, resources)
		values = append(values, id)
	}
	sort.Strings(values)
	return values, nil
}
