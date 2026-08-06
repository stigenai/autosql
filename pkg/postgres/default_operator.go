package postgres

import (
	"errors"
	"fmt"
	"math/big"
	"strconv"
	"strings"
)

// canonicalOperatorDefault returns a stable spelling only for the deliberately
// bounded arithmetic vocabulary. It is used by normalization after the full
// PostgreSQL parser has proved that the input is exactly one expression.
func canonicalOperatorDefault(expr defaultExpression) (string, error) {
	if !containsDefaultOperator(expr) {
		return "", errors.New("default expression does not contain an operator")
	}
	if _, err := analyzeNumericDefaultExpression(expr); err != nil {
		return "", errors.New("default operator operands are outside the bounded numeric grammar")
	}
	return renderCanonicalDefaultExpression(expr)
}

func containsDefaultOperator(expr defaultExpression) bool {
	switch expr.Kind {
	case defaultExpressionOperator:
		return true
	case defaultExpressionCast:
		return expr.Cast != nil && containsDefaultOperator(expr.Cast.Expression)
	case defaultExpressionFunction:
		if expr.Function != nil {
			for _, argument := range expr.Function.Arguments {
				if containsDefaultOperator(argument) {
					return true
				}
			}
		}
	}
	return false
}

type defaultNumericType uint8

const (
	defaultNumericInvalid defaultNumericType = iota
	defaultNumericSmallint
	defaultNumericInteger
	defaultNumericBigint
	defaultNumericNumeric
	defaultNumericReal
	defaultNumericDouble
)

type defaultNumericAnalysis struct {
	Type     defaultNumericType
	Constant *big.Rat
	Minimum  *big.Rat
	Maximum  *big.Rat
}

func boundedNumericDefaultExpression(expr defaultExpression) bool {
	_, err := analyzeNumericDefaultExpression(expr)
	return err == nil
}

