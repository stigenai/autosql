package postgres

import (
	"fmt"
	"slices"

	"autosql/pkg/schema"
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

type parsedTrigger struct {
	statement *pg_query.CreateTrigStmt
	SQL       string
}

func parseTriggerDefinition(resource schema.Resource, resources map[string]schema.Resource) (parsedTrigger, error) {
	definition := stringValue(spec(resource), "definition")
	if definition == "" {
		return parsedTrigger{}, unsupported(resource, "trigger definition is required")
	}
	parsed, err := pg_query.Parse(definition)
	if err != nil || parsed == nil || len(parsed.Stmts) != 1 {
		return parsedTrigger{}, unsupported(resource, "trigger definition must parse as exactly one CREATE TRIGGER statement")
	}
	statement := parsed.Stmts[0].GetStmt().GetCreateTrigStmt()
	if statement == nil || statement.GetReplace() || statement.GetTrigname() != resource.Name.Name {
		return parsedTrigger{}, unsupported(resource, "trigger definition identity is outside the managed grammar")
	}
	parent, ok := resources[resource.Name.Parent]
	if !ok || (parent.Kind != schema.KindTable && parent.Kind != schema.KindView) {
		return parsedTrigger{}, unsupported(resource, "trigger containing table or view is missing")
	}
	relation := statement.GetRelation()
	if relation == nil || relation.GetCatalogname() != "" || relation.GetSchemaname() != parent.Name.Schema || relation.GetRelname() != parent.Name.Name {
		return parsedTrigger{}, unsupported(resource, "trigger target does not match its containing table")
	}
	if len(statement.GetFuncname()) < 1 || len(statement.GetFuncname()) > 2 {
		return parsedTrigger{}, unsupported(resource, "trigger function identity is not canonical")
	}
	return parsedTrigger{statement: statement, SQL: definition}, nil
}

func validateTriggerSpec(resource schema.Resource, resources map[string]schema.Resource) error {
	values := spec(resource)
	if !allowedKeys(values, "definition", "enabled", "columns") {
		return unsupported(resource, "unknown trigger semantics")
	}
	if _, err := parseTriggerDefinition(resource, resources); err != nil {
		return err
	}
	switch stringValue(values, "enabled") {
	case "O", "D", "R", "A":
		return nil
	default:
		return unsupported(resource, "trigger enabled mode must be O, D, R, or A")
	}
}

func triggerExpectedDependencies(resource schema.Resource, resources map[string]schema.Resource) ([]string, error) {
	parsed, err := parseTriggerDefinition(resource, resources)
	if err != nil {
		return nil, err
	}
	parent, ok := resources[resource.Name.Parent]
	if !ok || (parent.Kind != schema.KindTable && parent.Kind != schema.KindView) {
		return nil, unsupported(resource, "trigger containing table or view is missing")
	}
	expected := []string{parent.ID}
	for _, column := range stringSlice(spec(resource), "columns") {
		found := ""
		for id, candidate := range resources {
			if candidate.Kind == schema.KindColumn && candidate.Name.Parent == parent.ID && candidate.Name.Name == column {
				found = id
				break
			}
		}
		if found == "" {
			return nil, unsupported(resource, fmt.Sprintf("trigger column %q is missing from the desired graph", column))
		}
		expected = append(expected, found)
	}
	parts := parsed.statement.GetFuncname()
	functionSchema, functionName := resource.Name.Schema, ""
	if len(parts) == 1 {
		functionName = parts[0].GetString_().GetSval()
	} else {
		functionSchema = parts[0].GetString_().GetSval()
		functionName = parts[1].GetString_().GetSval()
	}
	var functions []string
	for id, candidate := range resources {
		if candidate.Kind == schema.KindFunction && candidate.Name.Schema == functionSchema && stringValue(spec(candidate), "name") == functionName && stringValue(spec(candidate), "identity_arguments") == "" {
			functions = append(functions, id)
		}
	}
	if len(functions) != 1 {
		return nil, unsupported(resource, "trigger function dependency is missing or ambiguous")
	}
	expected = append(expected, functions[0])
	return expected, nil
}

func renderTriggerEnable(resource schema.Resource, resources map[string]schema.Resource) (string, error) {
	parent, err := parentName(resource, resources)
	if err != nil {
		return "", err
	}
	action := map[string]string{"O": "ENABLE", "D": "DISABLE", "R": "ENABLE REPLICA", "A": "ENABLE ALWAYS"}[stringValue(spec(resource), "enabled")]
	if action == "" {
		return "", unsupported(resource, "trigger enabled mode")
	}
	return "ALTER TABLE " + parent + " " + action + " TRIGGER " + quote(resource.Name.Name), nil
}

func triggerEnableOnly(before, after map[string]any) bool {
	if stringValue(before, "enabled") == stringValue(after, "enabled") {
		return false
	}
	keys := []string{"definition", "columns"}
	for _, key := range keys {
		if key == "columns" {
			if !slices.Equal(stringSlice(before, key), stringSlice(after, key)) {
				return false
			}
		} else if fmt.Sprint(before[key]) != fmt.Sprint(after[key]) {
			return false
		}
	}
	return len(before) == len(after)
}
