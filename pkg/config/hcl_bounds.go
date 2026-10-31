package config

import (
	"encoding/json"
	"errors"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

func consumeHCLBytes(value any, remaining *int, at hcl.Range) error {
	raw, err := json.Marshal(value)
	if err != nil {
		return locatedHCL(at, "", "invalid project value")
	}
	*remaining -= len(raw)
	if *remaining < 0 {
		return locatedHCL(at, "", "expanded project exceeds 4 MiB")
	}
	return nil
}

// Bound referenced values before interpolation. A small expression can repeat
// a large variable many times; a byte limit only after Value would allocate the
// expanded string before refusing it. Counting referenced subtrees is a
// conservative limit and deliberately does not execute composite expressions.
func checkHCLExpressionSize(expression hclsyntax.Expression, ctx *hcl.EvalContext) error {
	remaining := maxProjectBytes
	diagnostics := hclsyntax.VisitAll(expression, func(node hclsyntax.Node) hcl.Diagnostics {
		value := cty.NilVal
		switch node := node.(type) {
		case *hclsyntax.LiteralValueExpr:
			value = node.Val
		case *hclsyntax.ScopeTraversalExpr:
			var diagnostics hcl.Diagnostics
			value, diagnostics = node.Value(ctx)
			if diagnostics.HasErrors() {
				// Bare object keys also contain traversal nodes. The normal
				// evaluator distinguishes those from undeclared references.
				return nil
			}
		}
		if value == cty.NilVal {
			return nil
		}
		if err := countHCLValueBytes(value, &remaining); err != nil {
			at := node.Range()
			return hcl.Diagnostics{&hcl.Diagnostic{Severity: hcl.DiagError, Summary: "project expression exceeds 4 MiB evaluation limit", Subject: &at}}
		}
		return nil
	})
	return diagnosticHCL(diagnostics)
}

func countHCLValueBytes(value cty.Value, remaining *int) error {
	return cty.Walk(value, func(path cty.Path, value cty.Value) (bool, error) {
		*remaining -= 16
		if len(path) > 0 {
			if step, ok := path[len(path)-1].(cty.GetAttrStep); ok {
				*remaining -= len(step.Name)
			}
		}
		*remaining -= hclScalarBytes(value)
		if *remaining < 0 {
			return false, errors.New("project value exceeds evaluation limit")
		}
		return true, nil
	})
}

func hclScalarBytes(value cty.Value) int {
	if !value.IsKnown() || value.IsNull() {
		return 0
	}
	switch value.Type() {
	case cty.String:
		return len(value.AsString())
	case cty.Number:
		number := value.AsBigFloat()
		exponent := int64(number.MantExp(nil))
		if exponent < 0 {
			exponent = -exponent
		}
		// cty's JSON encoder emits decimal notation. A short exponent
		// literal must not allocate millions of decimal zeroes first.
		return int((exponent+int64(number.Prec()))/3 + 64)
	default:
		return 0
	}
}
