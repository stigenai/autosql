package postgres

import (
	"errors"
	"fmt"
	"strings"

	pg_query "github.com/pganalyze/pg_query_go/v6"
)

// defaultExpression is the deliberately small structural vocabulary accepted
// at the boundary between inspected PostgreSQL defaults and rendered SQL. A
// parsed expression is not necessarily approved for a column: callers must
// still apply type-specific policy to the classified form.
type defaultExpression struct {
	Kind      defaultExpressionKind
	Literal   *defaultLiteral
	Cast      *defaultCast
	Function  *defaultFunction
	Operator  *defaultOperator
	Reference *defaultReference
	Array     []defaultExpression
}

type defaultExpressionKind uint8

const (
	defaultExpressionInvalid defaultExpressionKind = iota
	defaultExpressionLiteral
	defaultExpressionCast
	defaultExpressionFunction
	defaultExpressionOperator
	defaultExpressionReference
	defaultExpressionArray
)

type defaultLiteralKind uint8

const (
	defaultLiteralInvalid defaultLiteralKind = iota
	defaultLiteralInteger
	defaultLiteralFloat
	defaultLiteralString
	defaultLiteralBoolean
	defaultLiteralNull
)

type defaultLiteral struct {
	Kind    defaultLiteralKind
	Text    string
	Boolean bool
}

type defaultCast struct {
	Expression defaultExpression
	Type       defaultTypeReference
}

type defaultFunction struct {
	Name      defaultReference
	Arguments []defaultExpression
	Precision *int
	SQLSyntax bool
}

type defaultOperator struct {
	Name  string
	Left  *defaultExpression
	Right defaultExpression
}

type defaultReference struct {
	Parts []string
}

type defaultTypeReference struct {
	Name        defaultReference
	Modifiers   []defaultExpression
	ArrayBounds int
}

const (
	maxDefaultExpressionBytes = 1 << 20
	maxDefaultExpressionDepth = 64
	maxDefaultExpressionNodes = 1024
	maxDefaultFunctionArgs    = 256
	maxDefaultNameParts       = 3
	maxDefaultNamePartBytes   = 63
)

// classifyDefaultExpression parses exactly one expression in a SELECT wrapper
// and rejects every statement feature outside a single unnamed target. The
// recursive classifier fails closed on AST nodes it does not explicitly know.
func classifyDefaultExpression(value string) (defaultExpression, error) {
	if value == "" {
		return defaultExpression{}, errors.New("default expression is empty")
	}
	if len(value) > maxDefaultExpressionBytes {
		return defaultExpression{}, errors.New("default expression exceeds size limit")
	}
	if err := validateDefaultLexicalForm(value); err != nil {
		return defaultExpression{}, err
	}

	parsed, err := pg_query.Parse("SELECT " + value)
	if err != nil || len(parsed.GetStmts()) != 1 {
		return defaultExpression{}, errors.New("default is not one valid PostgreSQL expression")
	}
	raw := parsed.GetStmts()[0]
	if raw == nil || raw.GetStmt() == nil {
		return defaultExpression{}, errors.New("default expression has no statement")
	}
	stmt := raw.GetStmt().GetSelectStmt()
	if !isDefaultExpressionSelect(stmt) {
		return defaultExpression{}, errors.New("default expression wrapper contains unsupported statement features")
	}
	target := stmt.GetTargetList()[0].GetResTarget()
	nodes := 0
	return classifyDefaultNode(target.GetVal(), 0, &nodes)
}