func analyzeNumericDefaultExpression(expr defaultExpression) (defaultNumericAnalysis, error) {
	switch expr.Kind {
	case defaultExpressionLiteral:
		if expr.Literal == nil || expr.Literal.Kind != defaultLiteralInteger && expr.Literal.Kind != defaultLiteralFloat || !canonicalNumericDefault.MatchString(expr.Literal.Text) || expr.Literal.Text == "-0" {
			return defaultNumericAnalysis{}, errors.New("numeric operand is not a canonical finite literal")
		}
		value, ok := new(big.Rat).SetString(expr.Literal.Text)
		if !ok {
			return defaultNumericAnalysis{}, errors.New("numeric literal cannot be represented")
		}
		typ := defaultNumericNumeric
		if !strings.Contains(expr.Literal.Text, ".") {
			integer := value.Num()
			switch {
			case integer.IsInt64() && integer.Cmp(big.NewInt(-2147483648)) >= 0 && integer.Cmp(big.NewInt(2147483647)) <= 0:
				typ = defaultNumericInteger
			case integer.IsInt64():
				typ = defaultNumericBigint
			}
		}
		return defaultNumericAnalysis{Type: typ, Constant: value, Minimum: new(big.Rat).Set(value), Maximum: new(big.Rat).Set(value)}, nil
	case defaultExpressionCast:
		if expr.Cast == nil {
			return defaultNumericAnalysis{}, errors.New("numeric cast is empty")
		}
		typ, ok := coreDefaultCastType(expr.Cast.Type)
		if !ok || typ.array || !isNumericCoreType(typ) {
			return defaultNumericAnalysis{}, errors.New("numeric cast target is unsupported")
		}
		inner, err := analyzeNumericDefaultExpression(expr.Cast.Expression)
		if err != nil {
			return defaultNumericAnalysis{}, err
		}
		target := numericTypeFromCore(typ)
		converted, err := convertDefaultNumericConstantToCore(inner.Constant, typ)
		if err != nil {
			return defaultNumericAnalysis{}, fmt.Errorf("numeric cast: %w", err)
		}
		minimum, err := convertDefaultNumericConstantToCore(inner.Minimum, typ)
		if err != nil {
			return defaultNumericAnalysis{}, fmt.Errorf("numeric cast minimum: %w", err)
		}
		maximum, err := convertDefaultNumericConstantToCore(inner.Maximum, typ)
		if err != nil {
			return defaultNumericAnalysis{}, fmt.Errorf("numeric cast maximum: %w", err)
		}
		return defaultNumericAnalysis{Type: target, Constant: converted, Minimum: minimum, Maximum: maximum}, nil
	case defaultExpressionFunction:
		if !boundedExtractEpoch(expr.Function) {
			return defaultNumericAnalysis{}, errors.New("numeric function is not allowlisted")
		}
		// PostgreSQL timestamps range to roughly +/- 10^13 Unix seconds. This
		// deliberately wider bound covers every supported server version.
		minimum, _ := new(big.Rat).SetString("-10000000000000")
		maximum, _ := new(big.Rat).SetString("10000000000000")
		return defaultNumericAnalysis{Type: defaultNumericNumeric, Minimum: minimum, Maximum: maximum}, nil
	case defaultExpressionOperator:
		if expr.Operator == nil || !isBoundedDefaultOperator(expr.Operator.Name) {
			return defaultNumericAnalysis{}, errors.New("numeric operator is not allowlisted")
		}
		right, err := analyzeNumericDefaultExpression(expr.Operator.Right)
		if err != nil {
			return defaultNumericAnalysis{}, fmt.Errorf("right operand: %w", err)
		}
		if expr.Operator.Left == nil {
			if expr.Operator.Name != "+" && expr.Operator.Name != "-" {
				return defaultNumericAnalysis{}, errors.New("numeric unary operator is unsupported")
			}
			if expr.Operator.Name == "-" && right.Constant != nil {
				right.Constant = new(big.Rat).Neg(right.Constant)
			}
			if expr.Operator.Name == "-" {
				right.Minimum, right.Maximum = negateDefaultNumericRange(right.Minimum, right.Maximum)
			}
			if right.Type == defaultNumericReal || right.Type == defaultNumericDouble {
				right, err = quantizeDefaultNumericAnalysis(right)
				if err != nil {
					return defaultNumericAnalysis{}, fmt.Errorf("unary result: %w", err)
				}
			}
			if err := defaultNumericConstantFits(right.Constant, right.Type); err != nil {
				return defaultNumericAnalysis{}, fmt.Errorf("unary result: %w", err)
			}
			return right, nil
		}
		left, err := analyzeNumericDefaultExpression(*expr.Operator.Left)
		if err != nil {
			return defaultNumericAnalysis{}, fmt.Errorf("left operand: %w", err)
		}
		resultType := promoteDefaultNumericTypes(left.Type, right.Type)
		if resultType == defaultNumericInvalid {
			return defaultNumericAnalysis{}, errors.New("numeric operand types have no bounded PostgreSQL operator")
		}
		if expr.Operator.Name == "%" && (resultType == defaultNumericReal || resultType == defaultNumericDouble) {
			return defaultNumericAnalysis{}, errors.New("modulo does not support real or double precision operands")
		}
		if (expr.Operator.Name == "/" || expr.Operator.Name == "%") && right.Constant != nil && right.Constant.Sign() == 0 {
			return defaultNumericAnalysis{}, errors.New("division or modulo divisor evaluates to zero")
		}
		if (expr.Operator.Name == "/" || expr.Operator.Name == "%") && right.Constant == nil && right.Minimum != nil && right.Maximum != nil && right.Minimum.Sign() <= 0 && right.Maximum.Sign() >= 0 {
			return defaultNumericAnalysis{}, errors.New("dynamic division or modulo divisor range includes zero")
		}
		constant, err := evaluateDefaultNumericOperator(expr.Operator.Name, resultType, left.Constant, right.Constant)
		if err != nil {
			return defaultNumericAnalysis{}, err
		}
		minimum, maximum, err := evaluateDefaultNumericRange(expr.Operator.Name, resultType, left, right)
		if err != nil {
			return defaultNumericAnalysis{}, err
		}
		result := defaultNumericAnalysis{Type: resultType, Constant: constant, Minimum: minimum, Maximum: maximum}
		if resultType == defaultNumericReal || resultType == defaultNumericDouble {
			result, err = quantizeDefaultNumericAnalysis(result)
			if err != nil {
				return defaultNumericAnalysis{}, fmt.Errorf("operator result: %w", err)
			}
		}
		if err := defaultNumericConstantFits(result.Constant, resultType); err != nil {
			return defaultNumericAnalysis{}, fmt.Errorf("operator result: %w", err)
		}
		if err := defaultNumericRangeFits(result.Minimum, result.Maximum, resultType); err != nil {
			return defaultNumericAnalysis{}, fmt.Errorf("operator result: %w", err)
		}
		return result, nil
	default:
		return defaultNumericAnalysis{}, errors.New("expression is not numeric")
	}
}

