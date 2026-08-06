package source

import (
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/convert"
)

const (
	hclLocalsVariable = "__autosql_locals"
	hclEachKey        = "__autosql_each_key"
	hclEachValue      = "__autosql_each_value"
)

func cloneHCLVariables(values HCLVariables) HCLVariables {
	clone := HCLVariables{}
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func declaredHCLVariables(body *hclsyntax.Body) []string {
	var names []string
	for _, block := range body.Blocks {
		if block.Type == "variable" && len(block.Labels) == 1 {
			names = append(names, block.Labels[0])
		}
	}
	sort.Strings(names)
	return names
}

func rejectUnknownHCLInputs(inputs HCLVariables, declared map[string]bool) error {
	var unknown []string
	for name := range inputs {
		if !declared[name] {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	return fmt.Errorf("%w: module received undeclared inputs: %s", ErrHCL, strings.Join(unknown, ", "))
}

func evaluateHCLOutputs(body *hclsyntax.Body, data []byte, variables HCLVariables) (map[string]any, error) {
	outputs := map[string]any{}
	for _, block := range body.Blocks {
		if block.Type != "output" {
			continue
		}
		if len(block.Labels) != 1 || block.Body.Attributes["value"] == nil {
			return nil, fmt.Errorf("%w: output requires one label and a value", ErrHCL)
		}
		name := block.Labels[0]
		if _, exists := outputs[name]; exists {
			return nil, fmt.Errorf("%w: duplicate module output %s", ErrHCL, name)
		}
		value, err := expressionValueWithSymbols(block.Body.Attributes["value"].Expr, data, variables, nil)
		if err != nil {
			return nil, fmt.Errorf("%w: output %s: %v", ErrHCL, name, err)
		}
		if attribute := block.Body.Attributes["type"]; attribute != nil {
			typeName := strings.TrimSpace(expressionSource(attribute.Expr, data))
			value, err = convertHCLVariable(value, typeName)
			if err != nil {
				return nil, fmt.Errorf("%w: output %s: %v", ErrHCL, name, err)
			}
		}
		outputs[name] = value
	}
	return outputs, nil
}

func prepareHCLBody(body *hclsyntax.Body, data []byte, variables HCLVariables) (*hclsyntax.Body, HCLVariables, error) {
	effective := HCLVariables{}
	for key, value := range variables {
		effective[key] = value
	}
	var problems []string
	for _, block := range body.Blocks {
		if block.Type != "variable" || len(block.Labels) != 1 {
			continue
		}
		name := block.Labels[0]
		value, supplied := effective[name]
		if !supplied {
			if attribute := block.Body.Attributes["default"]; attribute != nil {
				var err error
				value, err = expressionValueWithSymbols(attribute.Expr, data, effective, nil)
				if err != nil {
					problems = append(problems, fmt.Sprintf("variable %s default: %v", name, err))
					continue
				}
			} else {
				problems = append(problems, "variable "+name+" is required")
				continue
			}
		}
		if attribute := block.Body.Attributes["type"]; attribute != nil {
			typeName := strings.TrimSpace(expressionSource(attribute.Expr, data))
			converted, err := convertHCLVariable(value, typeName)
			if err != nil {
				problems = append(problems, fmt.Sprintf("variable %s: %v", name, err))
				continue
			}
			value = converted
		}
		effective[name] = value
	}
	locals, localProblems := evaluateHCLLocals(body, data, effective)
	problems = append(problems, localProblems...)
	if len(locals) > 0 {
		effective[hclLocalsVariable] = locals
	}
	for _, block := range body.Blocks {
		if block.Type != "variable" || len(block.Labels) != 1 {
			continue
		}
		for _, validation := range block.Body.Blocks {
			if validation.Type != "validation" {
				continue
			}
			condition := validation.Body.Attributes["condition"]
			if condition == nil {
				problems = append(problems, "variable "+block.Labels[0]+" validation requires condition")
				continue
			}
			value, err := expressionValueWithSymbols(condition.Expr, data, effective, nil)
			valid, ok := value.(bool)
			if err != nil || !ok || !valid {
				message := "validation failed"
				if attribute := validation.Body.Attributes["error_message"]; attribute != nil {
					if text, textErr := expressionValueWithSymbols(attribute.Expr, data, effective, nil); textErr == nil {
						message, _ = text.(string)
					}
				}
				problems = append(problems, fmt.Sprintf("variable %s: %s", block.Labels[0], message))
			}
		}
	}
	if len(problems) > 0 {
		sort.Strings(problems)
		return nil, nil, fmt.Errorf("%w: variable/local validation:\n- %s", ErrHCL, strings.Join(problems, "\n- "))
	}
	expanded, err := expandHCLBody(body, data, effective, nil)
	return expanded, effective, err
}

func evaluateHCLLocals(body *hclsyntax.Body, data []byte, variables HCLVariables) (map[string]any, []string) {
	pending := map[string]*hclsyntax.Attribute{}
	for _, block := range body.Blocks {
		if block.Type == "locals" {
			for name, attribute := range block.Body.Attributes {
				pending[name] = attribute
			}
		}
	}
	locals := map[string]any{}
	for len(pending) > 0 {
		progress := false
		keys := make([]string, 0, len(pending))
		for key := range pending {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			scope := map[string]any{"local": locals}
			value, err := expressionValueWithSymbols(pending[key].Expr, data, variables, scope)
			if err != nil {
				continue
			}
			locals[key] = value
			delete(pending, key)
			progress = true
		}
		if !progress {
			keys = keys[:0]
			for key := range pending {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			return locals, []string{"locals contain an unresolved reference or cycle: " + strings.Join(keys, ", ")}
		}
	}
	return locals, nil
}

func expandHCLBody(body *hclsyntax.Body, data []byte, variables HCLVariables, inheritedEach map[string]any) (*hclsyntax.Body, error) {
	out := &hclsyntax.Body{Attributes: body.Attributes, SrcRange: body.SrcRange, EndRange: body.EndRange}
	for _, block := range body.Blocks {
		if block.Type == "variable" || block.Type == "locals" {
			continue
		}
		instances := []map[string]any{inheritedEach}
		metaForEach := false
		if attribute := block.Body.Attributes["for_each"]; attribute != nil {
			scope := eachHCLSymbols(inheritedEach)
			value, err := expressionValueWithSymbols(attribute.Expr, data, variables, scope)
			if err != nil {
				return nil, fmt.Errorf("%s: for_each: %w", block.Type, err)
			}
			object, ok := value.(map[string]any)
			if !ok {
				if block.Type != "trigger" {
					return nil, fmt.Errorf("%w: %s for_each must be a map/object with stable string keys", ErrHCL, block.Type)
				}
				object = nil
			} else {
				metaForEach = true
			}
			keys := make([]string, 0, len(object))
			for key := range object {
				keys = append(keys, key)
			}
			sort.Strings(keys)
			if metaForEach {
				instances = make([]map[string]any, 0, len(keys))
				for _, key := range keys {
					instances = append(instances, map[string]any{"key": key, "value": object[key]})
				}
			}
		}
		for _, each := range instances {
			clone := *block
			clone.Labels = append([]string(nil), block.Labels...)
			cloneBody := *block.Body
			cloneBody.Attributes = map[string]*hclsyntax.Attribute{}
			for key, attribute := range block.Body.Attributes {
				if (key != "for_each" || !metaForEach) && (key != "name" || !metaForEach) {
					cloneBody.Attributes[key] = attribute
				}
			}
			if each != nil {
				if metaForEach {
					name := each["key"].(string)
					if attribute := block.Body.Attributes["name"]; attribute != nil {
						value, err := expressionValueWithSymbols(attribute.Expr, data, variables, eachHCLSymbols(each))
						if err != nil {
							return nil, err
						}
						name, _ = value.(string)
					}
					if name == "" || len(clone.Labels) == 0 {
						return nil, fmt.Errorf("%w: %s for_each block requires a non-empty label/name", ErrHCL, block.Type)
					}
					clone.Labels[len(clone.Labels)-1] = name
				}
				cloneBody.Attributes[hclEachKey] = literalHCLAttribute(hclEachKey, cty.StringVal(each["key"].(string)))
				value, err := ctyValue(each["value"])
				if err != nil {
					return nil, err
				}
				cloneBody.Attributes[hclEachValue] = literalHCLAttribute(hclEachValue, value)
			}
			expandedChildren, err := expandHCLBody(&cloneBody, data, variables, each)
			if err != nil {
				return nil, err
			}
			clone.Body = expandedChildren
			out.Blocks = append(out.Blocks, &clone)
		}
	}
	return out, nil
}

func literalHCLAttribute(name string, value cty.Value) *hclsyntax.Attribute {
	source := append([]byte(name+" = "), hclwrite.TokensForValue(value).Bytes()...)
	file, diagnostics := hclsyntax.ParseConfig(source, "generated.hcl", hcl.Pos{Line: 1, Column: 1})
	if diagnostics.HasErrors() {
		panic(diagnostics.Error())
	}
	return file.Body.(*hclsyntax.Body).Attributes[name]
}

func eachHCLSymbols(each map[string]any) map[string]any {
	if each == nil {
		return nil
	}
	return map[string]any{"each": each}
}

func hclBlockEvaluationSymbols(block *hclsyntax.Block, data []byte, variables HCLVariables, base map[string]any) map[string]any {
	out := map[string]any{}
	for key, value := range base {
		out[key] = value
	}
	each := map[string]any{}
	for attribute, name := range map[string]string{hclEachKey: "key", hclEachValue: "value"} {
		if item := block.Body.Attributes[attribute]; item != nil {
			value, err := expressionValueWithSymbols(item.Expr, data, variables, nil)
			if err == nil {
				each[name] = value
			}
		}
	}
	if len(each) > 0 {
		out["each"] = each
	}
	if locals, ok := variables[hclLocalsVariable].(map[string]any); ok {
		out["local"] = locals
	}
	return out
}

func convertHCLVariable(value any, typeName string) (any, error) {
	want := map[string]cty.Type{"string": cty.String, "number": cty.Number, "bool": cty.Bool, "boolean": cty.Bool, "list(string)": cty.List(cty.String), "set(string)": cty.Set(cty.String), "map(string)": cty.Map(cty.String)}[strings.ReplaceAll(typeName, " ", "")]
	if want == cty.NilType {
		return nil, fmt.Errorf("unsupported declared type %q", typeName)
	}
	current, err := ctyValue(value)
	if err != nil {
		return nil, err
	}
	converted, err := convert.Convert(current, want)
	if err != nil {
		return nil, fmt.Errorf("does not match %s", typeName)
	}
	return ctyToAny(converted), nil
}

func expressionSource(expression hcl.Expression, data []byte) string {
	rangeValue := expression.Range()
	if rangeValue.Start.Byte >= 0 && rangeValue.End.Byte <= len(data) {
		return string(data[rangeValue.Start.Byte:rangeValue.End.Byte])
	}
	return ""
}
