package postgres

import (
	"encoding/json"
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
	tree       *pg_query.ParseResult
}

func parseConstraintDefinition(resource schema.Resource, resources map[string]schema.Resource) (parsedConstraint, error) {
	definition := stringValue(spec(resource), "definition")
	if definition == "" {
		return parsedConstraint{}, unsupported(resource, "constraint definition is required")
	}
	parent, err := constraintParentResource(resource, resources)
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
	return parsedConstraint{statement: statement, constraint: constraint, tree: parsed}, nil
}

func renderConstraintCreateSQL(resource schema.Resource, resources map[string]schema.Resource) (string, error) {
	sql, _, err := renderConstraintCreate(resource, resources)
	return sql, err
}

func renderConstraintCreate(resource schema.Resource, resources map[string]schema.Resource) (string, string, error) {
	parsed, err := parseConstraintDefinition(resource, resources)
	if err != nil {
		return "", "", err
	}
	schemaBound, err := schemaBindForeignKeyReference(&parsed, resource, resources)
	if err != nil {
		return "", "", err
	}
	managedType := false
	for _, dependency := range resource.Dependencies {
		target := resources[dependency.Target]
		managedType = managedType || dependency.Type == schema.DependencyUses && (target.Kind == schema.KindEnum || target.Kind == schema.KindDomain || target.Kind == schema.KindComposite)
	}
	if !managedType && !schemaBound {
		parent, parentErr := constraintParentResource(resource, resources)
		if parentErr != nil {
			return "", "", parentErr
		}
		definition := stringValue(spec(resource), "definition")
		return "ALTER TABLE " + qualified(parent.Name) + " ADD CONSTRAINT " + quote(resource.Name.Name) + " " + definition, definition, nil
	}
	if err := qualifyExpressionTypeCasts(parsed.statement.ProtoReflect(), resource.Name.Schema, resource, resources); err != nil {
		return "", "", err
	}
	sql, err := pg_query.Deparse(parsed.tree)
	if err != nil {
		return "", "", unsupported(resource, "constraint definition could not be schema-bound")
	}
	marker := " ADD CONSTRAINT "
	position := strings.Index(sql, marker)
	if position < 0 {
		return "", "", unsupported(resource, "schema-bound constraint changed statement shape")
	}
	remainder := strings.TrimSpace(sql[position+len(marker):])
	if remainder == "" {
		return "", "", unsupported(resource, "schema-bound constraint lost its identity")
	}
	if remainder[0] == '"' {
		index := 1
		for index < len(remainder) {
			if remainder[index] != '"' {
				index++
				continue
			}
			if index+1 < len(remainder) && remainder[index+1] == '"' {
				index += 2
				continue
			}
			index++
			remainder = strings.TrimSpace(remainder[index:])
			break
		}
	} else if index := strings.IndexByte(remainder, ' '); index >= 0 {
		remainder = strings.TrimSpace(remainder[index+1:])
	} else {
		remainder = ""
	}
	if remainder == "" {
		return "", "", unsupported(resource, "schema-bound constraint lost its definition")
	}
	parent, err := constraintParentResource(resource, resources)
	if err != nil {
		return "", "", err
	}
	sql = "ALTER TABLE " + qualified(parent.Name) + " ADD CONSTRAINT " + quote(resource.Name.Name) + " " + remainder
	return sql, remainder, nil
}

