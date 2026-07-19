package source

import (
	"fmt"
	"sort"
	"strings"

	"autosql/pkg/schema"

	"github.com/hashicorp/hcl/v2/hclsyntax"
)

func hclBlockIdentity(block *hclsyntax.Block, parentName string) (string, string, error) {
	if len(block.Labels) == 0 && block.Type == "primary_key" && parentName != "" {
		return block.Type, parentName + "_pkey", nil
	}
	return blockIdentity(block)
}

func inlineHCLBlock(parent schema.Kind, child string) bool {
	switch parent {
	case schema.KindComposite:
		return child == "attribute"
	case schema.KindDomain:
		return child == "check"
	case schema.KindColumn:
		return child == "generated" || child == "identity"
	case schema.KindTable:
		return child == "partition"
	case schema.KindIndex:
		return child == "on"
	case schema.KindFunction, schema.KindProcedure:
		return child == "arg" || child == "return_table"
	case schema.KindTrigger:
		return child == "before" || child == "after" || child == "instead_of" || child == "execute" || child == "referencing"
	case schema.KindView, schema.KindMaterializedView:
		return child == "security" || child == "column"
	}
	return false
}

func lowerNativeHCLResource(kind schema.Kind, name schema.Name, block *hclsyntax.Block, spec map[string]any, dependencies *[]schema.Dependency, symbols *hclSymbolTable, data []byte, variables HCLVariables) error {
	if err := lowerNativeDependsOn(spec, dependencies); err != nil {
		return err
	}
	switch kind {
	case schema.KindExtension:
		setHCLDefault(spec, "relocatable", false)
		setHCLDefault(spec, "trusted", false)
		setHCLDefault(spec, "superuser", true)
		setHCLDefault(spec, "requires", []any{})
	case schema.KindTable:
		setHCLDefault(spec, "partitioned", false)
		setHCLDefault(spec, "persistence", "p")
		setHCLDefault(spec, "row_security", false)
		setHCLDefault(spec, "force_row_security", false)
		if err := lowerNativeTablePartition(name, block, spec, dependencies, symbols, data, variables); err != nil {
			return err
		}
	case schema.KindColumn:
		setHCLDefault(spec, "not_null", false)
		if nullable, ok := hclBooleanAlias(spec, "null", "nullable"); ok {
			spec["not_null"] = !nullable
		}
		delete(spec, "null")
		delete(spec, "nullable")
		id := schema.StableID(kind, name)
		if _, ok := spec["ordinal"]; !ok && symbols.ordinal(id) > 0 {
			spec["ordinal"] = symbols.ordinal(id)
		}
		if generated, ok := spec["generated"]; ok {
			if _, isMode := generated.(string); !isMode || generated != "s" {
				spec["default"] = generated
				spec["generated"] = "s"
			}
		}
		if err := lowerColumnInlineBlocks(block, spec, dependencies, symbols, data, variables, name.Parent); err != nil {
			return err
		}
		if identity, ok := spec["identity"].(string); ok {
			switch strings.ToLower(identity) {
			case "always", "a":
				spec["identity"] = "a"
			case "by_default", "by default", "d":
				spec["identity"] = "d"
			}
		}
	case schema.KindDomain:
		if base, ok := spec["type"]; ok {
			spec["base_type"] = base
			delete(spec, "type")
		}
		setHCLDefault(spec, "not_null", false)
		if err := lowerDomainChecks(block, spec, dependencies, symbols, data, variables, schema.StableID(kind, name)); err != nil {
			return err
		}
	case schema.KindComposite:
		if err := lowerCompositeAttributes(block, spec, dependencies, symbols, data, variables, schema.StableID(kind, name)); err != nil {
			return err
		}
	case schema.KindPrimaryKey, schema.KindUniqueConstraint, schema.KindCheckConstraint, schema.KindForeignKey:
		return lowerNativeConstraint(kind, name, spec, dependencies, symbols)
	case schema.KindIndex:
		return lowerNativeIndex(name, block, spec, dependencies, symbols, data, variables)
	case schema.KindView, schema.KindMaterializedView:
		if query, ok := spec["query"]; ok {
			spec["definition"] = query
			delete(spec, "query")
		}
	case schema.KindFunction, schema.KindProcedure:
		return lowerNativeRoutine(kind, name, spec)
	case schema.KindTrigger:
		return lowerNativeTrigger(name, spec, dependencies, symbols)
	case schema.KindPolicy:
		return lowerNativePolicy(spec, dependencies)
	case schema.KindRole:
		lowerNativeRole(spec)
	case schema.KindMembership:
		return lowerNativeRoleReferences(spec, dependencies, "parent", "member", "grantor")
	case schema.KindGrant:
		return lowerNativeGrant(spec, dependencies)
	case schema.KindDefaultPrivilege:
		return lowerNativeDefaultPrivilege(spec, dependencies)
	}
	return nil
}

