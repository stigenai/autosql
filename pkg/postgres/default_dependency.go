package postgres

import (
	"fmt"
	"strings"

	"autosql/pkg/schema"
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

func validateColumnDefault(column schema.Resource, value string, resources map[string]schema.Resource) error {
	var types, sequences, functions []schema.Resource
	for _, dependency := range column.Dependencies {
		target, ok := resources[dependency.Target]
		if !ok {
			continue
		}
		if dependency.Type == schema.DependencyUses && (target.Kind == schema.KindEnum || target.Kind == schema.KindDomain || target.Kind == schema.KindComposite) {
			types = append(types, target)
		}
		if dependency.Type == schema.DependencyReferences && target.Kind == schema.KindSequence {
			sequences = append(sequences, target)
		}
		if dependency.Type == schema.DependencyReferences && target.Kind == schema.KindFunction {
			functions = append(functions, target)
		}
	}
	if len(functions) != 0 {
		if len(types) != 0 || len(sequences) != 0 || len(functions) != 1 {
			return unsupported(column, "default rejected: managed routine dependency is ambiguous or combined with incompatible dependencies")
		}
		return validateManagedRoutineDefault(column, value, functions[0])
	}
	if len(types) > 1 {
		return unsupported(column, "default rejected: user-defined column type has ambiguous uses dependencies")
	}
	if len(types) == 1 {
		if len(sequences) != 0 {
			return unsupported(column, "default rejected: user-defined type and sequence dependencies cannot be combined")
		}
		return validateUserDefinedDefault(column, value, types[0])
	}
	if len(sequences) != 0 {
		return validateSequenceDefault(column, value, sequences)
	}
	return validateCoreDefault(column, value)
}

func validateManagedRoutineDefault(column schema.Resource, value string, routine schema.Resource) error {
	expression, err := classifyDefaultExpression(value)
	if err != nil {
		return unsupported(column, "default rejected: managed routine expression is outside the bounded grammar")
	}
	for expression.Kind == defaultExpressionCast && expression.Cast != nil {
		expression = expression.Cast.Expression
	}
	if expression.Kind != defaultExpressionFunction || expression.Function == nil || !generatedFunctionReferenceMatches(expression.Function.Name, column.Name.Schema, routine) {
		return unsupported(column, "default rejected: expression does not call the exact managed routine dependency")
	}
	if volatility := stringValue(spec(routine), "volatility"); volatility != "i" && volatility != "s" {
		return unsupported(column, "default rejected: managed routine must be immutable or stable")
	}
	columnType, columnOK := parseCoreColumnType(stringValue(spec(column), "type"))
	resultType, resultOK := parseCoreColumnType(postgresTypeAlias(stringValue(spec(routine), "result")))
	if !columnOK || !resultOK || !coreTypesCompatible(columnType, resultType) {
		return unsupported(column, "default rejected: managed routine result is incompatible with the column type")
	}
	return nil
}

func validateUserDefinedDefault(column schema.Resource, value string, target schema.Resource) error {
	expr, err := classifyDefaultExpression(value)
	if err != nil {
		return unsupported(column, fmt.Sprintf("default rejected for user-defined type %s: %s", target.Name.String(), err.Error()))
	}
	switch target.Kind {
	case schema.KindEnum:
		if !userDefinedCastMatches(expr, target, column.Name.Schema, strings.HasSuffix(stringValue(spec(column), "type"), "[]")) {
			return unsupported(column, fmt.Sprintf("default rejected for enum %s: cast target does not match the exact uses dependency", target.Name.String()))
		}
		literal := expr.Cast.Expression
		if literal.Kind != defaultExpressionLiteral || literal.Literal == nil || literal.Literal.Kind != defaultLiteralString {
			return unsupported(column, fmt.Sprintf("default rejected for enum %s: only literal labels are supported", target.Name.String()))
		}
		for _, label := range stringSlice(spec(target), "values") {
			if literal.Literal.Text == label {
				return nil
			}
		}
		return unsupported(column, fmt.Sprintf("default rejected for enum %s: label is not declared by the dependency", target.Name.String()))
	case schema.KindDomain:
		baseName := postgresTypeAlias(stringValue(spec(target), "base_type"))
		base, ok := parseCoreColumnType(baseName)
		if !ok || base.array {
			return unsupported(column, fmt.Sprintf("default rejected for domain %s: modeled base type %q is unsupported", target.Name.String(), baseName))
		}
		literal := expr
		plain := expr.Kind != defaultExpressionCast
		if expr.Kind == defaultExpressionCast {
			if !userDefinedCastMatches(expr, target, column.Name.Schema, false) {
				return unsupported(column, fmt.Sprintf("default rejected for domain %s: cast target does not match the exact uses dependency", target.Name.String()))
			}
			literal = expr.Cast.Expression
		}
		converted, ok := coreCastLiteralExpression(base, literal)
		if !ok || !coreScalarLiteralAllowed(base, converted) {
			return unsupported(column, fmt.Sprintf("default rejected for domain %s: literal is incompatible with modeled base type %q", target.Name.String(), baseName))
		}
		if plain && !coreLiteralSourceCanonical(base, expr, value) {
			return unsupported(column, fmt.Sprintf("default rejected for domain %s: literal source is not canonical for modeled base type %q", target.Name.String(), baseName))
		}
		return nil
	default:
		return unsupported(column, fmt.Sprintf("default rejected: defaults for user-defined %s types are not supported", target.Kind))
	}
}

func userDefinedCastMatches(expr defaultExpression, target schema.Resource, columnSchema string, array bool) bool {
	if expr.Kind != defaultExpressionCast || expr.Cast == nil || len(expr.Cast.Type.Modifiers) != 0 {
		return false
	}
	if expr.Cast.Type.ArrayBounds != 0 && (!array || expr.Cast.Type.ArrayBounds != 1) || array && expr.Cast.Type.ArrayBounds != 1 {
		return false
	}
	parts := expr.Cast.Type.Name.Parts
	if len(parts) == 2 {
		return parts[0] == target.Name.Schema && parts[1] == target.Name.Name
	}
	return len(parts) == 1 && parts[0] == target.Name.Name && (target.Name.Schema == columnSchema || target.Name.Schema == "public")
}

func qualifyUserDefinedDefaultCast(value string, target schema.Resource) (string, error) {
	parsed, err := pg_query.Parse("SELECT " + value)
	if err != nil || parsed == nil || len(parsed.Stmts) != 1 {
		return "", fmt.Errorf("expression could not be reparsed")
	}
	statement := parsed.Stmts[0].Stmt.GetSelectStmt()
	if statement == nil || len(statement.TargetList) != 1 {
		return "", fmt.Errorf("expression wrapper changed shape")
	}
	targetValue := statement.TargetList[0].GetResTarget()
	if targetValue == nil || targetValue.Val == nil || targetValue.Val.GetTypeCast() == nil || targetValue.Val.GetTypeCast().TypeName == nil {
		return "", fmt.Errorf("expression is not a root cast")
	}
	castType := targetValue.Val.GetTypeCast().TypeName
	castType.Names = []*pg_query.Node{
		{Node: &pg_query.Node_String_{String_: &pg_query.String{Sval: target.Name.Schema}}},
		{Node: &pg_query.Node_String_{String_: &pg_query.String{Sval: target.Name.Name}}},
	}
	rendered, err := pg_query.Deparse(parsed)
	if err != nil {
		return "", fmt.Errorf("qualified cast could not be rendered")
	}
	rendered = strings.TrimSpace(rendered)
	if !strings.HasPrefix(rendered, "SELECT ") {
		return "", fmt.Errorf("qualified cast rendered an unexpected statement")
	}
	return strings.TrimSpace(strings.TrimPrefix(rendered, "SELECT ")), nil
}

func validateSequenceDefault(column schema.Resource, value string, sequences []schema.Resource) error {
	if len(sequences) != 1 {
		return unsupported(column, "default rejected: sequence reference has ambiguous dependencies")
	}
	typ, ok := parseCoreColumnType(stringValue(spec(column), "type"))
	if !ok || typ.array || (typ.base != "smallint" && typ.base != "integer" && typ.base != "bigint") {
		return unsupported(column, fmt.Sprintf("default rejected: nextval is incompatible with normalized type %q", stringValue(spec(column), "type")))
	}
	expr, err := classifyDefaultExpression(value)
	if err != nil {
		return unsupported(column, fmt.Sprintf("default rejected for sequence %s: %s", sequences[0].Name.String(), err.Error()))
	}
	if !nextvalMatchesSequence(expr, sequences[0]) {
		return unsupported(column, fmt.Sprintf("default rejected for sequence %s: nextval(regclass literal) does not match the exact dependency", sequences[0].Name.String()))
	}
	return nil
}

func nextvalMatchesSequence(expr defaultExpression, sequence schema.Resource) bool {
	if expr.Kind != defaultExpressionFunction || expr.Function == nil || len(expr.Function.Arguments) != 1 {
		return false
	}
	name := strings.Join(expr.Function.Name.Parts, ".")
	if name != "nextval" && name != "pg_catalog.nextval" {
		return false
	}
	argument := expr.Function.Arguments[0]
	if argument.Kind != defaultExpressionCast || argument.Cast == nil || argument.Cast.Type.ArrayBounds != 0 || len(argument.Cast.Type.Modifiers) != 0 {
		return false
	}
	castName := strings.Join(argument.Cast.Type.Name.Parts, ".")
	if castName != "regclass" && castName != "pg_catalog.regclass" {
		return false
	}
	literal := argument.Cast.Expression
	if literal.Kind != defaultExpressionLiteral || literal.Literal == nil || literal.Literal.Kind != defaultLiteralString {
		return false
	}
	parts, ok := parseQualifiedRegclass(literal.Literal.Text)
	return ok && len(parts) == 2 && parts[0] == sequence.Name.Schema && parts[1] == sequence.Name.Name
}

func parseQualifiedRegclass(value string) ([]string, bool) {
	var parts []string
	for index := 0; index < len(value); {
		var part strings.Builder
		if value[index] == '"' {
			index++
			closed := false
			for index < len(value) {
				if value[index] != '"' {
					part.WriteByte(value[index])
					index++
					continue
				}
				if index+1 < len(value) && value[index+1] == '"' {
					part.WriteByte('"')
					index += 2
					continue
				}
				index++
				closed = true
				break
			}
			if !closed {
				return nil, false
			}
		} else {
			start := index
			for index < len(value) && value[index] != '.' {
				ch := value[index]
				if !(ch == '_' || ch >= 'a' && ch <= 'z' || ch >= '0' && ch <= '9' && index > start) {
					return nil, false
				}
				index++
			}
			part.WriteString(value[start:index])
		}
		if part.Len() == 0 {
			return nil, false
		}
		parts = append(parts, part.String())
		if index == len(value) {
			break
		}
		if value[index] != '.' || len(parts) >= 2 {
			return nil, false
		}
		index++
	}
	return parts, len(parts) == 2
}