// canonicalizeCheckExpressionCasts folds PostgreSQL's equivalent renderings
// of an array of scalar text casts into one array-level text[] cast. Older
// server/client combinations commonly deparsed
//
//	ARRAY[value::varchar::text, ...]
//
// while newer combinations deparse the same expression as
//
//	ARRAY[value::varchar, ...]::text[].
//
// Keeping one parser-proven spelling prevents cosmetic CHECK-expression
// changes from producing a different resource fingerprint or blocking
// adoption. The rewrite is deliberately limited to non-empty arrays whose
// every element has the same plain scalar text cast.
func canonicalizeCheckExpressionCasts(doc *schema.Document) error {
	resources := make(map[string]schema.Resource, len(doc.Graph.Resources))
	for _, resource := range doc.Graph.Resources {
		resources[resource.ID] = resource
	}
	for index := range doc.Graph.Resources {
		resource := &doc.Graph.Resources[index]
		if resource.Kind != schema.KindCheckConstraint {
			continue
		}
		parsed, err := parseConstraintDefinition(*resource, resources)
		if err != nil {
			// Preserve the existing normalization contract for constraint grammar
			// that is accepted only by a more narrowly scoped renderer.
			continue
		}
		var nodes []*pg_query.Node
		walkPostgresMessages(parsed.constraint.GetRawExpr().ProtoReflect(), func(message protoreflect.Message) {
			if node, ok := message.Interface().(*pg_query.Node); ok {
				nodes = append(nodes, node)
			}
		})
		changed := false
		for _, node := range nodes {
			changed = foldScalarTextArrayCasts(node) || changed
		}
		if !changed {
			for _, node := range nodes {
				cast := node.GetTypeCast()
				if cast != nil && cast.GetArg().GetAArrayExpr() != nil && plainTextArrayType(cast.GetTypeName()) {
					changed = true
					break
				}
			}
		}
		if !changed {
			continue
		}
		definition, err := deparseConstraintDefinition(parsed)
		if err != nil {
			return err
		}
		values := specMap(resource.Spec)
		values["definition"] = definition
		resource.Spec, err = json.Marshal(values)
		if err != nil {
			return err
		}
		resources[resource.ID] = *resource
	}
	return nil
}

func foldScalarTextArrayCasts(node *pg_query.Node) bool {
	array := node.GetAArrayExpr()
	if array == nil || len(array.GetElements()) == 0 {
		return false
	}
	var scalarType *pg_query.TypeName
	elements := make([]*pg_query.Node, len(array.GetElements()))
	for index, element := range array.GetElements() {
		cast := element.GetTypeCast()
		if cast == nil || cast.GetArg() == nil || !plainScalarTextType(cast.GetTypeName()) {
			return false
		}
		if scalarType == nil {
			scalarType = cast.GetTypeName()
		}
		elements[index] = cast.GetArg()
	}
	array.Elements = elements
	names := make([]*pg_query.Node, 0, len(scalarType.GetNames()))
	for _, name := range scalarType.GetNames() {
		names = append(names, &pg_query.Node{Node: &pg_query.Node_String_{String_: &pg_query.String{Sval: name.GetString_().GetSval()}}})
	}
	arrayType := &pg_query.TypeName{Names: names, ArrayBounds: []*pg_query.Node{pg_query.MakeIntNode(-1)}}
	node.Reset()
	node.Node = &pg_query.Node_TypeCast{TypeCast: &pg_query.TypeCast{
		Arg:      &pg_query.Node{Node: &pg_query.Node_AArrayExpr{AArrayExpr: array}},
		TypeName: arrayType,
	}}
	return true
}

func plainScalarTextType(name *pg_query.TypeName) bool {
	return plainTextType(name, 0)
}

func plainTextArrayType(name *pg_query.TypeName) bool {
	return plainTextType(name, 1)
}

func plainTextType(name *pg_query.TypeName, dimensions int) bool {
	if name == nil || name.GetSetof() || name.GetPctType() || len(name.GetTypmods()) != 0 || len(name.GetArrayBounds()) != dimensions {
		return false
	}
	parts := make([]string, 0, len(name.GetNames()))
	for _, part := range name.GetNames() {
		value := part.GetString_()
		if value == nil {
			return false
		}
		parts = append(parts, value.GetSval())
	}
	return len(parts) == 1 && parts[0] == "text" || len(parts) == 2 && parts[0] == "pg_catalog" && parts[1] == "text"
}