func lowerNativeTablePartition(name schema.Name, block *hclsyntax.Block, spec map[string]any, dependencies *[]schema.Dependency, symbols *hclSymbolTable, data []byte, variables HCLVariables) error {
	for _, nested := range block.Body.Blocks {
		if nested.Type != "partition" {
			continue
		}
		values, _, err := attributesAndNested(nested.Body, data, variables, hclBlockEvaluationSymbols(nested, data, variables, symbols.variables(schema.StableID(schema.KindTable, name))))
		if err != nil {
			return err
		}
		strategy := strings.ToLower(stringValueHCL(values["by"]))
		if strategy == "" {
			strategy = strings.ToLower(stringValueHCL(values["strategy"]))
		}
		if strategy != "range" && strategy != "list" && strategy != "hash" {
			return fmt.Errorf("%w: partition strategy must be range, list, or hash", ErrHCL)
		}
		items, ok := values["columns"].([]any)
		if !ok || len(items) == 0 {
			return fmt.Errorf("%w: partition columns must be a non-empty column reference list", ErrHCL)
		}
		columns := make([]any, len(items))
		for index, item := range items {
			ref, ok := referenceFromAny(item)
			parent := symbols.reference(ref.Parent)
			if !ok || ref.Kind != schema.KindColumn || parent.Kind != schema.KindTable || parent.Name != name.Name || parent.Schema != name.Schema {
				return fmt.Errorf("%w: partition columns must reference local columns", ErrHCL)
			}
			columns[index] = ref.Name
		}
		spec["partitioned"], spec["partition_strategy"], spec["partition_columns"] = true, strategy, columns
	}
	if raw, exists := spec["partition_of"]; exists {
		parent, ok := referenceFromAny(raw)
		if !ok || parent.Kind != schema.KindTable {
			return fmt.Errorf("%w: partition_of must be a table reference", ErrHCL)
		}
		spec["partition_of"] = parent.ID
		appendHCLDependency(dependencies, schema.Dependency{Target: parent.ID, Type: schema.DependencyReferences})
		if rawBound, ok := spec["bound"]; ok {
			bound, err := nativeHCLExpression(rawBound, "partition bound", dependencies)
			if err != nil {
				return err
			}
			spec["partition_bound"] = bound
			delete(spec, "bound")
		}
	}
	return nil
}

func lowerNativeDependsOn(spec map[string]any, dependencies *[]schema.Dependency) error {
	raw, exists := spec["depends_on"]
	if !exists {
		return nil
	}
	items, ok := raw.([]any)
	if !ok {
		return fmt.Errorf("%w: depends_on must be a list of resource references", ErrHCL)
	}
	for _, item := range items {
		ref, ok := referenceFromAny(item)
		if !ok {
			return fmt.Errorf("%w: depends_on must contain only resource references", ErrHCL)
		}
		appendHCLDependency(dependencies, schema.Dependency{Target: ref.ID, Type: schema.DependencyReferences})
	}
	delete(spec, "depends_on")
	return nil
}

