package postgres

import (
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"net"
	"net/netip"
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
	intervalDefault         = regexp.MustCompile(`^-?(?:(?:[0-9]+) days? )?([0-9]+):([0-9]{2}):([0-9]{2})(?:\.[0-9]{1,6})?$`)
)

func coreDefaultAllowed(typ coreColumnType, expr defaultExpression, source string) bool {
	if typ.array {
		return coreArrayDefaultAllowed(typ, expr)
	}
	if expr.Kind == defaultExpressionOperator {
		return coreOperatorDefaultAllowed(typ, expr, source)
	}
	if expr.Kind == defaultExpressionFunction {
		return coreFunctionDefaultAllowed(typ, expr.Function, source)
	}
	if expr.Kind == defaultExpressionCast && expr.Cast != nil {
		castType, ok := coreDefaultCastType(expr.Cast.Type)
		if !ok || castType.array || !coreTypesCompatible(typ, castType) {
			return false
		}
		if containsDefaultOperator(expr.Cast.Expression) {
			return coreOperatorDefaultAllowed(typ, expr, source)
		}
		literal, ok := coreCastLiteralExpression(castType, expr.Cast.Expression)
		return ok && coreScalarLiteralAllowed(castType, literal) && coreScalarLiteralAllowed(typ, literal)
	}
	return coreScalarLiteralAllowed(typ, expr) && coreLiteralSourceCanonical(typ, expr, source)
}

func coreOperatorDefaultAllowed(typ coreColumnType, expr defaultExpression, source string) bool {
	return validateCoreOperatorDefault(typ, expr, source) == nil
}

func validateCoreOperatorDefault(typ coreColumnType, expr defaultExpression, source string) error {
	if !isNumericCoreType(typ) {
		return errors.New("operator destination is not a numeric core type")
	}
	analysis, err := analyzeNumericDefaultExpression(expr)
	if err != nil {
		return err
	}
	destination := numericTypeFromCore(typ)
	if _, err := convertDefaultNumericConstantToCore(analysis.Constant, typ); err != nil {
		return fmt.Errorf("operator destination conversion: %w", err)
	}
	if analysis.Minimum == nil || analysis.Maximum == nil {
		return errors.New("operator destination range is not proven safe")
	}
	if _, err := convertDefaultNumericConstantToCore(analysis.Minimum, typ); err != nil {
		return fmt.Errorf("operator destination minimum: %w", err)
	}
	if _, err := convertDefaultNumericConstantToCore(analysis.Maximum, typ); err != nil {
		return fmt.Errorf("operator destination maximum: %w", err)
	}
	if destination == defaultNumericInvalid {
		return errors.New("operator destination numeric type is invalid")
	}
	if !canonicalDefaultSource(expr, source) {
		return errors.New("operator expression is not in canonical pg_catalog-bound form")
	}
	return nil
}

func canonicalDefaultSource(expr defaultExpression, source string) bool {
	canonical, err := canonicalOperatorDefault(expr)
	return err == nil && source == canonical
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
		if column.base == "numeric" {
			return true
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
	case "cidr", "inet", "macaddr":
		return literal.Kind == defaultLiteralString && networkLiteralAllowed(typ.base, literal.Text)
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
		return strings.HasPrefix(typ.base, "interval") && literal.Kind == defaultLiteralString && validIntervalDefault(literal.Text)
	}
}

func networkLiteralAllowed(typ, value string) bool {
	switch typ {
	case "cidr":
		prefix, err := netip.ParsePrefix(value)
		return err == nil && prefix.Addr().Zone() == "" && prefix == prefix.Masked()
	case "inet":
		if address, err := netip.ParseAddr(value); err == nil {
			return address.Zone() == ""
		}
		prefix, err := netip.ParsePrefix(value)
		return err == nil && prefix.Addr().Zone() == ""
	case "macaddr":
		address, err := net.ParseMAC(value)
		return err == nil && len(address) == 6
	default:
		return false
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
	integerPart := digits
	fraction := 0
	if dot := strings.IndexByte(digits, '.'); dot >= 0 {
		fraction = len(digits) - dot - 1
		integerPart = digits[:dot]
		digits = digits[:dot] + digits[dot+1:]
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		digits = "0"
	}
	integerPart = strings.TrimLeft(integerPart, "0")
	integerDigits := len(integerPart)
	return fraction <= scale && integerDigits <= precision-scale && len(digits) <= precision
}

func validIntervalDefault(value string) bool {
	match := intervalDefault.FindStringSubmatch(value)
	if match == nil {
		return false
	}
	minutes, minuteErr := strconv.Atoi(match[2])
	seconds, secondErr := strconv.Atoi(match[3])
	return minuteErr == nil && secondErr == nil && minutes < 60 && seconds < 60
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
		return len(expr.Array) > 0 && coreArrayElementsAllowed(elementType, expr.Array)
	}
	if expr.Kind != defaultExpressionCast || expr.Cast == nil {
		return false
	}
	castType, ok := coreDefaultCastType(expr.Cast.Type)
	if !ok || !castType.array || !coreTypesCompatible(typ, castType) {
		return false
	}
	if expr.Cast.Expression.Kind == defaultExpressionArray {
		return coreArrayElementsAllowed(elementType, expr.Cast.Expression.Array)
	}
	if expr.Cast.Expression.Kind != defaultExpressionLiteral || expr.Cast.Expression.Literal == nil || expr.Cast.Expression.Literal.Kind != defaultLiteralString {
		return false
	}
	values, ok := parseCoreArrayLiteral(expr.Cast.Expression.Literal.Text)
	if !ok {
		return false
	}
	for _, value := range values {
		if !coreScalarLiteralAllowed(elementType, arrayLiteralExpression(elementType, value)) {
			return false
		}
	}
	return true
}