func deparseConstraintDefinition(parsed parsedConstraint) (string, error) {
	sql, err := pg_query.Deparse(parsed.tree)
	if err != nil {
		return "", unsupported(schema.Resource{Kind: schema.KindCheckConstraint}, "constraint definition could not be canonicalized")
	}
	marker := " ADD CONSTRAINT "
	position := strings.Index(sql, marker)
	if position < 0 {
		return "", unsupported(schema.Resource{Kind: schema.KindCheckConstraint}, "canonicalized constraint changed statement shape")
	}
	remainder := strings.TrimSpace(sql[position+len(marker):])
	if remainder == "" {
		return "", unsupported(schema.Resource{Kind: schema.KindCheckConstraint}, "canonicalized constraint lost its identity")
	}
	if remainder[0] == '"' {
		cursor := 1
		for cursor < len(remainder) {
			if remainder[cursor] != '"' {
				cursor++
				continue
			}
			if cursor+1 < len(remainder) && remainder[cursor+1] == '"' {
				cursor += 2
				continue
			}
			cursor++
			remainder = strings.TrimSpace(remainder[cursor:])
			break
		}
	} else if cursor := strings.IndexByte(remainder, ' '); cursor >= 0 {
		remainder = strings.TrimSpace(remainder[cursor+1:])
	} else {
		remainder = ""
	}
	if remainder == "" {
		return "", unsupported(schema.Resource{Kind: schema.KindCheckConstraint}, "canonicalized constraint lost its definition")
	}
	return remainder, nil
}

func schemaBindForeignKeyReference(parsed *parsedConstraint, resource schema.Resource, resources map[string]schema.Resource) (bool, error) {
	if resource.Kind != schema.KindForeignKey {
		return false, nil
	}
	reference := parsed.constraint.GetPktable()
	if reference == nil || reference.GetCatalogname() != "" || reference.GetRelname() == "" {
		return false, unsupported(resource, "foreign key referenced table is not canonical")
	}
	target, err := foreignKeyReferencedTable(resource, reference, resources)
	if err != nil {
		return false, err
	}
	reference.Schemaname = target.Name.Schema
	reference.Relname = target.Name.Name
	return true, nil
}