func setHCLDefault(spec map[string]any, key string, value any) {
	if _, ok := spec[key]; !ok {
		spec[key] = value
	}
}

func hclBooleanAlias(spec map[string]any, names ...string) (bool, bool) {
	for _, name := range names {
		if value, ok := spec[name].(bool); ok {
			return value, true
		}
	}
	return false, false
}

func lowerColumnInlineBlocks(block *hclsyntax.Block, spec map[string]any, dependencies *[]schema.Dependency, symbols *hclSymbolTable, data []byte, variables HCLVariables, parent string) error {
	for _, nested := range block.Body.Blocks {
		if nested.Type != "generated" && nested.Type != "identity" {
			continue
		}
		values, _, err := attributesAndNested(nested.Body, data, variables, hclBlockEvaluationSymbols(nested, data, variables, symbols.variables(parent)))
		if err != nil {
			return err
		}
		switch nested.Type {
		case "generated":
			expression, err := nativeHCLExpression(values["expr"], "generated", dependencies)
			if err != nil {
				return err
			}
			if stored, ok := values["stored"].(bool); ok && !stored {
				return fmt.Errorf("%w: PostgreSQL generated columns must be stored", ErrHCL)
			}
			spec["default"], spec["generated"] = expression, "s"
		case "identity":
			mode, _ := values["mode"].(string)
			if strings.EqualFold(mode, "always") {
				spec["identity"] = "a"
			} else {
				spec["identity"] = "d"
			}
		}
	}
	return nil
}

func lowerDomainChecks(block *hclsyntax.Block, spec map[string]any, dependencies *[]schema.Dependency, symbols *hclSymbolTable, data []byte, variables HCLVariables, parent string) error {
	var constraints []any
	if existing, ok := spec["constraints"].([]any); ok {
		constraints = append(constraints, existing...)
	}
	for _, nested := range block.Body.Blocks {
		if nested.Type != "check" {
			continue
		}
		values, _, err := attributesAndNested(nested.Body, data, variables, hclBlockEvaluationSymbols(nested, data, variables, symbols.variables(parent)))
		if err != nil {
			return err
		}
		expression, err := nativeHCLExpression(values["expr"], "check", dependencies)
		if err != nil {
			return err
		}
		constraints = append(constraints, "CHECK ("+expression+")")
	}
	if len(constraints) > 0 {
		spec["constraints"] = constraints
	}
	return nil
}

func lowerCompositeAttributes(block *hclsyntax.Block, spec map[string]any, dependencies *[]schema.Dependency, symbols *hclSymbolTable, data []byte, variables HCLVariables, parent string) error {
	if _, exists := spec["attributes"]; exists {
		return nil
	}
	var attributes []any
	for _, nested := range block.Body.Blocks {
		if nested.Type != "attribute" {
			continue
		}
		if len(nested.Labels) != 1 {
			return fmt.Errorf("%w: composite attribute requires one name label", ErrHCL)
		}
		values, _, err := attributesAndNested(nested.Body, data, variables, hclBlockEvaluationSymbols(nested, data, variables, symbols.variables(parent)))
		if err != nil {
			return err
		}
		values["name"] = nested.Labels[0]
		values["ordinal"] = len(attributes) + 1
		for key, value := range values {
			resolved, err := resolveHCLReferences(value, key, dependencies)
			if err != nil {
				return err
			}
			values[key] = resolved
		}
		attributes = append(attributes, values)
	}
	if len(attributes) > 0 {
		spec["attributes"] = attributes
	}
	return nil
}

