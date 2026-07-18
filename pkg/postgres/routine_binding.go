package postgres

import (
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"

	"autosql/pkg/schema"
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

type routineSourceEdit struct {
	start       int
	end         int
	replacement string
}

func canonicalizeRoutineBindings(doc *schema.Document) error {
	resources := make(map[string]schema.Resource, len(doc.Graph.Resources))
	for _, resource := range doc.Graph.Resources {
		resources[resource.ID] = resource
	}
	for index := range doc.Graph.Resources {
		resource := &doc.Graph.Resources[index]
		if resource.Kind != schema.KindFunction && resource.Kind != schema.KindProcedure || stringValue(spec(*resource), "extension") != "" {
			continue
		}
		definition := stringValue(spec(*resource), "definition")
		statement, _, err := parseRoutineStatement(*resource, definition)
		if err != nil {
			continue
		}
		targets, err := routineSignatureTypeDependencies(statement, *resource, resources)
		if err != nil {
			return err
		}
		for _, target := range targets {
			exists := false
			for _, dependency := range resource.Dependencies {
				exists = exists || dependency.Target == target && dependency.Type == schema.DependencyUses
			}
			if !exists {
				resource.Dependencies = append(resource.Dependencies, schema.Dependency{Target: target, Type: schema.DependencyUses})
			}
		}
		resources[resource.ID] = *resource
		definition, configuration, err := schemaBindRoutineSource(*resource, resources)
		if err != nil {
			return err
		}
		values := specMap(resource.Spec)
		values["definition"] = definition
		values["body_digest"] = routineDefinitionDigest(definition)
		values["configuration"] = configuration
		resource.Spec, err = json.Marshal(values)
		if err != nil {
			return err
		}
		resources[resource.ID] = *resource
	}
	return nil
}

func parseRoutineStatement(resource schema.Resource, definition string) (*pg_query.CreateFunctionStmt, *pg_query.ParseResult, error) {
	parsed, err := pg_query.Parse(definition)
	if err != nil || parsed == nil || len(parsed.Stmts) != 1 {
		return nil, nil, unsupported(resource, "routine source must parse as exactly one CREATE FUNCTION or CREATE PROCEDURE statement")
	}
	statement := parsed.Stmts[0].GetStmt().GetCreateFunctionStmt()
	if statement == nil || statement.GetIsProcedure() != (resource.Kind == schema.KindProcedure) {
		return nil, nil, unsupported(resource, "routine source kind does not match its resource kind")
	}
	return statement, parsed, nil
}

func routineTypeNames(statement *pg_query.CreateFunctionStmt) []*pg_query.TypeName {
	var types []*pg_query.TypeName
	for _, node := range statement.GetParameters() {
		if parameter := node.GetFunctionParameter(); parameter != nil && parameter.GetArgType() != nil {
			types = append(types, parameter.GetArgType())
		}
	}
	if statement.GetReturnType() != nil {
		types = append(types, statement.GetReturnType())
	}
	return types
}

func routineSignatureTypeDependencies(statement *pg_query.CreateFunctionStmt, resource schema.Resource, resources map[string]schema.Resource) ([]string, error) {
	seen := map[string]bool{}
	for _, typeName := range routineTypeNames(statement) {
		target, matched, err := expressionTypeTarget(typeName, resource.Name.Schema, resource, resources)
		if err != nil {
			return nil, err
		}
		if matched {
			seen[target.ID] = true
		}
	}
	result := make([]string, 0, len(seen))
	for target := range seen {
		result = append(result, target)
	}
	sort.Strings(result)
	return result, nil
}

func schemaBindRoutineTypeName(typeName *pg_query.TypeName, resource schema.Resource, resources map[string]schema.Resource) error {
	target, matched, err := expressionTypeTarget(typeName, resource.Name.Schema, resource, resources)
	if err != nil || !matched {
		return err
	}
	declared := false
	for _, dependency := range resource.Dependencies {
		declared = declared || dependency.Target == target.ID && dependency.Type == schema.DependencyUses
	}
	if !declared {
		return unsupported(resource, "routine signature type is missing its exact uses dependency")
	}
	typeName.Names = []*pg_query.Node{
		{Node: &pg_query.Node_String_{String_: &pg_query.String{Sval: target.Name.Schema}}},
		{Node: &pg_query.Node_String_{String_: &pg_query.String{Sval: target.Name.Name}}},
	}
	return nil
}

func schemaBindRoutineSource(resource schema.Resource, resources map[string]schema.Resource) (string, []string, error) {
	definition := stringValue(spec(resource), "definition")
	statement, _, err := parseRoutineStatement(resource, definition)
	if err != nil {
		return "", nil, err
	}
	edits := make([]routineSourceEdit, 0, len(statement.GetParameters())+2)
	declared := map[string]bool{}
	for _, dependency := range resource.Dependencies {
		if dependency.Type == schema.DependencyUses {
			declared[dependency.Target] = true
		}
	}
	for _, typeName := range routineTypeNames(statement) {
		target, matched, targetErr := expressionTypeTarget(typeName, resource.Name.Schema, resource, resources)
		if targetErr != nil {
			return "", nil, targetErr
		}
		if !matched {
			continue
		}
		if !declared[target.ID] {
			return "", nil, unsupported(resource, "routine signature type is missing its exact uses dependency")
		}
		start, end, spanErr := routineTypeNameSpan(definition, typeName)
		if spanErr != nil {
			return "", nil, unsupported(resource, spanErr.Error())
		}
		edits = append(edits, routineSourceEdit{start: start, end: end, replacement: routineQualifiedTypeName(target.Name)})
	}

	requiresRuntimeSearchPath := statement.GetSqlBody() == nil
	configuration := canonicalRoutineConfiguration(resource, requiresRuntimeSearchPath)
	hasSourceSearchPath, asLocation := routineSourceConfiguration(statement)
	if !hasSourceSearchPath && requiresRuntimeSearchPath {
		if asLocation < 0 || asLocation > len(definition) {
			return "", nil, unsupported(resource, "routine AS clause location is not parser-proven")
		}
		edits = append(edits, routineSourceEdit{start: asLocation, end: asLocation, replacement: routineSearchPathClause(resource.Name.Schema)})
	} else if hasSourceSearchPath && !managedRoutineSearchPath(stringSlice(spec(resource), "configuration"), resource.Name.Schema) {
		return "", nil, unsupported(resource, "routine source search_path must begin with pg_catalog and include its schema")
	}

	sort.Slice(edits, func(i, j int) bool { return edits[i].start > edits[j].start })
	canonical := definition
	lastStart := len(definition) + 1
	for _, edit := range edits {
		if edit.start < 0 || edit.end < edit.start || edit.end > len(canonical) || edit.end > lastStart {
			return "", nil, unsupported(resource, "routine source bindings overlap or exceed parser-proven spans")
		}
		canonical = canonical[:edit.start] + edit.replacement + canonical[edit.end:]
		lastStart = edit.start
	}
	if _, _, err := parseRoutineStatement(resource, canonical); err != nil {
		return "", nil, unsupported(resource, "schema-bound routine source no longer parses")
	}
	return canonical, configuration, nil
}

var routineSafeIdentifier = regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)