// validateDefaultLexicalForm rejects syntax-level comments, dollar quotes and
// statement delimiters while preserving those byte sequences inside ordinary
// quoted literals and identifiers.
func validateDefaultLexicalForm(value string) error {
	for i := 0; i < len(value); {
		if (value[i] == 'E' || value[i] == 'e') && i+1 < len(value) && value[i+1] == '\'' {
			return errors.New("escape-prefixed default strings are forbidden")
		}
		if (value[i] == 'U' || value[i] == 'u') && i+2 < len(value) && value[i+1] == '&' && (value[i+2] == '\'' || value[i+2] == '"') {
			return errors.New("Unicode-escaped default strings and identifiers are forbidden")
		}
		if (value[i] == 'B' || value[i] == 'b' || value[i] == 'X' || value[i] == 'x') && i+1 < len(value) && value[i+1] == '\'' {
			return errors.New("bit-string default literals are forbidden")
		}
		switch value[i] {
		case '\'':
			i++
			for i < len(value) {
				if value[i] != '\'' {
					i++
					continue
				}
				if i+1 < len(value) && value[i+1] == '\'' {
					i += 2
					continue
				}
				i++
				break
			}
		case '"':
			i++
			for i < len(value) {
				if value[i] != '"' {
					i++
					continue
				}
				if i+1 < len(value) && value[i+1] == '"' {
					i += 2
					continue
				}
				i++
				break
			}
		case ';':
			return errors.New("default expression statement delimiters are forbidden")
		case '-':
			if i+1 < len(value) && value[i+1] == '-' {
				return errors.New("default expression comments are forbidden")
			}
			i++
		case '/', '*':
			if i+1 < len(value) && (value[i:i+2] == "/*" || value[i:i+2] == "*/") {
				return errors.New("default expression comments are forbidden")
			}
			i++
		case '$':
			return errors.New("dollar syntax in default expressions is forbidden")
		default:
			i++
		}
	}
	return nil
}

func isDefaultExpressionSelect(stmt *pg_query.SelectStmt) bool {
	if stmt == nil || len(stmt.GetTargetList()) != 1 {
		return false
	}
	target := stmt.GetTargetList()[0].GetResTarget()
	if target == nil || target.GetVal() == nil || target.GetName() != "" || len(target.GetIndirection()) != 0 {
		return false
	}
	return len(stmt.GetDistinctClause()) == 0 && stmt.GetIntoClause() == nil && len(stmt.GetFromClause()) == 0 &&
		stmt.GetWhereClause() == nil && len(stmt.GetGroupClause()) == 0 && !stmt.GetGroupDistinct() &&
		stmt.GetHavingClause() == nil && len(stmt.GetWindowClause()) == 0 && len(stmt.GetValuesLists()) == 0 &&
		len(stmt.GetSortClause()) == 0 && stmt.GetLimitOffset() == nil && stmt.GetLimitCount() == nil &&
		len(stmt.GetLockingClause()) == 0 && stmt.GetWithClause() == nil && stmt.GetLarg() == nil && stmt.GetRarg() == nil &&
		stmt.GetOp() == pg_query.SetOperation_SETOP_NONE
}

