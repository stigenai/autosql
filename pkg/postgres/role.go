package postgres

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"autosql/pkg/schema"
	"autosql/pkg/secret"
	"github.com/jackc/pgx/v5"
)

var roleSettingName = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_.]*$`)

func validateRoleSpec(r schema.Resource, options map[string]string) error {
	if protectedRole(r.Name.Name) {
		return unsupported(r, "system/protected roles cannot be managed")
	}
	s := spec(r)
	if !allowedKeys(s, "superuser", "inherit", "create_role", "create_database", "login", "replication", "bypass_rls", "connection_limit", "valid_until", "configuration") {
		return unsupported(r, "unknown role semantics")
	}
	for _, key := range []string{"superuser", "inherit", "create_role", "create_database", "login", "replication", "bypass_rls"} {
		if _, ok := s[key].(bool); !ok {
			return unsupported(r, "role "+key+" must be boolean")
		}
	}
	limit, ok := numberValue(s, "connection_limit")
	if !ok {
		return unsupported(r, "role connection_limit must be an integer")
	}
	parsedLimit, err := strconv.Atoi(limit)
	if err != nil || parsedLimit < -1 {
		return unsupported(r, "role connection_limit must be -1 or nonnegative")
	}
	if value, exists := s["valid_until"]; exists {
		validUntil, stringOK := value.(string)
		if !stringOK || strings.TrimSpace(validUntil) == "" || len(validUntil) > 256 {
			return unsupported(r, "role valid_until must be a bounded string")
		}
	}
	if _, err := roleConfiguration(s); err != nil {
		return unsupported(r, err.Error())
	}
	if roleEscalates(s) && !enabled(options, "allow_role_escalation", false) {
		return unsupported(r, "privilege-escalating role attributes require allow_role_escalation=true")
	}
	return nil
}

func protectedRole(name string) bool {
	return name == "postgres" || strings.HasPrefix(name, "pg_") || name == "public" || name == "current_user" || name == "session_user"
}

func roleEscalates(s map[string]any) bool {
	return boolValue(s, "superuser") || boolValue(s, "create_role") || boolValue(s, "create_database") || boolValue(s, "replication") || boolValue(s, "bypass_rls")
}

func roleConfiguration(s map[string]any) (map[string]string, error) {
	configuration := map[string]string{}
	for _, entry := range stringSlice(s, "configuration") {
		key, value, found := strings.Cut(entry, "=")
		if !found || !roleSettingName.MatchString(key) || value == "" || len(value) > 4096 {
			return nil, errors.New("role configuration must contain bounded key=value entries")
		}
		if _, exists := configuration[key]; exists {
			return nil, errors.New("role configuration keys must be unique")
		}
		configuration[key] = value
	}
	return configuration, nil
}

func renderRoleCreate(r schema.Resource, options map[string]string) ([]string, error) {
	if err := validateRoleSpec(r, options); err != nil {
		return nil, err
	}
	s := spec(r)
	statement := "CREATE ROLE " + quote(r.Name.Name) + " WITH " + strings.Join(roleAttributeClauses(s), " ")
	out := []string{statement}
	configuration, _ := roleConfiguration(s)
	keys := sortedStringKeys(configuration)
	for _, key := range keys {
		out = append(out, "ALTER ROLE "+quote(r.Name.Name)+" SET "+quote(key)+" TO "+literal(configuration[key]))
	}
	return out, nil
}

func renderRoleAlter(before, after schema.Resource, options map[string]string) ([]string, error) {
	if err := validateRoleSpec(before, map[string]string{"allow_role_escalation": "true"}); err != nil {
		return nil, err
	}
	if err := validateRoleSpec(after, options); err != nil {
		return nil, err
	}
	bs, as := spec(before), spec(after)
	out := []string{}
	if !sameRoleAttributes(bs, as) {
		out = append(out, "ALTER ROLE "+quote(after.Name.Name)+" WITH "+strings.Join(roleAttributeClauses(as), " "))
	}
	beforeConfiguration, _ := roleConfiguration(bs)
	afterConfiguration, _ := roleConfiguration(as)
	for _, key := range sortedStringKeys(beforeConfiguration) {
		if _, retained := afterConfiguration[key]; !retained {
			out = append(out, "ALTER ROLE "+quote(after.Name.Name)+" RESET "+quote(key))
		}
	}
	for _, key := range sortedStringKeys(afterConfiguration) {
		if beforeConfiguration[key] != afterConfiguration[key] {
			out = append(out, "ALTER ROLE "+quote(after.Name.Name)+" SET "+quote(key)+" TO "+literal(afterConfiguration[key]))
		}
	}
	if len(out) == 0 {
		return nil, unsupported(after, "role alteration has no changed semantics")
	}
	return out, nil
}

func roleAttributeClauses(s map[string]any) []string {
	boolean := []struct {
		key, yes, no string
	}{
		{"superuser", "SUPERUSER", "NOSUPERUSER"}, {"inherit", "INHERIT", "NOINHERIT"},
		{"create_role", "CREATEROLE", "NOCREATEROLE"}, {"create_database", "CREATEDB", "NOCREATEDB"},
		{"login", "LOGIN", "NOLOGIN"}, {"replication", "REPLICATION", "NOREPLICATION"}, {"bypass_rls", "BYPASSRLS", "NOBYPASSRLS"},
	}
	out := make([]string, 0, len(boolean)+2)
	for _, attribute := range boolean {
		if boolValue(s, attribute.key) {
			out = append(out, attribute.yes)
		} else {
			out = append(out, attribute.no)
		}
	}
	limit, _ := numberValue(s, "connection_limit")
	out = append(out, "CONNECTION LIMIT "+limit)
	if validUntil := stringValue(s, "valid_until"); validUntil != "" {
		out = append(out, "VALID UNTIL "+literal(validUntil))
	}
	return out
}

func sameRoleAttributes(before, after map[string]any) bool {
	for _, key := range []string{"superuser", "inherit", "create_role", "create_database", "login", "replication", "bypass_rls"} {
		if boolValue(before, key) != boolValue(after, key) {
			return false
		}
	}
	beforeLimit, _ := numberValue(before, "connection_limit")
	afterLimit, _ := numberValue(after, "connection_limit")
	return beforeLimit == afterLimit && stringValue(before, "valid_until") == stringValue(after, "valid_until")
}

func renderRoleDrop(r schema.Resource, resources map[string]schema.Resource, options map[string]string) ([]string, error) {
	if protectedRole(r.Name.Name) || !enabled(options, "allow_role_drop", false) {
		return nil, unsupported(r, "role retirement requires an unprotected role and allow_role_drop=true")
	}
	target := strings.TrimSpace(options["reassign_owned_to."+r.Name.Name])
	if target == "" || target == r.Name.Name || protectedRole(target) {
		return nil, unsupported(r, "role retirement requires a safe reassign_owned_to.<role> target")
	}
	found := false
	for _, candidate := range resources {
		if candidate.Kind == schema.KindRole && candidate.Name.Name == target {
			found = true
		}
	}
	if !found {
		return nil, unsupported(r, "role retirement target must be declared")
	}
	return []string{
		"REASSIGN OWNED BY " + quote(r.Name.Name) + " TO " + quote(target),
		"DROP OWNED BY " + quote(r.Name.Name),
		"DROP ROLE " + quote(r.Name.Name),
	}, nil
}

func sortedStringKeys(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

// RolePasswordChange is a runtime-only, parameterized password operation.
// The secret value is never stored in a schema document or rendered plan.
type RolePasswordChange struct {
	Role      string
	Reference secret.Reference
}

// ApplyRolePasswordChange resolves and sends a password only as a PostgreSQL
// bind parameter. Errors never include the resolved value.
func ApplyRolePasswordChange(ctx context.Context, conn *pgx.Conn, resolver *secret.Resolver, change RolePasswordChange) error {
	if conn == nil || resolver == nil || protectedRole(change.Role) {
		return errors.New("invalid role password change")
	}
	if err := change.Reference.Validate(); err != nil {
		return errors.New("invalid role password secret reference")
	}
	password, err := resolver.Resolve(ctx, change.Reference)
	if err != nil {
		return errors.New("resolve role password secret")
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return errors.New("begin role password change")
	}
	defer tx.Rollback(context.Background())
	// PostgreSQL utility statements do not accept bind parameters. Keep both
	// role and password out of SQL text by passing them to a transaction-local
	// pg_temp helper, which quotes them server-side with format(%I, %L).
	const helper = `CREATE OR REPLACE FUNCTION pg_temp.autosql_set_role_password(target name, secret text) RETURNS void LANGUAGE plpgsql AS 'BEGIN EXECUTE format(''ALTER ROLE %I PASSWORD %L'', target, secret); END'`
	if _, err = tx.Exec(ctx, helper); err != nil {
		return errors.New("prepare role password change")
	}
	if _, err = tx.Exec(ctx, "SELECT pg_temp.autosql_set_role_password($1, $2)", change.Role, password); err != nil {
		return errors.New("apply role password change")
	}
	if _, err = tx.Exec(ctx, "DROP FUNCTION pg_temp.autosql_set_role_password(name, text)"); err != nil {
		return errors.New("finalize role password change")
	}
	if err = tx.Commit(ctx); err != nil {
		return errors.New("commit role password change")
	}
	return nil
}

func roleOwnerDependency(owner string, resources map[string]schema.Resource) (string, error) {
	if owner == "" {
		return "", nil
	}
	var matches []string
	for id, candidate := range resources {
		if candidate.Kind == schema.KindRole && candidate.Name.Name == owner {
			matches = append(matches, id)
		}
	}
	if len(matches) != 1 {
		return "", fmt.Errorf("owner role %q is undeclared or ambiguous", owner)
	}
	return matches[0], nil
}