func boundedExtractEpoch(function *defaultFunction) bool {
	if function == nil || !function.SQLSyntax || strings.Join(function.Name.Parts, ".") != "pg_catalog.extract" || len(function.Arguments) != 2 {
		return false
	}
	field, source := function.Arguments[0], function.Arguments[1]
	return field.Kind == defaultExpressionLiteral && field.Literal != nil && field.Literal.Kind == defaultLiteralString && strings.EqualFold(field.Literal.Text, "epoch") && boundedTemporalDefaultExpression(source)
}

func boundedTemporalDefaultExpression(expr defaultExpression) bool {
	if expr.Kind != defaultExpressionFunction || expr.Function == nil || len(expr.Function.Arguments) != 0 {
		return false
	}
	switch strings.Join(expr.Function.Name.Parts, ".") {
	case "now", "transaction_timestamp", "current_timestamp", "current_timestamp_n", "localtimestamp", "localtimestamp_n":
		return expr.Function.Precision == nil || *expr.Function.Precision >= 0 && *expr.Function.Precision <= 6
	default:
		return false
	}
}

func isNumericCoreType(typ coreColumnType) bool {
	switch typ.base {
	case "smallint", "integer", "bigint", "real", "double precision", "numeric":
		return true
	default:
		return false
	}
}

func numericTypeFromCore(typ coreColumnType) defaultNumericType {
	switch typ.base {
	case "smallint":
		return defaultNumericSmallint
	case "integer":
		return defaultNumericInteger
	case "bigint":
		return defaultNumericBigint
	case "numeric":
		return defaultNumericNumeric
	case "real":
		return defaultNumericReal
	case "double precision":
		return defaultNumericDouble
	default:
		return defaultNumericInvalid
	}
}

func promoteDefaultNumericTypes(left, right defaultNumericType) defaultNumericType {
	if left == defaultNumericInvalid || right == defaultNumericInvalid {
		return defaultNumericInvalid
	}
	if left == defaultNumericDouble || right == defaultNumericDouble || left == defaultNumericReal && right != defaultNumericReal || right == defaultNumericReal && left != defaultNumericReal {
		return defaultNumericDouble
	}
	if left == defaultNumericReal && right == defaultNumericReal {
		return defaultNumericReal
	}
	if left == defaultNumericNumeric || right == defaultNumericNumeric {
		return defaultNumericNumeric
	}
	if left == defaultNumericBigint || right == defaultNumericBigint {
		return defaultNumericBigint
	}
	if left == defaultNumericInteger || right == defaultNumericInteger {
		return defaultNumericInteger
	}
	return defaultNumericSmallint
}

func negateDefaultNumericRange(minimum, maximum *big.Rat) (*big.Rat, *big.Rat) {
	if minimum == nil || maximum == nil {
		return nil, nil
	}
	return new(big.Rat).Neg(maximum), new(big.Rat).Neg(minimum)
}

func quantizeDefaultNumericAnalysis(value defaultNumericAnalysis) (defaultNumericAnalysis, error) {
	var err error
	value.Constant, err = convertDefaultNumericConstant(value.Constant, value.Type)
	if err != nil {
		return defaultNumericAnalysis{}, err
	}
	value.Minimum, err = convertDefaultNumericConstant(value.Minimum, value.Type)
	if err != nil {
		return defaultNumericAnalysis{}, err
	}
	value.Maximum, err = convertDefaultNumericConstant(value.Maximum, value.Type)
	if err != nil {
		return defaultNumericAnalysis{}, err
	}
	return value, nil
}