func classifyDefaultNode(node *pg_query.Node, depth int, nodes *int) (defaultExpression, error) {
	if node == nil {
		return defaultExpression{}, errors.New("default expression contains an empty node")
	}
	if depth > maxDefaultExpressionDepth {
		return defaultExpression{}, errors.New("default expression exceeds nesting limit")
	}
	(*nodes)++
	if *nodes > maxDefaultExpressionNodes {
		return defaultExpression{}, errors.New("default expression exceeds node limit")
	}
	if value := node.GetAConst(); value != nil {
		return classifyDefaultConstant(value)
	}
	if cast := node.GetTypeCast(); cast != nil {
		arg, err := classifyDefaultNode(cast.GetArg(), depth+1, nodes)
		if err != nil {
			return defaultExpression{}, err
		}
		typ, err := classifyDefaultType(cast.GetTypeName(), depth+1, nodes)
		if err != nil {
			return defaultExpression{}, err
		}
		return defaultExpression{Kind: defaultExpressionCast, Cast: &defaultCast{Expression: arg, Type: typ}}, nil
	}
	if call := node.GetFuncCall(); call != nil {
		name, err := classifyDefaultName(call.GetFuncname())
		if err != nil {
			return defaultExpression{}, fmt.Errorf("default function name: %w", err)
		}
		explicitCall := call.GetFuncformat() == pg_query.CoercionForm_COERCE_EXPLICIT_CALL
		extractSyntax := call.GetFuncformat() == pg_query.CoercionForm_COERCE_SQL_SYNTAX && strings.Join(name.Parts, ".") == "pg_catalog.extract"
		if call.GetAggFilter() != nil || len(call.GetAggOrder()) != 0 || call.GetOver() != nil || call.GetAggWithinGroup() || call.GetAggStar() || call.GetAggDistinct() || call.GetFuncVariadic() || !explicitCall && !extractSyntax {
			return defaultExpression{}, errors.New("default function uses unsupported call features")
		}
		if len(call.GetArgs()) > maxDefaultFunctionArgs {
			return defaultExpression{}, errors.New("default function exceeds argument limit")
		}
		args := make([]defaultExpression, 0, len(call.GetArgs()))
		for _, rawArg := range call.GetArgs() {
			arg, err := classifyDefaultNode(rawArg, depth+1, nodes)
			if err != nil {
				return defaultExpression{}, fmt.Errorf("default function argument: %w", err)
			}
			args = append(args, arg)
		}
		return defaultExpression{Kind: defaultExpressionFunction, Function: &defaultFunction{Name: name, Arguments: args, SQLSyntax: extractSyntax}}, nil
	}
	if ref := node.GetColumnRef(); ref != nil {
		name, err := classifyDefaultName(ref.GetFields())
		if err != nil {
			return defaultExpression{}, fmt.Errorf("default reference: %w", err)
		}
		return defaultExpression{Kind: defaultExpressionReference, Reference: &name}, nil
	}
	if array := node.GetAArrayExpr(); array != nil {
		if len(array.GetElements()) > maxDefaultFunctionArgs {
			return defaultExpression{}, errors.New("default array exceeds element limit")
		}
		elements := make([]defaultExpression, 0, len(array.GetElements()))
		for _, rawElement := range array.GetElements() {
			element, err := classifyDefaultNode(rawElement, depth+1, nodes)
			if err != nil {
				return defaultExpression{}, fmt.Errorf("default array element: %w", err)
			}
			elements = append(elements, element)
		}
		return defaultExpression{Kind: defaultExpressionArray, Array: elements}, nil
	}
	if sqlValue := node.GetSqlvalueFunction(); sqlValue != nil {
		if sqlValue.GetXpr() != nil {
			return defaultExpression{}, errors.New("SQL value function has an unexpected expression")
		}
		name := ""
		var precision *int
		switch sqlValue.GetOp() {
		case pg_query.SQLValueFunctionOp_SVFOP_CURRENT_DATE:
			name = "current_date"
		case pg_query.SQLValueFunctionOp_SVFOP_CURRENT_TIME:
			name = "current_time"
		case pg_query.SQLValueFunctionOp_SVFOP_CURRENT_TIME_N:
			name = "current_time_n"
			value := int(sqlValue.GetTypmod())
			precision = &value
		case pg_query.SQLValueFunctionOp_SVFOP_CURRENT_TIMESTAMP:
			name = "current_timestamp"
		case pg_query.SQLValueFunctionOp_SVFOP_CURRENT_TIMESTAMP_N:
			name = "current_timestamp_n"
			value := int(sqlValue.GetTypmod())
			precision = &value
		case pg_query.SQLValueFunctionOp_SVFOP_LOCALTIME:
			name = "localtime"
		case pg_query.SQLValueFunctionOp_SVFOP_LOCALTIME_N:
			name = "localtime_n"
			value := int(sqlValue.GetTypmod())
			precision = &value
		case pg_query.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP:
			name = "localtimestamp"
		case pg_query.SQLValueFunctionOp_SVFOP_LOCALTIMESTAMP_N:
			name = "localtimestamp_n"
			value := int(sqlValue.GetTypmod())
			precision = &value
		default:
			return defaultExpression{}, errors.New("SQL value function is not supported")
		}
		return defaultExpression{Kind: defaultExpressionFunction, Function: &defaultFunction{Name: defaultReference{Parts: []string{name}}, Precision: precision}}, nil
	}
	if operator := node.GetAExpr(); operator != nil {
		if operator.GetKind() != pg_query.A_Expr_Kind_AEXPR_OP {
			return defaultExpression{}, errors.New("default operator expression kind is not supported")
		}
		name, err := classifyDefaultName(operator.GetName())
		operatorName := ""
		if err == nil && len(name.Parts) == 1 {
			operatorName = name.Parts[0]
		} else if err == nil && len(name.Parts) == 2 && name.Parts[0] == "pg_catalog" {
			operatorName = name.Parts[1]
		}
		if !isBoundedDefaultOperator(operatorName) {
			return defaultExpression{}, errors.New("default operator is not allowlisted; supported operators are +, -, *, /, and %")
		}
		if operator.GetRexpr() == nil {
			return defaultExpression{}, errors.New("default operator is missing its right operand")
		}
		right, err := classifyDefaultNode(operator.GetRexpr(), depth+1, nodes)
		if err != nil {
			return defaultExpression{}, fmt.Errorf("default operator right operand: %w", err)
		}
		var left *defaultExpression
		if operator.GetLexpr() == nil {
			if operatorName != "+" && operatorName != "-" {
				return defaultExpression{}, errors.New("only + and - are supported as unary default operators")
			}
		} else {
			classified, err := classifyDefaultNode(operator.GetLexpr(), depth+1, nodes)
			if err != nil {
				return defaultExpression{}, fmt.Errorf("default operator left operand: %w", err)
			}
			left = &classified
		}
		return defaultExpression{Kind: defaultExpressionOperator, Operator: &defaultOperator{Name: operatorName, Left: left, Right: right}}, nil
	}
	return defaultExpression{}, errors.New("default expression contains an unsupported AST node")
}

