package dbtest

import (
	"math/big"
	"strings"
	"unicode"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

func testScalar(expression hcl.Expression) (Scalar, error) {
	value, err := testLiteral(expression)
	if err != nil {
		return Scalar{}, err
	}
	if value.IsNull() {
		return NullScalar(), nil
	}
	switch value.Type() {
	case cty.String:
		return StringScalar(value.AsString()), nil
	case cty.Bool:
		return BooleanScalar(value.True()), nil
	case cty.Number:
		n, accuracy := value.AsBigFloat().Int64()
		if accuracy != big.Exact {
			return Scalar{}, languageError(expression.Range(), "integer must fit signed 64 bits exactly")
		}
		return IntegerScalar(n), nil
	default:
		return Scalar{}, languageError(expression.Range(), "expected a literal integer, string, boolean or null")
	}
}

func testLiteral(expression hcl.Expression) (cty.Value, error) {
	if !isTestLiteral(expression) {
		return cty.NilVal, languageError(expression.Range(), "test values must be literal; expressions and interpolation are unsupported")
	}
	value, diags := expression.Value(nil)
	if err := languageDiagnostics(diags, expression.Range()); err != nil {
		return cty.NilVal, err
	}
	if !value.IsWhollyKnown() {
		return cty.NilVal, languageError(expression.Range(), "test values must be known")
	}
	return value, nil
}

func isTestLiteral(expression hcl.Expression) bool {
	switch expr := expression.(type) {
	case *hclsyntax.LiteralValueExpr:
		return true
	case *hclsyntax.TemplateExpr:
		return expr.IsStringLiteral()
	case *hclsyntax.UnaryOpExpr:
		literal, ok := expr.Val.(*hclsyntax.LiteralValueExpr)
		return ok && expr.Op == hclsyntax.OpNegate && literal.Val.Type() == cty.Number && !literal.Val.IsNull()
	default:
		return false
	}
}

func testString(attribute *hcl.Attribute) (string, error) {
	value, err := testScalar(attribute.Expr)
	if err != nil {
		return "", err
	}
	if value.Kind() != "string" || strings.TrimSpace(value.text) == "" {
		return "", languageError(attribute.Range, "expected a nonempty literal string")
	}
	return value.text, nil
}

func testParameters(attribute *hcl.Attribute) ([]Scalar, error) {
	if attribute == nil {
		return nil, nil
	}
	tuple, ok := attribute.Expr.(*hclsyntax.TupleConsExpr)
	if !ok || len(tuple.Exprs) > 1024 {
		return nil, languageError(attribute.Range, "params requires a literal list of at most 1024 scalars")
	}
	result := make([]Scalar, len(tuple.Exprs))
	for i, expr := range tuple.Exprs {
		value, err := testScalar(expr)
		if err != nil {
			return nil, err
		}
		result[i] = value
	}
	return result, nil
}

func testBlockName(block *hcl.Block) (string, error) {
	name := block.Labels[0]
	if strings.TrimSpace(name) != name || name == "" || len(name) > 200 || strings.Contains(name, "::") || strings.ContainsFunc(name, unicode.IsControl) {
		return "", languageError(block.LabelRanges[0], "name must be 1-200 characters without control characters or ::")
	}
	return name, nil
}
