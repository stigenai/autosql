package postgres

import (
	"encoding/json"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

var (
	canonicalIntegerDefault = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)$`)
	canonicalNumericDefault = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?$`)
	uuidDefault             = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	bitDefault              = regexp.MustCompile(`^[01]+$`)
	intervalDefault         = regexp.MustCompile(`^-?(?:(?:[0-9]+) days? )?[0-9]{2}:[0-9]{2}:[0-9]{2}(?:\.[0-9]{1,6})?$`)
)

func coreDefaultAllowed(typ coreColumnType, expr defaultExpression, source string) bool {
	if typ.array {
		return coreArrayDefaultAllowed(typ, expr)
	}
	if expr.Kind == defaultExpressionFunction {
		return coreFunctionDefaultAllowed(typ, expr.Function, source)
	}
	if expr.Kind == defaultExpressionCast && expr.Cast != nil {
		castType, ok := coreDefaultCastType(expr.Cast.Type)
		if !ok || castType.array || !coreTypesCompatible(typ, castType) {
			return false
		}
		return coreScalarLiteralAllowed(castType, expr.Cast.Expression)
	}
	return coreScalarLiteralAllowed(typ, expr) && coreLiteralSourceCanonical(typ, expr, source)
}

func coreDefaultCastType(ref defaultTypeReference) (coreColumnType, bool) {
	name := strings.Join(ref.Name.Parts, ".")
	name = postgresTypeAlias(name)
	if len(ref.Modifiers) > 0 {
		parts := make([]string, 0, len(ref.Modifiers))
		for _, modifier := range ref.Modifiers {
			if modifier.Literal == nil || modifier.Literal.Kind != defaultLiteralInteger {
				return coreColumnType{}, false
			}
			parts = append(parts, modifier.Literal.Text)
		}
		name += "(" + strings.Join(parts, ",") + ")"
	}
	if ref.ArrayBounds == 1 {
		name += "[]"
	}
	return parseCoreColumnType(name)
}

func coreTypesCompatible(column, cast coreColumnType) bool {
	if column.array != cast.array {
		return false
	}
	if column.base == cast.base {
		if cast.modifier == "" || column.modifier == "" || column.modifier == cast.modifier {
			return true
		}
		if column.base == "character" || column.base == "character varying" || column.base == "bit varying" {
			columnLimit, columnOK := canonicalUnsigned(column.modifier)
			castLimit, castOK := canonicalUnsigned(cast.modifier)
			return columnOK && castOK && castLimit <= columnLimit
		}
		return false
	}
	textual := func(base string) bool { return base == "text" || base == "character" || base == "character varying" }
	numeric := func(base string) bool {
		return base == "smallint" || base == "integer" || base == "bigint" || base == "real" || base == "double precision" || base == "numeric"
	}
	interval := func(base string) bool { return base == "interval" || strings.HasPrefix(base, "interval ") }
	return textual(column.base) && textual(cast.base) || numeric(column.base) && numeric(cast.base) ||
		interval(column.base) && interval(cast.base) ||
		(column.base == "bit" || column.base == "bit varying") && (cast.base == "bit" || cast.base == "bit varying")
}