func isBoundedDefaultOperator(value string) bool {
	switch value {
	case "+", "-", "*", "/", "%":
		return true
	default:
		return false
	}
}

func classifyDefaultConstant(value *pg_query.A_Const) (defaultExpression, error) {
	literal := defaultLiteral{}
	switch {
	case value.GetIsnull():
		literal.Kind = defaultLiteralNull
	case value.GetIval() != nil:
		literal.Kind, literal.Text = defaultLiteralInteger, fmt.Sprint(value.GetIval().GetIval())
	case value.GetFval() != nil:
		literal.Kind, literal.Text = defaultLiteralFloat, value.GetFval().GetFval()
	case value.GetSval() != nil:
		literal.Kind, literal.Text = defaultLiteralString, value.GetSval().GetSval()
	case value.GetBoolval() != nil:
		literal.Kind, literal.Boolean = defaultLiteralBoolean, value.GetBoolval().GetBoolval()
	default:
		return defaultExpression{}, errors.New("default literal kind is not supported")
	}
	return defaultExpression{Kind: defaultExpressionLiteral, Literal: &literal}, nil
}

func classifyDefaultType(value *pg_query.TypeName, depth int, nodes *int) (defaultTypeReference, error) {
	if value == nil || value.GetSetof() || value.GetPctType() || value.GetTypeOid() != 0 || value.GetTypemod() != -1 {
		return defaultTypeReference{}, errors.New("default cast type contains unsupported attributes")
	}
	name, err := classifyDefaultName(value.GetNames())
	if err != nil {
		return defaultTypeReference{}, fmt.Errorf("default cast type name: %w", err)
	}
	if len(value.GetTypmods()) > 2 || len(value.GetArrayBounds()) > 1 {
		return defaultTypeReference{}, errors.New("default cast type exceeds modifier or array limits")
	}
	modifiers := make([]defaultExpression, 0, len(value.GetTypmods()))
	for _, rawModifier := range value.GetTypmods() {
		modifier, err := classifyDefaultNode(rawModifier, depth+1, nodes)
		if err != nil || modifier.Kind != defaultExpressionLiteral || modifier.Literal == nil || modifier.Literal.Kind != defaultLiteralInteger || len(modifier.Literal.Text) > 1 && modifier.Literal.Text[0] == '-' {
			return defaultTypeReference{}, errors.New("default cast type modifier is not a nonnegative integer literal")
		}
		modifiers = append(modifiers, modifier)
	}
	for _, bound := range value.GetArrayBounds() {
		integer := bound.GetInteger()
		if integer == nil || integer.GetIval() != -1 {
			return defaultTypeReference{}, errors.New("default cast type array bound is not unsized")
		}
	}
	return defaultTypeReference{Name: name, Modifiers: modifiers, ArrayBounds: len(value.GetArrayBounds())}, nil
}

func classifyDefaultName(nodes []*pg_query.Node) (defaultReference, error) {
	if len(nodes) == 0 || len(nodes) > maxDefaultNameParts {
		return defaultReference{}, errors.New("name is empty")
	}
	parts := make([]string, 0, len(nodes))
	for _, node := range nodes {
		part := node.GetString_()
		if part == nil || part.GetSval() == "" || len(part.GetSval()) > maxDefaultNamePartBytes {
			return defaultReference{}, errors.New("name contains a non-identifier component")
		}
		parts = append(parts, part.GetSval())
	}
	return defaultReference{Parts: parts}, nil
}

func defaultNameEquals(nodes []*pg_query.Node, want string) bool {
	name, err := classifyDefaultName(nodes)
	return err == nil && len(name.Parts) == 1 && name.Parts[0] == want
}
