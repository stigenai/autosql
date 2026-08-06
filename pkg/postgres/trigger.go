package postgres

import (
	"fmt"
	"slices"
	"strings"

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
	if relation == nil || relation.GetCatalogname() != "" || relation.GetRelname() != parent.Name.Name || relation.GetSchemaname() != "" && relation.GetSchemaname() != parent.Name.Schema {
		return parsedTrigger{}, unsupported(resource, "trigger target does not match its containing table")
	}
	// pg_get_triggerdef may omit the target schema even though inspection has
	// already bound the trigger to an exact containing relation. Canonicalize
	// that spelling in the parsed tree so execution never depends on search_path.
	relation.Schemaname = parent.Name.Schema
	if len(statement.GetFuncname()) < 1 || len(statement.GetFuncname()) > 2 {
		return parsedTrigger{}, unsupported(resource, "trigger function identity is not canonical")
	}
	if err := schemaBindTriggerFunction(statement, resource, resources); err != nil {
		return parsedTrigger{}, err
	}
	canonical, err := pg_query.Deparse(parsed)
	if err != nil {
		return parsedTrigger{}, unsupported(resource, "trigger definition cannot be rendered canonically")
	}
	return parsedTrigger{statement: statement, SQL: strings.TrimSuffix(strings.TrimSpace(canonical), ";")}, nil
}

// schemaBindTriggerFunction proves the executable routine target from the
// trigger's exact function dependency before making the statement independent
// of search_path. PostgreSQL inspection may emit a bare EXECUTE FUNCTION name
// even though pg_depend has already identified the precise routine.
func schemaBindTriggerFunction(statement *pg_query.CreateTrigStmt, resource schema.Resource, resources map[string]schema.Resource) error {
	parts := statement.GetFuncname()
	if len(parts) < 1 || len(parts) > 2 {
		return unsupported(resource, "trigger function identity is not canonical")
	}
	for _, part := range parts {
		if part.GetString_() == nil || part.GetString_().GetSval() == "" {
			return unsupported(resource, "trigger function identity is not canonical")
		}
	}

	target, err := triggerFunctionDependency(resource, resources)
	if err != nil {
		return err
	}
	wantName := stringValue(spec(target), "name")
	gotSchema, gotName := "", parts[0].GetString_().GetSval()
	if len(parts) == 2 {
		gotSchema = parts[0].GetString_().GetSval()
		gotName = parts[1].GetString_().GetSval()
	}
	if wantName == "" || gotName != wantName || gotSchema != "" && gotSchema != target.Name.Schema || stringValue(spec(target), "identity_arguments") != "" {
		return unsupported(resource, "trigger function does not match its exact references dependency")
	}
	statement.Funcname = []*pg_query.Node{
		{Node: &pg_query.Node_String_{String_: &pg_query.String{Sval: target.Name.Schema}}},
		{Node: &pg_query.Node_String_{String_: &pg_query.String{Sval: wantName}}},
	}
	return nil
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
	_, err := parseTriggerDefinition(resource, resources)
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
	function, err := triggerFunctionDependency(resource, resources)
	if err != nil {
		return nil, err
	}
	expected = append(expected, function.ID)
	return expected, nil
}

func triggerFunctionDependency(resource schema.Resource, resources map[string]schema.Resource) (schema.Resource, error) {
	seen := map[string]bool{}
	var functions []schema.Resource
	for _, dependency := range resource.Dependencies {
		candidate, ok := resources[dependency.Target]
		if dependency.Type == schema.DependencyReferences && ok && candidate.Kind == schema.KindFunction && !seen[candidate.ID] {
			seen[candidate.ID] = true
			functions = append(functions, candidate)
		}
	}
	if len(functions) != 1 {
		return schema.Resource{}, unsupported(resource, "trigger function dependency is missing or ambiguous")
	}
	return functions[0], nil
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