func lowerNativeConstraint(kind schema.Kind, name schema.Name, spec map[string]any, dependencies *[]schema.Dependency, symbols *hclSymbolTable) error {
	if _, exists := spec["definition"]; exists {
		return nil
	}
	setHCLDefault(spec, "deferrable", false)
	setHCLDefault(spec, "initially_deferred", false)
	setHCLDefault(spec, "validated", true)
	switch kind {
	case schema.KindPrimaryKey, schema.KindUniqueConstraint:
		columns, _, err := nativeHCLColumns(spec["columns"], dependencies)
		if err != nil {
			return err
		}
		prefix := "PRIMARY KEY"
		if kind == schema.KindUniqueConstraint {
			prefix = "UNIQUE"
			if value, _ := spec["nulls_not_distinct"].(bool); value {
				prefix += " NULLS NOT DISTINCT"
			}
		}
		spec["columns"] = stringSliceAny(columns)
		definition := prefix + " (" + quoteHCLIdentifiers(columns) + ")"
		if raw, ok := spec["include"]; ok {
			included, _, includeErr := nativeHCLColumns(raw, dependencies)
			if includeErr != nil {
				return includeErr
			}
			definition += " INCLUDE (" + quoteHCLIdentifiers(included) + ")"
		}
		delete(spec, "include")
		delete(spec, "nulls_not_distinct")
		spec["definition"] = definition + nativeConstraintTiming(spec)
	case schema.KindCheckConstraint:
		expression, err := nativeHCLExpression(spec["expr"], "check", dependencies)
		if err != nil {
			return err
		}
		delete(spec, "expr")
		spec["definition"] = "CHECK (" + expression + ")" + nativeConstraintTiming(spec)
	case schema.KindForeignKey:
		columns, _, err := nativeHCLColumns(spec["columns"], dependencies)
		if err != nil {
			return err
		}
		refColumns, refs, err := nativeHCLColumns(spec["ref_columns"], dependencies)
		if err != nil {
			return err
		}
		if len(refs) == 0 || refs[0].Parent == "" {
			return fmt.Errorf("%w: foreign_key ref_columns require column references", ErrHCL)
		}
		refTable := symbols.reference(refs[0].Parent)
		if refTable.Kind != schema.KindTable {
			return fmt.Errorf("%w: foreign_key ref_columns must belong to a table", ErrHCL)
		}
		for _, ref := range refs[1:] {
			if ref.Parent != refTable.ID {
				return fmt.Errorf("%w: foreign_key ref_columns must belong to one table", ErrHCL)
			}
		}
		appendHCLDependency(dependencies, schema.Dependency{Target: refTable.ID, Type: schema.DependencyReferences})
		spec["columns"], spec["referenced_columns"] = stringSliceAny(columns), stringSliceAny(refColumns)
		delete(spec, "ref_columns")
		definition := "FOREIGN KEY (" + quoteHCLIdentifiers(columns) + ") REFERENCES " + quoteHCLQualified(refTable) + " (" + quoteHCLIdentifiers(refColumns) + ")"
		if action := nativeHCLKeyword(spec["on_delete"]); action != "" {
			definition += " ON DELETE " + action
		}
		if action := nativeHCLKeyword(spec["on_update"]); action != "" {
			definition += " ON UPDATE " + action
		}
		delete(spec, "on_delete")
		delete(spec, "on_update")
		spec["definition"] = definition + nativeConstraintTiming(spec)
	}
	_ = name
	return nil
}

func nativeConstraintTiming(spec map[string]any) string {
	suffix := ""
	if deferred, _ := spec["initially_deferred"].(bool); deferred {
		suffix = " DEFERRABLE INITIALLY DEFERRED"
	} else if deferrable, _ := spec["deferrable"].(bool); deferrable {
		suffix = " DEFERRABLE"
	}
	if validated, exists := spec["validated"].(bool); exists && !validated {
		suffix += " NOT VALID"
	}
	return suffix
}