func coreScalarLiteralAllowed(typ coreColumnType, expr defaultExpression) bool {
	if expr.Kind != defaultExpressionLiteral || expr.Literal == nil || expr.Literal.Kind == defaultLiteralNull {
		return false
	}
	literal := expr.Literal
	switch typ.base {
	case "smallint", "integer", "bigint":
		if literal.Kind != defaultLiteralInteger && literal.Kind != defaultLiteralFloat {
			return false
		}
		bits := map[string]int{"smallint": 16, "integer": 32, "bigint": 64}[typ.base]
		_, err := strconv.ParseInt(literal.Text, 10, bits)
		return canonicalIntegerDefault.MatchString(literal.Text) && literal.Text != "-0" && err == nil
	case "real", "double precision":
		if literal.Kind != defaultLiteralInteger && literal.Kind != defaultLiteralFloat || !canonicalNumericDefault.MatchString(literal.Text) {
			return false
		}
		value, err := strconv.ParseFloat(literal.Text, 64)
		return err == nil && !math.IsInf(value, 0) && !math.IsNaN(value)
	case "numeric":
		return (literal.Kind == defaultLiteralInteger || literal.Kind == defaultLiteralFloat || literal.Kind == defaultLiteralString) && numericDefaultFits(typ, literal.Text)
	case "boolean":
		return literal.Kind == defaultLiteralBoolean
	case "text", "character", "character varying":
		if literal.Kind != defaultLiteralString {
			return false
		}
		if typ.modifier == "" {
			return true
		}
		limit, ok := canonicalUnsigned(typ.modifier)
		return ok && utf8.RuneCountInString(literal.Text) <= limit
	case "bit", "bit varying":
		if literal.Kind != defaultLiteralString || !bitDefault.MatchString(literal.Text) {
			return false
		}
		if typ.modifier == "" {
			return true
		}
		limit, ok := canonicalUnsigned(typ.modifier)
		return ok && (typ.base == "bit varying" && len(literal.Text) <= limit || typ.base == "bit" && len(literal.Text) == limit)
	case "uuid":
		return literal.Kind == defaultLiteralString && uuidDefault.MatchString(literal.Text)
	case "json", "jsonb":
		return literal.Kind == defaultLiteralString && json.Valid([]byte(literal.Text))
	case "date":
		return literal.Kind == defaultLiteralString && parsesTime("2006-01-02", literal.Text)
	case "time":
		return literal.Kind == defaultLiteralString && parsesAnyTime(literal.Text, "15:04:05", "15:04:05.999999")
	case "timetz":
		return literal.Kind == defaultLiteralString && parsesAnyTime(literal.Text, "15:04:05Z07:00", "15:04:05.999999Z07:00")
	case "timestamp":
		return literal.Kind == defaultLiteralString && parsesAnyTime(literal.Text, "2006-01-02 15:04:05", "2006-01-02 15:04:05.999999")
	case "timestamptz":
		return literal.Kind == defaultLiteralString && parsesAnyTime(literal.Text, "2006-01-02 15:04:05Z07:00", "2006-01-02 15:04:05.999999Z07:00")
	default:
		return strings.HasPrefix(typ.base, "interval") && literal.Kind == defaultLiteralString && intervalDefault.MatchString(literal.Text)
	}
}

func coreLiteralSourceCanonical(typ coreColumnType, expr defaultExpression, source string) bool {
	if expr.Literal == nil {
		return false
	}
	switch expr.Literal.Kind {
	case defaultLiteralBoolean:
		return source == "true" && expr.Literal.Boolean || source == "false" && !expr.Literal.Boolean
	case defaultLiteralInteger, defaultLiteralFloat:
		return source == expr.Literal.Text
	case defaultLiteralString:
		return len(source) >= 2 && source[0] == '\'' && source[len(source)-1] == '\''
	default:
		return false
	}
}

func numericDefaultFits(typ coreColumnType, value string) bool {
	if !canonicalNumericDefault.MatchString(value) || value == "-0" {
		return false
	}
	if typ.modifier == "" {
		return true
	}
	parts := strings.Split(typ.modifier, ",")
	precision, _ := strconv.Atoi(parts[0])
	scale := 0
	if len(parts) == 2 {
		scale, _ = strconv.Atoi(parts[1])
	}
	digits := strings.TrimPrefix(value, "-")
	fraction := 0
	if dot := strings.IndexByte(digits, '.'); dot >= 0 {
		fraction = len(digits) - dot - 1
		digits = digits[:dot] + digits[dot+1:]
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		digits = "0"
	}
	return fraction <= scale && len(digits) <= precision
}

func parsesTime(layout, value string) bool {
	_, err := time.Parse(layout, value)
	return err == nil
}

func parsesAnyTime(value string, layouts ...string) bool {
	for _, layout := range layouts {
		if parsesTime(layout, value) {
			return true
		}
	}
	return false
}

func coreArrayDefaultAllowed(typ coreColumnType, expr defaultExpression) bool {
	elementType := typ
	elementType.array = false
	if expr.Kind == defaultExpressionArray {
		for _, element := range expr.Array {
			if element.Kind == defaultExpressionArray || !coreArrayElementAllowed(elementType, element) {
				return false
			}
		}
		return true
	}
	if expr.Kind != defaultExpressionCast || expr.Cast == nil {
		return false
	}
	castType, ok := coreDefaultCastType(expr.Cast.Type)
	if !ok || !castType.array || !coreTypesCompatible(typ, castType) {
		return false
	}
	if expr.Cast.Expression.Kind == defaultExpressionArray {
		return coreArrayDefaultAllowed(castType, expr.Cast.Expression)
	}
	if expr.Cast.Expression.Kind != defaultExpressionLiteral || expr.Cast.Expression.Literal == nil || expr.Cast.Expression.Literal.Kind != defaultLiteralString {
		return false
	}
	values, ok := parseCoreArrayLiteral(expr.Cast.Expression.Literal.Text)
	if !ok {
		return false
	}
	for _, value := range values {
		if !coreScalarLiteralAllowed(elementType, defaultExpression{Kind: defaultExpressionLiteral, Literal: &defaultLiteral{Kind: defaultLiteralString, Text: value}}) {
			return false
		}
	}
	return true
}