func routineQualifiedTypeName(name schema.Name) string {
	identifier := func(value string) string {
		if routineSafeIdentifier.MatchString(value) {
			return value
		}
		return quote(value)
	}
	return identifier(name.Schema) + "." + identifier(name.Name)
}

func routineTypeNameSpan(definition string, typeName *pg_query.TypeName) (int, int, error) {
	if typeName == nil || typeName.GetLocation() < 0 || len(typeName.GetNames()) == 0 {
		return 0, 0, fmt.Errorf("routine signature type location is not parser-proven")
	}
	scan, err := pg_query.Scan(definition)
	if err != nil {
		return 0, 0, fmt.Errorf("routine source cannot be tokenized")
	}
	start := int(typeName.GetLocation())
	index := -1
	for tokenIndex, token := range scan.GetTokens() {
		if int(token.GetStart()) == start {
			index = tokenIndex
			break
		}
	}
	if index < 0 {
		return 0, 0, fmt.Errorf("routine signature type token is missing")
	}
	end := int(scan.GetTokens()[index].GetEnd())
	for part := 1; part < len(typeName.GetNames()); part++ {
		if index+2 >= len(scan.GetTokens()) {
			return 0, 0, fmt.Errorf("routine signature type qualification is truncated")
		}
		dot := scan.GetTokens()[index+1]
		if definition[dot.GetStart():dot.GetEnd()] != "." {
			return 0, 0, fmt.Errorf("routine signature type qualification is not canonical")
		}
		index += 2
		end = int(scan.GetTokens()[index].GetEnd())
	}
	return start, end, nil
}