func lowerNativeIndex(name schema.Name, block *hclsyntax.Block, spec map[string]any, dependencies *[]schema.Dependency, symbols *hclSymbolTable, data []byte, variables HCLVariables) error {
	if _, exists := spec["definition"]; exists {
		return nil
	}
	parent := symbols.reference(name.Parent)
	if parent.Kind != schema.KindTable && parent.Kind != schema.KindMaterializedView {
		return fmt.Errorf("%w: index must be nested under a table or materialized view", ErrHCL)
	}
	var columns, keys []string
	for _, nested := range block.Body.Blocks {
		if nested.Type != "on" {
			continue
		}
		values, _, err := attributesAndNested(nested.Body, data, variables, hclBlockEvaluationSymbols(nested, data, variables, symbols.variables(name.Parent)))
		if err != nil {
			return err
		}
		key, column, err := nativeHCLIndexKey(values, dependencies)
		if err != nil {
			return err
		}
		keys = append(keys, key)
		if column != "" {
			columns = append(columns, column)
		}
	}
	if len(keys) == 0 {
		var err error
		columns, _, err = nativeHCLColumns(spec["columns"], dependencies)
		if err != nil {
			return err
		}
		for _, column := range columns {
			keys = append(keys, quoteHCLIdentifier(column))
		}
	}
	method := strings.ToLower(nativeHCLKeyword(spec["method"]))
	if method == "" {
		method = "btree"
	}
	unique, _ := spec["unique"].(bool)
	definition := "CREATE "
	if unique {
		definition += "UNIQUE "
	}
	definition += "INDEX " + quoteHCLIdentifier(name.Name) + " ON " + quoteHCLQualified(parent) + " USING " + method + " (" + strings.Join(keys, ", ") + ")"
	if raw, ok := spec["include"]; ok {
		included, _, err := nativeHCLColumns(raw, dependencies)
		if err != nil {
			return err
		}
		definition += " INCLUDE (" + quoteHCLIdentifiers(included) + ")"
	}
	if raw, ok := spec["with"].(map[string]any); ok && len(raw) > 0 {
		definition += " WITH (" + nativeHCLStorageParameters(raw) + ")"
	}
	if tablespace, ok := spec["tablespace"].(string); ok && strings.TrimSpace(tablespace) != "" {
		definition += " TABLESPACE " + quoteHCLIdentifier(tablespace)
	}
	if raw, ok := spec["where"]; ok {
		where, err := nativeHCLExpression(raw, "where", dependencies)
		if err != nil {
			return err
		}
		definition += " WHERE " + where
	}
	spec["columns"] = stringSliceAny(columns)
	delete(spec, "where")
	delete(spec, "include")
	delete(spec, "with")
	delete(spec, "tablespace")
	spec["method"], spec["unique"], spec["valid"], spec["ready"], spec["definition"] = method, unique, true, true, definition
	return nil
}

func nativeHCLIndexKey(values map[string]any, dependencies *[]schema.Dependency) (string, string, error) {
	var part, column string
	if raw, ok := values["column"]; ok {
		ref, ok := referenceFromAny(raw)
		if !ok || ref.Kind != schema.KindColumn {
			return "", "", fmt.Errorf("%w: index on.column must be a column reference", ErrHCL)
		}
		appendHCLDependency(dependencies, schema.Dependency{Target: ref.ID, Type: schema.DependencyReferences})
		part, column = quoteHCLIdentifier(ref.Name), ref.Name
	} else if raw, ok := values["expr"]; ok {
		expression, err := nativeHCLExpression(raw, "index key", dependencies)
		if err != nil {
			return "", "", err
		}
		part = "(" + expression + ")"
	} else {
		return "", "", fmt.Errorf("%w: index on block requires column or expr", ErrHCL)
	}
	if opclass, ok := values["opclass"].(string); ok && strings.TrimSpace(opclass) != "" {
		part += " " + strings.TrimSpace(opclass)
	}
	if order := nativeHCLKeyword(values["order"]); order != "" {
		if order != "ASC" && order != "DESC" {
			return "", "", fmt.Errorf("%w: index order must be asc or desc", ErrHCL)
		}
		part += " " + order
	}
	if nulls := nativeHCLKeyword(values["nulls"]); nulls != "" {
		if nulls != "FIRST" && nulls != "LAST" {
			return "", "", fmt.Errorf("%w: index nulls must be first or last", ErrHCL)
		}
		part += " NULLS " + nulls
	}
	return part, column, nil
}