func evaluateDefaultNumericRange(operator string, typ defaultNumericType, left, right defaultNumericAnalysis) (*big.Rat, *big.Rat, error) {
	if left.Minimum == nil || left.Maximum == nil || right.Minimum == nil || right.Maximum == nil {
		return nil, nil, nil
	}
	if operator == "%" {
		if left.Constant != nil && right.Constant != nil {
			value, err := evaluateDefaultNumericOperator(operator, typ, left.Constant, right.Constant)
			if err != nil {
				return nil, nil, err
			}
			return new(big.Rat).Set(value), new(big.Rat).Set(value), nil
		}
		bound := maxAbsDefaultNumeric(right.Minimum, right.Maximum)
		return new(big.Rat).Neg(bound), new(big.Rat).Set(bound), nil
	}
	if operator == "/" && right.Minimum.Sign() <= 0 && right.Maximum.Sign() >= 0 {
		return nil, nil, errors.New("dynamic division divisor range includes zero")
	}
	values := make([]*big.Rat, 0, 4)
	for _, lhs := range []*big.Rat{left.Minimum, left.Maximum} {
		for _, rhs := range []*big.Rat{right.Minimum, right.Maximum} {
			value, err := evaluateDefaultNumericOperator(operator, typ, lhs, rhs)
			if err != nil {
				return nil, nil, err
			}
			values = append(values, value)
		}
	}
	minimum, maximum := new(big.Rat).Set(values[0]), new(big.Rat).Set(values[0])
	for _, value := range values[1:] {
		if value.Cmp(minimum) < 0 {
			minimum.Set(value)
		}
		if value.Cmp(maximum) > 0 {
			maximum.Set(value)
		}
	}
	return minimum, maximum, nil
}

func maxAbsDefaultNumeric(left, right *big.Rat) *big.Rat {
	left = new(big.Rat).Abs(new(big.Rat).Set(left))
	right = new(big.Rat).Abs(new(big.Rat).Set(right))
	if left.Cmp(right) >= 0 {
		return left
	}
	return right
}

func convertDefaultNumericConstant(value *big.Rat, target defaultNumericType) (*big.Rat, error) {
	if value == nil {
		return nil, nil
	}
	converted := new(big.Rat).Set(value)
	if target == defaultNumericSmallint || target == defaultNumericInteger || target == defaultNumericBigint {
		converted.SetInt(roundDefaultNumericInteger(value))
	} else if target == defaultNumericReal || target == defaultNumericDouble {
		bits := 64
		if target == defaultNumericReal {
			bits = 32
		}
		parsed, err := strconv.ParseFloat(value.FloatString(100), bits)
		if err != nil {
			return nil, errors.New("floating value is outside PostgreSQL type bounds")
		}
		if target == defaultNumericReal {
			parsed = float64(float32(parsed))
		}
		converted = new(big.Rat).SetFloat64(parsed)
	}
	if err := defaultNumericConstantFits(converted, target); err != nil {
		return nil, err
	}
	return converted, nil
}

func convertDefaultNumericConstantToCore(value *big.Rat, target coreColumnType) (*big.Rat, error) {
	converted, err := convertDefaultNumericConstant(value, numericTypeFromCore(target))
	if err != nil || converted == nil || target.base != "numeric" || target.modifier == "" {
		return converted, err
	}
	parts := strings.Split(target.modifier, ",")
	precision, parseErr := strconv.Atoi(parts[0])
	if parseErr != nil {
		return nil, errors.New("numeric precision is invalid")
	}
	scale := 0
	if len(parts) == 2 {
		scale, parseErr = strconv.Atoi(parts[1])
		if parseErr != nil {
			return nil, errors.New("numeric scale is invalid")
		}
	}
	factor := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(scale)), nil)
	scaled := new(big.Rat).Mul(converted, new(big.Rat).SetInt(factor))
	converted = new(big.Rat).SetFrac(roundDefaultNumericInteger(scaled), factor)
	limit := new(big.Int).Exp(big.NewInt(10), big.NewInt(int64(precision-scale)), nil)
	if new(big.Rat).Abs(new(big.Rat).Set(converted)).Cmp(new(big.Rat).SetInt(limit)) >= 0 {
		return nil, errors.New("numeric value is outside PostgreSQL precision and scale")
	}
	return converted, nil
}