func coreArrayElementsAllowed(typ coreColumnType, elements []defaultExpression) bool {
	for _, element := range elements {
		if element.Kind == defaultExpressionArray || !coreArrayElementAllowed(typ, element) {
			return false
		}
	}
	return true
}

func coreArrayElementAllowed(typ coreColumnType, expr defaultExpression) bool {
	if expr.Kind == defaultExpressionCast && expr.Cast != nil {
		castType, ok := coreDefaultCastType(expr.Cast.Type)
		if !ok || castType.array || !coreTypesCompatible(typ, castType) {
			return false
		}
		literal, ok := coreCastLiteralExpression(castType, expr.Cast.Expression)
		return ok && coreScalarLiteralAllowed(castType, literal) && coreScalarLiteralAllowed(typ, literal)
	}
	return coreScalarLiteralAllowed(typ, expr)
}

func coreCastLiteralExpression(typ coreColumnType, expr defaultExpression) (defaultExpression, bool) {
	if expr.Kind != defaultExpressionLiteral || expr.Literal == nil {
		return defaultExpression{}, false
	}
	if expr.Literal.Kind != defaultLiteralString {
		return expr, true
	}
	converted := expr
	converted.Literal = &defaultLiteral{Text: expr.Literal.Text}
	switch typ.base {
	case "smallint", "integer", "bigint":
		converted.Literal.Kind = defaultLiteralInteger
	case "real", "double precision", "numeric":
		converted.Literal.Kind = defaultLiteralFloat
	case "boolean":
		converted.Literal.Kind = defaultLiteralBoolean
		if expr.Literal.Text == "true" {
			converted.Literal.Boolean = true
		} else if expr.Literal.Text != "false" {
			return defaultExpression{}, false
		}
	default:
		converted.Literal.Kind = defaultLiteralString
	}
	return converted, true
}

func arrayLiteralExpression(typ coreColumnType, value string) defaultExpression {
	kind := defaultLiteralString
	switch typ.base {
	case "smallint", "integer", "bigint":
		kind = defaultLiteralInteger
	case "real", "double precision", "numeric":
		kind = defaultLiteralFloat
	case "boolean":
		if value == "true" || value == "t" {
			return defaultExpression{Kind: defaultExpressionLiteral, Literal: &defaultLiteral{Kind: defaultLiteralBoolean, Boolean: true}}
		}
		if value == "false" || value == "f" {
			return defaultExpression{Kind: defaultExpressionLiteral, Literal: &defaultLiteral{Kind: defaultLiteralBoolean}}
		}
	}
	return defaultExpression{Kind: defaultExpressionLiteral, Literal: &defaultLiteral{Kind: kind, Text: value}}
}

// parseCoreArrayLiteral accepts PostgreSQL's one-dimensional literal form and
// deliberately rejects NULL elements, dimensions, and malformed escapes.
func parseCoreArrayLiteral(value string) ([]string, bool) {
	if len(value) < 2 || value[0] != '{' || value[len(value)-1] != '}' {
		return nil, false
	}
	body := value[1 : len(value)-1]
	if body == "" {
		return []string{}, true
	}
	var parts []string
	var element strings.Builder
	quoted, escaped, elementQuoted, quoteClosed := false, false, false, false
	flush := func() bool {
		part := element.String()
		if part == "" && !elementQuoted || !elementQuoted && strings.EqualFold(part, "null") {
			return false
		}
		parts = append(parts, part)
		element.Reset()
		elementQuoted = false
		quoteClosed = false
		return true
	}
	for _, ch := range body {
		if escaped {
			element.WriteRune(ch)
			escaped = false
			continue
		}
		if quoteClosed && ch != ',' {
			return nil, false
		}
		if ch == '\\' {
			escaped = true
			continue
		}
		if ch == '"' {
			if quoted {
				quoted = false
				quoteClosed = true
			} else {
				if element.Len() != 0 || elementQuoted {
					return nil, false
				}
				quoted = true
				elementQuoted = true
			}
			continue
		}
		if !quoted && (ch == '{' || ch == '}') {
			return nil, false
		}
		if !quoted && ch == ',' {
			if !flush() {
				return nil, false
			}
			continue
		}
		element.WriteRune(ch)
	}
	if quoted || escaped || !flush() {
		return nil, false
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