func nativeHCLStorageParameters(values map[string]any) string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		parts = append(parts, key+"="+fmt.Sprint(values[key]))
	}
	return strings.Join(parts, ", ")
}

func lowerNativeRoutine(kind schema.Kind, identity schema.Name, spec map[string]any) error {
	baseName := strings.TrimSpace(stringValueHCL(spec["name"]))
	if baseName == "" {
		baseName = identity.Name
		if index := strings.IndexByte(baseName, '('); index >= 0 {
			baseName = baseName[:index]
		}
		spec["name"] = baseName
	}
	arguments := stringValueHCL(spec["arguments"])
	identityArguments := stringValueHCL(spec["identity_arguments"])
	if identityArguments == "" {
		if start := strings.IndexByte(identity.Name, '('); start >= 0 && strings.HasSuffix(identity.Name, ")") {
			identityArguments = identity.Name[start+1 : len(identity.Name)-1]
		}
		spec["identity_arguments"] = identityArguments
	}
	if _, ok := spec["arguments"]; !ok {
		spec["arguments"] = arguments
	}
	setHCLDefault(spec, "result", "")
	setHCLDefault(spec, "returns_set", false)
	setHCLDefault(spec, "language", "sql")
	setHCLDefault(spec, "volatility", "v")
	setHCLDefault(spec, "strict", false)
	setHCLDefault(spec, "security_definer", false)
	setHCLDefault(spec, "leakproof", false)
	setHCLDefault(spec, "parallel", "u")
	setHCLDefault(spec, "cost", float64(100))
	setHCLDefault(spec, "rows", float64(0))
	setHCLDefault(spec, "configuration", []any{})
	body, hasBody := spec["body"].(string)
	if !hasBody {
		return nil
	}
	delete(spec, "body")
	keyword := "FUNCTION"
	if kind == schema.KindProcedure {
		keyword = "PROCEDURE"
	}
	definition := "CREATE " + keyword + " " + quoteHCLIdentifier(identity.Schema) + "." + quoteHCLIdentifier(baseName) + "(" + arguments + ")"
	if kind == schema.KindFunction {
		result := stringValueHCL(spec["result"])
		if result == "" {
			return fmt.Errorf("%w: function body requires result", ErrHCL)
		}
		definition += " RETURNS "
		if value, _ := spec["returns_set"].(bool); value {
			definition += "SETOF "
		}
		definition += result
	}
	definition += " LANGUAGE " + stringValueHCL(spec["language"])
	definition += " AS $autosql$\n" + strings.TrimSpace(body) + "\n$autosql$"
	spec["definition"] = definition
	return nil
}

func lowerNativeTrigger(name schema.Name, spec map[string]any, dependencies *[]schema.Dependency, symbols *hclSymbolTable) error {
	rawFunction, hasFunction := spec["function"]
	if !hasFunction {
		setHCLDefault(spec, "enabled", "O")
		setHCLDefault(spec, "columns", []any{})
		return nil
	}
	function, ok := referenceFromAny(rawFunction)
	if !ok || function.Kind != schema.KindFunction {
		return fmt.Errorf("%w: trigger function must be a function reference", ErrHCL)
	}
	appendHCLDependency(dependencies, schema.Dependency{Target: function.ID, Type: schema.DependencyReferences})
	delete(spec, "function")
	parent := symbols.reference(name.Parent)
	if parent.Kind != schema.KindTable && parent.Kind != schema.KindView {
		return fmt.Errorf("%w: trigger must be nested under a table or view", ErrHCL)
	}
	timing := nativeHCLKeyword(spec["timing"])
	if timing == "" {
		timing = "BEFORE"
	}
	events, err := nativeHCLStringList(spec["events"])
	if err != nil || len(events) == 0 {
		return fmt.Errorf("%w: trigger events must be a non-empty string list", ErrHCL)
	}
	for index := range events {
		events[index] = nativeHCLKeyword(events[index])
	}
	base := function.Name
	if index := strings.IndexByte(base, '('); index >= 0 {
		base = base[:index]
	}
	definition := "CREATE TRIGGER " + quoteHCLIdentifier(name.Name) + " " + timing + " " + strings.Join(events, " OR ") + " ON " + quoteHCLQualified(parent)
	level := nativeHCLKeyword(spec["for_each"])
	if level == "" {
		level = "ROW"
	}
	definition += " FOR EACH " + level
	if raw, ok := spec["when"]; ok {
		when, expressionErr := nativeHCLExpression(raw, "trigger when", dependencies)
		if expressionErr != nil {
			return expressionErr
		}
		definition += " WHEN (" + when + ")"
	}
	definition += " EXECUTE FUNCTION " + quoteHCLIdentifier(function.Schema) + "." + quoteHCLIdentifier(base) + "()"
	for _, key := range []string{"timing", "events", "for_each", "when"} {
		delete(spec, key)
	}
	setHCLDefault(spec, "enabled", "O")
	setHCLDefault(spec, "columns", []any{})
	spec["definition"] = definition
	return nil
}