func roundDefaultNumericInteger(value *big.Rat) *big.Int {
	quotient := new(big.Int).Quo(value.Num(), value.Denom())
	remainder := new(big.Int).Rem(value.Num(), value.Denom())
	if new(big.Int).Lsh(new(big.Int).Abs(remainder), 1).Cmp(value.Denom()) >= 0 {
		if value.Sign() < 0 {
			quotient.Sub(quotient, big.NewInt(1))
		} else {
			quotient.Add(quotient, big.NewInt(1))
		}
	}
	return quotient
}

func evaluateDefaultNumericOperator(operator string, typ defaultNumericType, left, right *big.Rat) (*big.Rat, error) {
	if left == nil || right == nil {
		return nil, nil
	}
	integer := typ == defaultNumericSmallint || typ == defaultNumericInteger || typ == defaultNumericBigint
	switch operator {
	case "+":
		return new(big.Rat).Add(left, right), nil
	case "-":
		return new(big.Rat).Sub(left, right), nil
	case "*":
		return new(big.Rat).Mul(left, right), nil
	case "/":
		if right.Sign() == 0 {
			return nil, errors.New("division divisor evaluates to zero")
		}
		if integer {
			return new(big.Rat).SetInt(new(big.Int).Quo(left.Num(), right.Num())), nil
		}
		return new(big.Rat).Quo(left, right), nil
	case "%":
		if right.Sign() == 0 {
			return nil, errors.New("modulo divisor evaluates to zero")
		}
		if integer {
			minimum := map[defaultNumericType]*big.Int{
				defaultNumericSmallint: big.NewInt(-32768),
				defaultNumericInteger:  big.NewInt(-2147483648),
				defaultNumericBigint:   new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 63)),
			}[typ]
			if left.IsInt() && right.IsInt() && left.Num().Cmp(minimum) == 0 && right.Num().Cmp(big.NewInt(-1)) == 0 {
				return nil, errors.New("integer minimum modulo -1 is rejected as an overflow edge")
			}
			return new(big.Rat).SetInt(new(big.Int).Rem(left.Num(), right.Num())), nil
		}
		ratio := new(big.Rat).Quo(left, right)
		quotient := new(big.Int).Quo(ratio.Num(), ratio.Denom())
		return new(big.Rat).Sub(left, new(big.Rat).Mul(new(big.Rat).SetInt(quotient), right)), nil
	default:
		return nil, errors.New("operator is unsupported")
	}
}

func defaultNumericConstantFits(value *big.Rat, typ defaultNumericType) error {
	if value == nil {
		return nil
	}
	if typ == defaultNumericSmallint || typ == defaultNumericInteger || typ == defaultNumericBigint {
		if !value.IsInt() {
			return errors.New("integer result is fractional")
		}
		bounds := map[defaultNumericType][2]*big.Int{
			defaultNumericSmallint: {big.NewInt(-32768), big.NewInt(32767)},
			defaultNumericInteger:  {big.NewInt(-2147483648), big.NewInt(2147483647)},
			defaultNumericBigint:   {new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), 63)), new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), 63), big.NewInt(1))},
		}[typ]
		if value.Num().Cmp(bounds[0]) < 0 || value.Num().Cmp(bounds[1]) > 0 {
			return errors.New("integer result is outside PostgreSQL type bounds")
		}
	}
	if typ == defaultNumericReal || typ == defaultNumericDouble {
		bits := 64
		if typ == defaultNumericReal {
			bits = 32
		}
		if _, err := strconv.ParseFloat(value.FloatString(80), bits); err != nil {
			return errors.New("floating result is outside PostgreSQL type bounds")
		}
	}
	return nil
}

func defaultNumericRangeFits(minimum, maximum *big.Rat, typ defaultNumericType) error {
	if minimum == nil || maximum == nil {
		return errors.New("dynamic numeric range is not proven")
	}
	if err := defaultNumericConstantFits(minimum, typ); err != nil {
		return err
	}
	return defaultNumericConstantFits(maximum, typ)
}