func routineSourceConfiguration(statement *pg_query.CreateFunctionStmt) (bool, int) {
	asLocation := -1
	for _, node := range statement.GetOptions() {
		option := node.GetDefElem()
		if option == nil {
			continue
		}
		if option.GetDefname() == "as" {
			asLocation = int(option.GetLocation())
		}
		if option.GetDefname() == "set" {
			setting := option.GetArg().GetVariableSetStmt()
			if setting != nil && strings.EqualFold(setting.GetName(), "search_path") {
				return true, asLocation
			}
		}
	}
	return false, asLocation
}

func routineSearchPathClause(namespace string) string {
	paths := []string{"pg_catalog"}
	if namespace != "pg_catalog" {
		paths = append(paths, namespace)
	}
	if namespace != "public" {
		paths = append(paths, "public")
	}
	quoted := make([]string, len(paths))
	for index, path := range paths {
		quoted[index] = literal(path)
	}
	return " SET search_path TO " + strings.Join(quoted, ", ") + "\n"
}

func canonicalRoutineSearchPath(namespace string) string {
	paths := []string{"pg_catalog"}
	if namespace != "pg_catalog" {
		paths = append(paths, namespace)
	}
	if namespace != "public" {
		paths = append(paths, "public")
	}
	return "search_path=" + strings.Join(paths, ", ")
}

func canonicalRoutineConfiguration(resource schema.Resource, required bool) []string {
	configuration := append([]string(nil), stringSlice(spec(resource), "configuration")...)
	for _, setting := range configuration {
		if strings.EqualFold(strings.TrimSpace(strings.SplitN(setting, "=", 2)[0]), "search_path") {
			return configuration
		}
	}
	if !required {
		return configuration
	}
	return append(configuration, canonicalRoutineSearchPath(resource.Name.Schema))
}

func managedRoutineSearchPath(configuration []string, namespace string) bool {
	return safeRoutineSearchPath(configuration)
}

func validateRoutineDependencies(resource schema.Resource, resources map[string]schema.Resource) error {
	if stringValue(spec(resource), "extension") != "" {
		return nil
	}
	statement, _, err := parseRoutineStatement(resource, stringValue(spec(resource), "definition"))
	if err != nil {
		// Renderability diagnostics own malformed or legacy opaque routine source.
		return nil
	}
	expected, err := routineSignatureTypeDependencies(statement, resource, resources)
	if err != nil {
		return err
	}
	var actual []string
	seen := map[string]bool{}
	for _, dependency := range resource.Dependencies {
		if dependency.Type == schema.DependencyUses && !extensionDependency(dependency, resources) && !seen[dependency.Target] {
			seen[dependency.Target] = true
			actual = append(actual, dependency.Target)
		}
	}
	sort.Strings(actual)
	if len(expected) != len(actual) {
		return unsupported(resource, "declared type dependencies do not exactly match rendered routine signature")
	}
	for index := range expected {
		if expected[index] != actual[index] {
			return unsupported(resource, "declared type dependencies do not exactly match rendered routine signature")
		}
	}
	return nil
}