func lowerNativePolicy(spec map[string]any, dependencies *[]schema.Dependency) error {
	command := strings.ToLower(stringValueHCL(spec["command"]))
	if canonical, ok := map[string]string{"all": "*", "select": "r", "insert": "a", "update": "w", "delete": "d"}[command]; ok {
		spec["command"] = canonical
	}
	setHCLDefault(spec, "command", "*")
	setHCLDefault(spec, "permissive", true)
	if raw, exists := spec["roles"]; exists {
		items, ok := raw.([]any)
		if !ok || len(items) == 0 {
			return fmt.Errorf("%w: policy roles must be a non-empty role reference list", ErrHCL)
		}
		roles := make([]any, 0, len(items))
		for _, item := range items {
			if text, ok := item.(string); ok && strings.EqualFold(text, "public") {
				roles = append(roles, "public")
				continue
			}
			ref, ok := referenceFromAny(item)
			if !ok || ref.Kind != schema.KindRole {
				return fmt.Errorf("%w: policy roles must contain role references or public", ErrHCL)
			}
			roles = append(roles, ref.Name)
			appendHCLDependency(dependencies, schema.Dependency{Target: ref.ID, Type: schema.DependencyReferences})
		}
		spec["roles"] = roles
	}
	for _, key := range []string{"using", "check"} {
		if raw, ok := spec[key]; ok {
			expression, err := nativeHCLExpression(raw, key, dependencies)
			if err != nil {
				return err
			}
			spec[key] = expression
		}
	}
	return nil
}

func lowerNativeRole(spec map[string]any) {
	setHCLDefault(spec, "superuser", false)
	setHCLDefault(spec, "inherit", true)
	setHCLDefault(spec, "create_role", false)
	setHCLDefault(spec, "create_database", false)
	setHCLDefault(spec, "login", false)
	setHCLDefault(spec, "replication", false)
	setHCLDefault(spec, "bypass_rls", false)
	setHCLDefault(spec, "connection_limit", float64(-1))
	setHCLDefault(spec, "configuration", []any{})
}

func lowerNativeRoleReferences(spec map[string]any, dependencies *[]schema.Dependency, keys ...string) error {
	for _, key := range keys {
		raw, exists := spec[key]
		if !exists {
			continue
		}
		if text, ok := raw.(string); ok && (strings.EqualFold(text, "public") || strings.EqualFold(text, "postgres")) {
			spec[key] = strings.ToLower(text)
			continue
		}
		ref, ok := referenceFromAny(raw)
		if !ok || ref.Kind != schema.KindRole {
			return fmt.Errorf("%w: %s must be a role reference", ErrHCL, key)
		}
		spec[key] = ref.Name
		appendHCLDependency(dependencies, schema.Dependency{Target: ref.ID, Type: schema.DependencyReferences})
	}
	return nil
}