func renderCanonicalDefaultExpression(expr defaultExpression) (string, error) {
	var rendered string
	switch expr.Kind {
	case defaultExpressionLiteral:
		if expr.Literal == nil {
			return "", errors.New("default literal is empty")
		}
		switch expr.Literal.Kind {
		case defaultLiteralInteger, defaultLiteralFloat:
			rendered = expr.Literal.Text
		case defaultLiteralString:
			rendered = literal(expr.Literal.Text)
		case defaultLiteralBoolean:
			if expr.Literal.Boolean {
				rendered = "true"
			} else {
				rendered = "false"
			}
		case defaultLiteralNull:
			rendered = "NULL"
		default:
			return "", errors.New("default literal kind is invalid")
		}
	case defaultExpressionCast:
		if expr.Cast == nil {
			return "", errors.New("default cast is empty")
		}
		inner, err := renderCanonicalDefaultExpression(expr.Cast.Expression)
		if err != nil {
			return "", err
		}
		typ, err := renderCanonicalDefaultType(expr.Cast.Type)
		if err != nil {
			return "", err
		}
		rendered = inner + "::" + typ
	case defaultExpressionFunction:
		value, err := renderCanonicalDefaultFunction(expr.Function)
		if err != nil {
			return "", err
		}
		rendered = value
	case defaultExpressionOperator:
		if expr.Operator == nil {
			return "", errors.New("default operator is empty")
		}
		if expr.Operator.Left == nil {
			right, err := renderCanonicalDefaultExpression(expr.Operator.Right)
			if err != nil {
				return "", err
			}
			rendered = "(OPERATOR(pg_catalog." + expr.Operator.Name + ") " + right + ")"
		} else {
			left, err := renderCanonicalDefaultExpression(*expr.Operator.Left)
			if err != nil {
				return "", err
			}
			right, err := renderCanonicalDefaultExpression(expr.Operator.Right)
			if err != nil {
				return "", err
			}
			rendered = "(" + left + " OPERATOR(pg_catalog." + expr.Operator.Name + ") " + right + ")"
		}
	default:
		return "", errors.New("default expression kind is not canonically renderable")
	}
	return rendered, nil
}

func renderCanonicalDefaultFunction(function *defaultFunction) (string, error) {
	if function == nil {
		return "", errors.New("default function is empty")
	}
	name := strings.Join(function.Name.Parts, ".")
	switch name {
	case "now", "transaction_timestamp", "current_timestamp":
		if len(function.Arguments) == 0 && function.Precision == nil {
			return "CURRENT_TIMESTAMP", nil
		}
	case "current_timestamp_n":
		if len(function.Arguments) == 0 && function.Precision != nil && *function.Precision >= 0 && *function.Precision <= 6 {
			return fmt.Sprintf("CURRENT_TIMESTAMP(%d)", *function.Precision), nil
		}
	case "localtimestamp":
		if len(function.Arguments) == 0 && function.Precision == nil {
			return "LOCALTIMESTAMP", nil
		}
	case "localtimestamp_n":
		if len(function.Arguments) == 0 && function.Precision != nil && *function.Precision >= 0 && *function.Precision <= 6 {
			return fmt.Sprintf("LOCALTIMESTAMP(%d)", *function.Precision), nil
		}
	case "pg_catalog.extract":
		if boundedExtractEpoch(function) {
			source, err := renderCanonicalDefaultExpression(function.Arguments[1])
			if err != nil {
				return "", err
			}
			return "extract(epoch from " + source + ")", nil
		}
	}
	return "", fmt.Errorf("default function %q is not in the bounded operator allowlist", name)
}

func renderCanonicalDefaultType(ref defaultTypeReference) (string, error) {
	typ, ok := coreDefaultCastType(ref)
	if !ok {
		return "", errors.New("default cast type is outside canonical core grammar")
	}
	value := typ.base
	if typ.modifier != "" {
		value += "(" + typ.modifier + ")"
	}
	if typ.array {
		value += "[]"
	}
	return value, nil
}