func coreArrayElementAllowed(typ coreColumnType, expr defaultExpression) bool {
	if expr.Kind == defaultExpressionCast && expr.Cast != nil {
		castType, ok := coreDefaultCastType(expr.Cast.Type)
		return ok && !castType.array && coreTypesCompatible(typ, castType) && coreScalarLiteralAllowed(castType, expr.Cast.Expression)
	}
	return coreScalarLiteralAllowed(typ, expr)
}

// parseCoreArrayLiteral accepts PostgreSQL's one-dimensional literal form and
// deliberately rejects NULL elements, dimensions, nesting, and escapes.
func parseCoreArrayLiteral(value string) ([]string, bool) {
	if len(value) < 2 || value[0] != '{' || value[len(value)-1] != '}' {
		return nil, false
	}
	body := value[1 : len(value)-1]
	if body == "" {
		return []string{}, true
	}
	if strings.ContainsAny(body, `{}\\"`) {
		return nil, false
	}
	parts := strings.Split(body, ",")
	for _, part := range parts {
		if part == "" || strings.EqualFold(part, "null") {
			return nil, false
		}
	}
	return parts, true
}

func coreFunctionDefaultAllowed(typ coreColumnType, function *defaultFunction, source string) bool {
	if function == nil {
		return false
	}
	name := strings.Join(function.Name.Parts, ".")
	precisionOK := function.Precision == nil || *function.Precision >= 0 && *function.Precision <= 6
	switch name {
	case "current_timestamp", "current_timestamp_n":
		return precisionOK && len(function.Arguments) == 0 && (typ.base == "timestamp" || typ.base == "timestamptz") &&
			(source == "CURRENT_TIMESTAMP" || canonicalTemporalPrecision(source, "CURRENT_TIMESTAMP"))
	case "current_date":
		return len(function.Arguments) == 0 && typ.base == "date" && source == "CURRENT_DATE"
	case "current_time", "current_time_n":
		return precisionOK && len(function.Arguments) == 0 && typ.base == "timetz" && (source == "CURRENT_TIME" || canonicalTemporalPrecision(source, "CURRENT_TIME"))
	case "localtime", "localtime_n":
		return precisionOK && len(function.Arguments) == 0 && typ.base == "time" && (source == "LOCALTIME" || canonicalTemporalPrecision(source, "LOCALTIME"))
	case "localtimestamp", "localtimestamp_n":
		return precisionOK && len(function.Arguments) == 0 && typ.base == "timestamp" && (source == "LOCALTIMESTAMP" || canonicalTemporalPrecision(source, "LOCALTIMESTAMP"))
	case "pg_catalog.gen_random_uuid":
		return len(function.Arguments) == 0 && typ.base == "uuid" && source == "pg_catalog.gen_random_uuid()"
	case "pg_catalog.timezone":
		return typ.base == "timestamp" && source == "pg_catalog.timezone('utc'::text, CURRENT_TIMESTAMP)" && utcTimezoneArguments(function.Arguments)
	default:
		return false
	}
}

func canonicalTemporalPrecision(source, prefix string) bool {
	return len(source) == len(prefix)+3 && strings.HasPrefix(source, prefix+"(") && source[len(source)-1] == ')' && source[len(prefix)+1] >= '0' && source[len(prefix)+1] <= '6'
}

func utcTimezoneArguments(arguments []defaultExpression) bool {
	if len(arguments) != 2 || arguments[0].Kind != defaultExpressionCast || arguments[0].Cast == nil || arguments[1].Kind != defaultExpressionFunction {
		return false
	}
	castType, ok := coreDefaultCastType(arguments[0].Cast.Type)
	return ok && castType.base == "text" && coreScalarLiteralAllowed(castType, arguments[0].Cast.Expression) &&
		arguments[0].Cast.Expression.Literal.Text == "utc" && arguments[1].Function != nil &&
		strings.Join(arguments[1].Function.Name.Parts, ".") == "current_timestamp" && len(arguments[1].Function.Arguments) == 0
}