func lowerNativeGrant(spec map[string]any, dependencies *[]schema.Dependency) error {
	rawTarget, ok := spec["target"]
	if !ok {
		rawTarget = spec["object"]
	}
	target, ok := referenceFromAny(rawTarget)
	if !ok || target.Kind == schema.KindRole {
		return fmt.Errorf("%w: grant target must be a database object reference", ErrHCL)
	}
	appendHCLDependency(dependencies, schema.Dependency{Target: target.ID, Type: schema.DependencyReferences})
	delete(spec, "target")
	delete(spec, "object")
	if err := lowerNativeRoleReferences(spec, dependencies, "grantee", "grantor"); err != nil {
		return err
	}
	setHCLDefault(spec, "grantable", false)
	return nil
}

func lowerNativeDefaultPrivilege(spec map[string]any, dependencies *[]schema.Dependency) error {
	if err := lowerNativeRoleReferences(spec, dependencies, "owner", "grantee"); err != nil {
		return err
	}
	if raw, ok := spec["in_schema"]; ok {
		ref, ok := referenceFromAny(raw)
		if !ok || ref.Kind != schema.KindSchema {
			return fmt.Errorf("%w: default privilege schema must be a schema reference", ErrHCL)
		}
		spec["schema"] = ref.Name
		appendHCLDependency(dependencies, schema.Dependency{Target: ref.ID, Type: schema.DependencyReferences})
		delete(spec, "in_schema")
	}
	setHCLDefault(spec, "grantable", false)
	return nil
}

func nativeHCLStringList(value any) ([]string, error) {
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("not a list")
	}
	result := make([]string, len(items))
	for index, item := range items {
		text, ok := item.(string)
		if !ok || strings.TrimSpace(text) == "" {
			return nil, fmt.Errorf("not a string list")
		}
		result[index] = text
	}
	return result, nil
}

func stringValueHCL(value any) string {
	text, _ := value.(string)
	return strings.TrimSpace(text)
}

func nativeHCLColumns(value any, dependencies *[]schema.Dependency) ([]string, []hclReference, error) {
	items, ok := value.([]any)
	if !ok || len(items) == 0 {
		return nil, nil, fmt.Errorf("%w: columns must be a non-empty reference list", ErrHCL)
	}
	columns := make([]string, len(items))
	refs := make([]hclReference, len(items))
	for index, item := range items {
		ref, ok := referenceFromAny(item)
		if !ok || ref.Kind != schema.KindColumn {
			return nil, nil, fmt.Errorf("%w: columns must contain only column references", ErrHCL)
		}
		columns[index], refs[index] = ref.Name, ref
		appendHCLDependency(dependencies, schema.Dependency{Target: ref.ID, Type: schema.DependencyReferences})
	}
	return columns, refs, nil
}

func nativeHCLExpression(value any, attribute string, dependencies *[]schema.Dependency) (string, error) {
	resolved, err := resolveHCLReferences(value, attribute, dependencies)
	if err != nil {
		return "", err
	}
	expression, ok := resolved.(string)
	if !ok || strings.TrimSpace(expression) == "" {
		return "", fmt.Errorf("%w: %s expression is required", ErrHCL, attribute)
	}
	return expression, nil
}

func nativeHCLKeyword(value any) string {
	text, _ := value.(string)
	return strings.ToUpper(strings.ReplaceAll(strings.TrimSpace(text), "_", " "))
}

func quoteHCLIdentifier(value string) string {
	return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
}

func quoteHCLIdentifiers(values []string) string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = quoteHCLIdentifier(value)
	}
	return strings.Join(quoted, ", ")
}

func quoteHCLQualified(ref hclReference) string {
	if ref.Schema == "" {
		return quoteHCLIdentifier(ref.Name)
	}
	return quoteHCLIdentifier(ref.Schema) + "." + quoteHCLIdentifier(ref.Name)
}

func stringSliceAny(values []string) []any {
	result := make([]any, len(values))
	for index, value := range values {
		result[index] = value
	}
	return result
}
