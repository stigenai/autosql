package source

import (
	"encoding/json"
	"fmt"
	"strings"

	"autosql/pkg/schema"

	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
)

const hclExpressionPrefix = "\x00autosql-expression:"

type hclSQLExpression struct {
	SQL        string         `json:"sql"`
	References []hclReference `json:"references,omitempty"`
}

func encodeHCLExpression(expression hclSQLExpression) cty.Value {
	encoded, _ := json.Marshal(expression)
	return cty.StringVal(hclExpressionPrefix + string(encoded))
}

func decodeHCLExpression(value any) (hclSQLExpression, bool) {
	text, ok := value.(string)
	if !ok || !strings.HasPrefix(text, hclExpressionPrefix) {
		return hclSQLExpression{}, false
	}
	var expression hclSQLExpression
	if json.Unmarshal([]byte(strings.TrimPrefix(text, hclExpressionPrefix)), &expression) != nil || strings.TrimSpace(expression.SQL) == "" {
		return hclSQLExpression{}, false
	}
	return expression, true
}

func hclExpressionFunctions() map[string]function.Function {
	return map[string]function.Function{
		"literal":    hclLiteralFunction(),
		"sql":        hclSQLFunction(),
		"cast":       hclCastFunction(),
		"enum_value": hclEnumValueFunction(),
		"sql_array":  hclArrayFunction(),
	}
}

func hclLiteralFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "value", Type: cty.DynamicPseudoType, AllowNull: true}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			value := args[0]
			var sql string
			switch {
			case value.IsNull():
				sql = "NULL"
			case value.Type() == cty.String:
				sql = "'" + strings.ReplaceAll(value.AsString(), "'", "''") + "'"
			case value.Type() == cty.Bool:
				if value.True() {
					sql = "true"
				} else {
					sql = "false"
				}
			case value.Type() == cty.Number:
				sql = value.AsBigFloat().Text('f', -1)
			default:
				return cty.NilVal, fmt.Errorf("literal() supports only null, string, boolean, and number values")
			}
			return encodeHCLExpression(hclSQLExpression{SQL: sql}), nil
		},
	})
}

func hclSQLFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "expression", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			if strings.TrimSpace(args[0].AsString()) == "" {
				return cty.NilVal, fmt.Errorf("sql() expression cannot be empty")
			}
			return encodeHCLExpression(hclSQLExpression{SQL: args[0].AsString()}), nil
		},
	})
}

func hclCastFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "expression", Type: cty.String}, {Name: "type", Type: cty.DynamicPseudoType}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			expression, ok := decodeHCLExpression(args[0].AsString())
			if !ok {
				return cty.NilVal, fmt.Errorf("cast() expression must be produced by literal(), sql(), enum_value(), cast(), or sql_array()")
			}
			typeName, reference, err := hclExpressionType(args[1])
			if err != nil {
				return cty.NilVal, err
			}
			expression.SQL = "(" + expression.SQL + ")::" + typeName
			if reference != nil {
				expression.References = append(expression.References, *reference)
			}
			return encodeHCLExpression(expression), nil
		},
	})
}

func hclEnumValueFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "type", Type: cty.DynamicPseudoType}, {Name: "value", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			ref, ok := referenceFromAny(ctyToAny(args[0]))
			if !ok || ref.Kind != schema.KindEnum {
				return cty.NilVal, fmt.Errorf("enum_value() requires an enum reference")
			}
			literal := "'" + strings.ReplaceAll(args[1].AsString(), "'", "''") + "'"
			return encodeHCLExpression(hclSQLExpression{SQL: literal + "::" + ref.qualifiedName(), References: []hclReference{ref}}), nil
		},
	})
}

func hclArrayFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "values", Type: cty.DynamicPseudoType}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			if !args[0].CanIterateElements() {
				return cty.NilVal, fmt.Errorf("sql_array() requires a list or tuple")
			}
			var parts []string
			var references []hclReference
			iterator := args[0].ElementIterator()
			for iterator.Next() {
				_, value := iterator.Element()
				if value.Type() != cty.String {
					return cty.NilVal, fmt.Errorf("sql_array() elements must be produced by expression constructors")
				}
				expression, ok := decodeHCLExpression(value.AsString())
				if !ok {
					return cty.NilVal, fmt.Errorf("sql_array() elements must be produced by expression constructors")
				}
				parts = append(parts, expression.SQL)
				references = append(references, expression.References...)
			}
			return encodeHCLExpression(hclSQLExpression{SQL: "ARRAY[" + strings.Join(parts, ", ") + "]", References: references}), nil
		},
	})
}

func hclExpressionType(value cty.Value) (string, *hclReference, error) {
	if value.Type() == cty.String {
		name := strings.TrimSpace(value.AsString())
		if name == "" {
			return "", nil, fmt.Errorf("cast() type cannot be empty")
		}
		return name, nil, nil
	}
	ref, ok := referenceFromAny(ctyToAny(value))
	if !ok {
		return "", nil, fmt.Errorf("cast() type must be a type name or enum, domain, or composite reference")
	}
	switch ref.Kind {
	case schema.KindEnum, schema.KindDomain, schema.KindComposite:
		return ref.qualifiedName(), &ref, nil
	default:
		return "", nil, fmt.Errorf("cast() cannot target %s", ref.Kind)
	}
}
