package postgres

import (
	"fmt"
	"slices"
	"strings"

	"autosql/pkg/schema"
	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

// parsedConstraint is the bounded, parser-proven representation of a table
// constraint definition. AutoSQL preserves PostgreSQL's canonical deparse in
// the document, but never executes it until the parser proves that it is one
// ADD CONSTRAINT command for the resource being rendered.
type parsedConstraint struct {
	statement  *pg_query.AlterTableStmt
	constraint *pg_query.Constraint
}

func parseConstraintDefinition(resource schema.Resource, resources map[string]schema.Resource) (parsedConstraint, error) {
	definition := stringValue(spec(resource), "definition")
	if definition == "" {
		return parsedConstraint{}, unsupported(resource, "constraint definition is required")
	}
	parent, err := parentResource(resource, resources)
	if err != nil {
		return parsedConstraint{}, err
	}
	sql := "ALTER TABLE " + qualified(parent.Name) + " ADD CONSTRAINT " + quote(resource.Name.Name) + " " + definition
	parsed, err := pg_query.Parse(sql)
	if err != nil || parsed == nil || len(parsed.Stmts) != 1 {
		return parsedConstraint{}, unsupported(resource, "constraint definition must parse as exactly one ADD CONSTRAINT command")
	}
	statement := parsed.Stmts[0].GetStmt().GetAlterTableStmt()
	if statement == nil || statement.GetMissingOk() || len(statement.GetCmds()) != 1 {
		return parsedConstraint{}, unsupported(resource, "constraint definition must be exactly one ADD CONSTRAINT command")
	}
	relation := statement.GetRelation()
	if relation == nil || relation.GetCatalogname() != "" || relation.GetSchemaname() != parent.Name.Schema || relation.GetRelname() != parent.Name.Name {
		return parsedConstraint{}, unsupported(resource, "constraint target does not match its containing table")
	}
	command := statement.GetCmds()[0].GetAlterTableCmd()
	if command == nil || command.GetSubtype() != pg_query.AlterTableType_AT_AddConstraint || command.GetMissingOk() {
		return parsedConstraint{}, unsupported(resource, "constraint definition is not an ADD CONSTRAINT command")
	}
	constraint := command.GetDef().GetConstraint()
	if constraint == nil || constraint.GetConname() != resource.Name.Name {
		return parsedConstraint{}, unsupported(resource, "constraint identity does not match its resource name")
	}
	want := map[schema.Kind]pg_query.ConstrType{
		schema.KindPrimaryKey:       pg_query.ConstrType_CONSTR_PRIMARY,
		schema.KindUniqueConstraint: pg_query.ConstrType_CONSTR_UNIQUE,
		schema.KindCheckConstraint:  pg_query.ConstrType_CONSTR_CHECK,
		schema.KindForeignKey:       pg_query.ConstrType_CONSTR_FOREIGN,
	}[resource.Kind]
	if want == pg_query.ConstrType_CONSTR_TYPE_UNDEFINED || constraint.GetContype() != want {
		return parsedConstraint{}, unsupported(resource, "constraint definition kind does not match its resource kind")
	}
	values := spec(resource)
	if raw, ok := values["deferrable"]; ok {
		value, valid := raw.(bool)
		if !valid || value != constraint.GetDeferrable() {
			return parsedConstraint{}, unsupported(resource, "constraint deferrable metadata does not match its definition")
		}
	}
	if raw, ok := values["initially_deferred"]; ok {
		value, valid := raw.(bool)
		if !valid || value != constraint.GetInitdeferred() {
			return parsedConstraint{}, unsupported(resource, "constraint initially_deferred metadata does not match its definition")
		}
	}
	if raw, ok := values["validated"]; ok {
		value, valid := raw.(bool)
		if !valid || value == constraint.GetSkipValidation() {
			return parsedConstraint{}, unsupported(resource, "constraint validated metadata does not match its definition")
		}
	}
	return parsedConstraint{statement: statement, constraint: constraint}, nil
}

type parsedIndex struct {
	statement *pg_query.IndexStmt
	tree      *pg_query.ParseResult
	SQL       string
}

func parseIndexDefinition(resource schema.Resource, resources map[string]schema.Resource) (parsedIndex, error) {
	values := spec(resource)
	definition := strings.TrimSpace(stringValue(values, "definition"))
	if definition == "" {
		return parsedIndex{}, unsupported(resource, "index definition is required")
	}
	parent, err := parentResource(resource, resources)
	if err != nil {
		return parsedIndex{}, err
	}
	sql := definition
	if !strings.HasPrefix(strings.ToUpper(definition), "CREATE ") {
		unique := ""
		if boolValue(values, "unique") {
			unique = "UNIQUE "
		}
		// PostgreSQL places an index in its table's schema and does not accept a
		// schema-qualified index name in CREATE INDEX.
		sql = "CREATE " + unique + "INDEX " + quote(resource.Name.Name) + " ON " + qualified(parent.Name) + " " + definition
	}
	parsed, err := pg_query.Parse(sql)
	if err != nil || parsed == nil || len(parsed.Stmts) != 1 {
		return parsedIndex{}, unsupported(resource, "index definition must parse as exactly one CREATE INDEX command")
	}
	statement := parsed.Stmts[0].GetStmt().GetIndexStmt()
	if statement == nil || statement.GetIfNotExists() || statement.GetConcurrent() || statement.GetPrimary() || statement.GetIsconstraint() {
		return parsedIndex{}, unsupported(resource, "index definition is outside the managed CREATE INDEX grammar")
	}
	relation := statement.GetRelation()
	if relation == nil || relation.GetCatalogname() != "" || relation.GetSchemaname() != parent.Name.Schema || relation.GetRelname() != parent.Name.Name {
		return parsedIndex{}, unsupported(resource, "index target does not match its containing table")
	}
	if statement.GetIdxname() != resource.Name.Name {
		return parsedIndex{}, unsupported(resource, "index identity does not match its resource name")
	}
	if raw, ok := values["unique"]; ok {
		value, valid := raw.(bool)
		if !valid || value != statement.GetUnique() {
			return parsedIndex{}, unsupported(resource, "index unique metadata does not match its definition")
		}
	}
	if method := stringValue(values, "method"); method != "" {
		parsedMethod := statement.GetAccessMethod()
		if parsedMethod == "" {
			parsedMethod = "btree"
		}
		if method != parsedMethod {
			return parsedIndex{}, unsupported(resource, "index method metadata does not match its definition")
		}
	}
	for _, key := range []string{"valid", "ready"} {
		if raw, ok := values[key]; ok {
			value, valid := raw.(bool)
			if !valid || !value {
				return parsedIndex{}, unsupported(resource, fmt.Sprintf("index %s must be true for managed creation", key))
			}
		}
	}
	return parsedIndex{statement: statement, tree: parsed, SQL: sql}, nil
}

func parentResource(resource schema.Resource, resources map[string]schema.Resource) (schema.Resource, error) {
	parent, ok := resources[resource.Name.Parent]
	if !ok || parent.Kind != schema.KindTable {
		return schema.Resource{}, unsupported(resource, "containing table is missing")
	}
	return parent, nil
}

func validateConstraintIndexSpec(resource schema.Resource, resources map[string]schema.Resource) error {
	switch resource.Kind {
	case schema.KindPrimaryKey, schema.KindUniqueConstraint, schema.KindCheckConstraint, schema.KindForeignKey:
		if !allowedKeys(spec(resource), "definition", "deferrable", "initially_deferred", "validated", "columns", "referenced_columns") {
			return unsupported(resource, "unknown constraint semantics")
		}
		_, err := parseConstraintDefinition(resource, resources)
		return err
	case schema.KindIndex:
		if !allowedKeys(spec(resource), "definition", "method", "unique", "valid", "ready", "columns") {
			return unsupported(resource, "unknown index semantics")
		}
		_, err := parseIndexDefinition(resource, resources)
		return err
	default:
		return nil
	}
}

func validateIndexAvailability(resource schema.Resource, parsed parsedIndex, options map[string]string) error {
	method := parsed.statement.GetAccessMethod()
	if method == "" {
		method = "btree"
	}
	builtins := map[string]bool{"btree": true, "hash": true, "gist": true, "spgist": true, "gin": true, "brin": true}
	if !builtins[method] && !commaOptionContains(options, "available_index_access_methods", method) {
		return unsupported(resource, fmt.Sprintf("index access method %q is not declared available", method))
	}
	knownOpclasses := map[string]bool{
		"text_pattern_ops": true, "varchar_pattern_ops": true, "bpchar_pattern_ops": true, "name_pattern_ops": true,
		"jsonb_ops": true, "jsonb_path_ops": true, "array_ops": true, "uuid_ops": true,
		"int2_ops": true, "int4_ops": true, "int8_ops": true, "numeric_ops": true,
		"timestamp_ops": true, "timestamptz_ops": true, "date_ops": true,
	}
	for _, node := range parsed.statement.GetIndexParams() {
		element := node.GetIndexElem()
		if element == nil || len(element.GetOpclass()) == 0 {
			continue
		}
		parts := make([]string, 0, len(element.GetOpclass()))
		for _, part := range element.GetOpclass() {
			if value := part.GetString_(); value != nil {
				parts = append(parts, value.GetSval())
			}
		}
		name := strings.Join(parts, ".")
		base := name
		if len(parts) > 0 {
			base = parts[len(parts)-1]
		}
		if !knownOpclasses[base] && !commaOptionContains(options, "available_index_opclasses", name) {
			return unsupported(resource, fmt.Sprintf("index operator class %q is not declared available", name))
		}
	}
	return nil
}

func commaOptionContains(options map[string]string, key, wanted string) bool {
	for _, value := range strings.Split(options[key], ",") {
		if strings.TrimSpace(value) == wanted {
			return true
		}
	}
	return false
}

func constraintIndexExpectedDependencies(resource schema.Resource, resources map[string]schema.Resource) ([]string, error) {
	expected := []string{resource.Name.Parent}
	parent, err := parentResource(resource, resources)
	if err != nil {
		return nil, err
	}
	addColumns := func(table schema.Resource, key string) error {
		for _, name := range stringSlice(spec(resource), key) {
			found := ""
			for id, candidate := range resources {
				if candidate.Kind == schema.KindColumn && candidate.Name.Parent == table.ID && candidate.Name.Name == name {
					found = id
					break
				}
			}
			if found == "" {
				return unsupported(resource, fmt.Sprintf("%s column %q is missing from the desired graph", key, name))
			}
			expected = append(expected, found)
		}
		return nil
	}
	if err := addColumns(parent, "columns"); err != nil {
		return nil, err
	}
	var expressionRoot protoreflect.Message
	if resource.Kind == schema.KindIndex {
		parsed, parseErr := parseIndexDefinition(resource, resources)
		if parseErr != nil {
			return nil, parseErr
		}
		expressionRoot = parsed.statement.ProtoReflect()
	} else {
		parsed, parseErr := parseConstraintDefinition(resource, resources)
		if parseErr != nil {
			return nil, parseErr
		}
		expressionRoot = parsed.statement.ProtoReflect()
	}
	if err := rejectUnsafeExpressionNodes(expressionRoot, resource); err != nil {
		return nil, err
	}
	routines, err := expressionRoutineDependencies(expressionRoot, resource.Name.Schema, resource, resources)
	if err != nil {
		return nil, err
	}
	expected = append(expected, routines...)
	for _, routine := range routines {
		if stringValue(spec(resources[routine]), "volatility") != "i" {
			return nil, unsupported(resource, "check and index expression routines must be immutable")
		}
	}
	if resource.Kind != schema.KindForeignKey {
		return expected, nil
	}
	parsed, err := parseConstraintDefinition(resource, resources)
	if err != nil {
		return nil, err
	}
	reference := parsed.constraint.GetPktable()
	if reference == nil || reference.GetCatalogname() != "" || reference.GetRelname() == "" {
		return nil, unsupported(resource, "foreign key referenced table is not canonical")
	}
	schemaName := reference.GetSchemaname()
	if schemaName == "" {
		schemaName = resource.Name.Schema
	}
	for id, candidate := range resources {
		if candidate.Kind == schema.KindTable && candidate.Name.Schema == schemaName && candidate.Name.Name == reference.GetRelname() {
			expected = append(expected, id)
			if err := addColumns(candidate, "referenced_columns"); err != nil {
				return nil, err
			}
			var keys []string
			for keyID, key := range resources {
				if (key.Kind == schema.KindPrimaryKey || key.Kind == schema.KindUniqueConstraint) && key.Name.Parent == candidate.ID && slices.Equal(stringSlice(spec(key), "columns"), stringSlice(spec(resource), "referenced_columns")) {
					keys = append(keys, keyID)
				}
			}
			if len(keys) == 0 {
				return nil, unsupported(resource, "foreign key has no matching referenced primary or unique constraint")
			}
			selected := ""
			for _, dependency := range resource.Dependencies {
				if slices.Contains(keys, dependency.Target) {
					selected = dependency.Target
					break
				}
			}
			if selected == "" && len(keys) == 1 {
				selected = keys[0]
			}
			if selected == "" {
				return nil, unsupported(resource, "foreign key referenced key dependency is ambiguous")
			}
			expected = append(expected, selected)
			return expected, nil
		}
	}
	return nil, unsupported(resource, "foreign key referenced table is missing from the desired graph")
}
