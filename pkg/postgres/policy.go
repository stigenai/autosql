package postgres

import (
	"errors"
	"fmt"
	"sort"
	"strings"

	"autosql/pkg/schema"
	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type parsedPolicy struct {
	SQL          string
	Dependencies []string
}

func parsePolicy(r schema.Resource, resources map[string]schema.Resource) (parsedPolicy, error) {
	s := spec(r)
	if !allowedKeys(s, "command", "permissive", "roles", "using", "check") {
		return parsedPolicy{}, unsupported(r, "unknown policy semantics")
	}
	parent, err := parentName(r, resources)
	if err != nil {
		return parsedPolicy{}, err
	}
	command, ok := map[string]string{"*": "ALL", "r": "SELECT", "a": "INSERT", "w": "UPDATE", "d": "DELETE"}[stringValue(s, "command")]
	if !ok {
		return parsedPolicy{}, unsupported(r, "policy command must be canonical")
	}
	permissive, ok := s["permissive"].(bool)
	if !ok {
		return parsedPolicy{}, unsupported(r, "policy permissive must be boolean")
	}
	roles := stringSlice(s, "roles")
	if len(roles) == 0 {
		return parsedPolicy{}, unsupported(r, "policy roles must be explicit")
	}
	using, check := stringValue(s, "using"), stringValue(s, "check")
	switch command {
	case "SELECT", "DELETE":
		if using == "" || check != "" {
			return parsedPolicy{}, unsupported(r, "read/delete policy requires USING and forbids WITH CHECK")
		}
	case "INSERT":
		if using != "" || check == "" {
			return parsedPolicy{}, unsupported(r, "insert policy requires WITH CHECK and forbids USING")
		}
	case "ALL", "UPDATE":
		if using == "" || check == "" {
			return parsedPolicy{}, unsupported(r, "all/update policy requires both USING and WITH CHECK")
		}
	}

	dependencies := []string{r.Name.Parent}
	quotedRoles := make([]string, 0, len(roles))
	seenRoles := map[string]bool{}
	for _, role := range roles {
		if role == "" || seenRoles[role] {
			return parsedPolicy{}, unsupported(r, "policy roles must be non-empty and unique")
		}
		seenRoles[role] = true
		if role == "public" {
			quotedRoles = append(quotedRoles, "PUBLIC")
			continue
		}
		var matches []string
		for id, candidate := range resources {
			if candidate.Kind == schema.KindRole && candidate.Name.Name == role {
				matches = append(matches, id)
			}
		}
		if len(matches) != 1 {
			return parsedPolicy{}, unsupported(r, "policy role is undeclared or ambiguous")
		}
		dependencies = append(dependencies, matches[0])
		quotedRoles = append(quotedRoles, quote(role))
	}
	sort.Strings(quotedRoles)

	for _, expression := range []string{using, check} {
		if expression == "" {
			continue
		}
		expressionDependencies, expressionErr := policyExpressionDependencies(expression, r, resources)
		if expressionErr != nil {
			return parsedPolicy{}, expressionErr
		}
		dependencies = append(dependencies, expressionDependencies...)
	}
	dependencies = uniqueSorted(dependencies)

	mode := "RESTRICTIVE"
	if permissive {
		mode = "PERMISSIVE"
	}
	sql := "CREATE POLICY " + quote(r.Name.Name) + " ON " + parent + " AS " + mode + " FOR " + command + " TO " + strings.Join(quotedRoles, ", ")
	if using != "" {
		sql += " USING (" + using + ")"
	}
	if check != "" {
		sql += " WITH CHECK (" + check + ")"
	}
	parsed, parseErr := pg_query.Parse(sql)
	if parseErr != nil || len(parsed.GetStmts()) != 1 || parsed.GetStmts()[0].GetStmt().GetCreatePolicyStmt() == nil {
		return parsedPolicy{}, unsupported(r, "policy is not one valid CREATE POLICY statement")
	}
	stmt := parsed.GetStmts()[0].GetStmt().GetCreatePolicyStmt()
	if stmt.GetPolicyName() != r.Name.Name || stmt.GetTable().GetSchemaname() != resources[r.Name.Parent].Name.Schema || stmt.GetTable().GetRelname() != resources[r.Name.Parent].Name.Name {
		return parsedPolicy{}, unsupported(r, "policy parsed identity differs from canonical identity")
	}
	return parsedPolicy{SQL: sql, Dependencies: dependencies}, nil
}

func policyExpressionDependencies(expression string, resource schema.Resource, resources map[string]schema.Resource) ([]string, error) {
	if err := validateDefaultLexicalForm(expression); err != nil {
		return nil, unsupported(resource, "unsafe policy expression: "+err.Error())
	}
	parsed, err := pg_query.Parse("SELECT " + expression)
	if err != nil || len(parsed.GetStmts()) != 1 {
		return nil, unsupported(resource, "policy expression is not one valid expression")
	}
	stmt := parsed.GetStmts()[0].GetStmt().GetSelectStmt()
	if !isDefaultExpressionSelect(stmt) {
		return nil, unsupported(resource, "policy expression wrapper contains statement features")
	}
	node := stmt.GetTargetList()[0].GetResTarget().GetVal()
	if err := rejectUnsafeExpressionNodes(node.ProtoReflect(), resource); err != nil {
		return nil, err
	}
	parent := resources[resource.Name.Parent]
	dependencies := []string{}
	var problem error
	walkPolicyExpression(node.ProtoReflect(), func(message protoreflect.Message) {
		if problem != nil {
			return
		}
		if ref, ok := message.Interface().(*pg_query.ColumnRef); ok {
			parts := nodeStringParts(ref.GetFields())
			if len(parts) == 0 || len(parts) > 2 || len(parts) == 2 && parts[0] != parent.Name.Name {
				problem = errors.New("policy column reference is not local to its table")
				return
			}
			column := parts[len(parts)-1]
			matches := []string{}
			for id, candidate := range resources {
				if candidate.Kind == schema.KindColumn && candidate.Name.Parent == parent.ID && candidate.Name.Name == column {
					matches = append(matches, id)
				}
			}
			if len(matches) != 1 {
				problem = fmt.Errorf("policy column %q is undeclared or ambiguous", column)
				return
			}
			dependencies = append(dependencies, matches[0])
		}
		if call, ok := message.Interface().(*pg_query.FuncCall); ok {
			parts := nodeStringParts(call.GetFuncname())
			name := strings.Join(parts, ".")
			if name == "current_setting" || name == "pg_catalog.current_setting" || immutableBuiltinExpressionFunction(name) {
				return
			}
			matches := []string{}
			for id, candidate := range resources {
				if candidate.Kind == schema.KindFunction && generatedFunctionReferenceMatches(defaultReference{Parts: parts}, parent.Name.Schema, candidate) {
					matches = append(matches, id)
				}
			}
			if len(matches) != 1 {
				problem = fmt.Errorf("policy routine %q is undeclared or ambiguous", name)
				return
			}
			dependencies = append(dependencies, matches[0])
		}
	})
	if problem != nil {
		return nil, unsupported(resource, problem.Error())
	}
	return uniqueSorted(dependencies), nil
}

func walkPolicyExpression(message protoreflect.Message, visit func(protoreflect.Message)) {
	if !message.IsValid() {
		return
	}
	visit(message)
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		switch {
		case field.IsList() && field.Kind() == protoreflect.MessageKind:
			list := value.List()
			for index := 0; index < list.Len(); index++ {
				walkPolicyExpression(list.Get(index).Message(), visit)
			}
		case field.Kind() == protoreflect.MessageKind:
			walkPolicyExpression(value.Message(), visit)
		}
		return true
	})
}

func nodeStringParts(nodes []*pg_query.Node) []string {
	parts := make([]string, 0, len(nodes))
	for _, node := range nodes {
		if value := node.GetString_(); value != nil {
			parts = append(parts, value.GetSval())
		}
	}
	return parts
}

func uniqueSorted(values []string) []string {
	seen := map[string]bool{}
	out := make([]string, 0, len(values))
	for _, value := range values {
		if value != "" && !seen[value] {
			seen[value] = true
			out = append(out, value)
		}
	}
	sort.Strings(out)
	return out
}

func policyExpectedDependencies(r schema.Resource, resources map[string]schema.Resource) ([]string, error) {
	parsed, err := parsePolicy(r, resources)
	if err != nil {
		return nil, err
	}
	return parsed.Dependencies, nil
}