func foreignKeyReferencedTable(resource schema.Resource, reference *pg_query.RangeVar, resources map[string]schema.Resource) (schema.Resource, error) {
	seen := map[string]bool{}
	var matches []schema.Resource
	for _, dependency := range resource.Dependencies {
		candidate, ok := resources[dependency.Target]
		if dependency.Type == schema.DependencyReferences && ok && candidate.Kind == schema.KindTable && !seen[candidate.ID] {
			seen[candidate.ID] = true
			matches = append(matches, candidate)
		}
	}
	if len(matches) != 1 {
		return schema.Resource{}, unsupported(resource, "foreign key referenced table dependency is missing or ambiguous")
	}
	if reference.GetRelname() != matches[0].Name.Name || reference.GetSchemaname() != "" && reference.GetSchemaname() != matches[0].Name.Schema {
		return schema.Resource{}, unsupported(resource, "foreign key referenced table does not match its exact references dependency")
	}
	return matches[0], nil
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
	parent, err := indexParentResource(resource, resources)
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

func schemaBindIndexTypeCasts(parsed *parsedIndex, resource schema.Resource, resources map[string]schema.Resource) error {
	managedType := false
	for _, dependency := range resource.Dependencies {
		target := resources[dependency.Target]
		managedType = managedType || dependency.Type == schema.DependencyUses && (target.Kind == schema.KindEnum || target.Kind == schema.KindDomain || target.Kind == schema.KindComposite)
	}
	if !managedType {
		return nil
	}
	if err := qualifyExpressionTypeCasts(parsed.statement.ProtoReflect(), resource.Name.Schema, resource, resources); err != nil {
		return err
	}
	sql, err := pg_query.Deparse(parsed.tree)
	if err != nil {
		return unsupported(resource, "index definition could not be schema-bound")
	}
	parsed.SQL = sql
	return nil
}

func constraintParentResource(resource schema.Resource, resources map[string]schema.Resource) (schema.Resource, error) {
	parent, ok := resources[resource.Name.Parent]
	if !ok || parent.Kind != schema.KindTable {
		return schema.Resource{}, unsupported(resource, "containing table is missing")
	}
	return parent, nil
}

func indexParentResource(resource schema.Resource, resources map[string]schema.Resource) (schema.Resource, error) {
	parent, ok := resources[resource.Name.Parent]
	if !ok || parent.Kind != schema.KindTable && parent.Kind != schema.KindMaterializedView {
		return schema.Resource{}, unsupported(resource, "containing table or materialized view is missing")
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

func validateIndexAvailability(resource schema.Resource, parsed parsedIndex, resources map[string]schema.Resource, options map[string]string) error {
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
		if base == "inet_ops" {
			if err := validateBuiltinInetOpclass(resource, element, method, parts, resources); err != nil {
				return err
			}
			continue
		}
		if !knownOpclasses[base] && !commaOptionContains(options, "available_index_opclasses", name) {
			return unsupported(resource, fmt.Sprintf("index operator class %q is not declared available", name))
		}
	}
	return nil
}

func validateBuiltinInetOpclass(resource schema.Resource, element *pg_query.IndexElem, method string, parts []string, resources map[string]schema.Resource) error {
	if len(parts) != 1 && (len(parts) != 2 || parts[0] != "pg_catalog") {
		return unsupported(resource, "inet_ops must be unqualified or explicitly pg_catalog-qualified")
	}
	if method != "btree" && method != "hash" && method != "gist" && method != "spgist" {
		return unsupported(resource, fmt.Sprintf("inet_ops is unavailable for index access method %q", method))
	}
	if element.GetName() == "" || element.GetExpr() != nil {
		return unsupported(resource, "inet_ops requires one direct inet or cidr column")
	}
	parent, err := indexParentResource(resource, resources)
	if err != nil {
		return err
	}
	var matches []schema.Resource
	for _, candidate := range resources {
		if candidate.Kind == schema.KindColumn && candidate.Name.Parent == parent.ID && candidate.Name.Name == element.GetName() {
			matches = append(matches, candidate)
		}
	}
	if len(matches) != 1 {
		return unsupported(resource, "inet_ops index column is missing or ambiguous")
	}
	typ := postgresTypeAlias(stringValue(spec(matches[0]), "type"))
	if typ != "inet" && typ != "cidr" {
		return unsupported(resource, fmt.Sprintf("inet_ops requires inet or cidr, got %q", typ))
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
	var parent schema.Resource
	var err error
	if resource.Kind == schema.KindIndex {
		parent, err = indexParentResource(resource, resources)
	} else {
		parent, err = constraintParentResource(resource, resources)
	}
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
	if resource.Kind == schema.KindCheckConstraint || resource.Kind == schema.KindIndex {
		types, typeErr := expressionTypeDependencies(expressionRoot, resource.Name.Schema, resource, resources)
		if typeErr != nil {
			return nil, typeErr
		}
		expected = append(expected, types...)
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
	target, err := foreignKeyReferencedTable(resource, reference, resources)
	if err != nil {
		return nil, err
	}
	expected = append(expected, target.ID)
	if err := addColumns(target, "referenced_columns"); err != nil {
		return nil, err
	}
	var keys []string
	for keyID, key := range resources {
		if (key.Kind == schema.KindPrimaryKey || key.Kind == schema.KindUniqueConstraint) && key.Name.Parent == target.ID && slices.Equal(stringSlice(spec(key), "columns"), stringSlice(spec(resource), "referenced_columns")) {
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
