package config

import (
	"fmt"
	"reflect"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

// An empty set creates no environments, but must not hide typos, unsupported
// expressions, or undeclared references. Symbolic each values permit valid
// templates without inventing a concrete environment or credential reference.
func validateEmptyHCLSet(block *hclsyntax.Block, ctx *hcl.EvalContext) error {
	field := "/environment_sets/" + pointerPart(block.Labels[0])
	if len(block.Body.Blocks) != 0 {
		return locatedHCL(block.Body.Blocks[0].TypeRange, field, "nested blocks are not supported; use typed attributes")
	}
	for _, name := range []string{"target", "schema_sources", "migration_dir"} {
		if _, ok := block.Body.Attributes[name]; !ok {
			return locatedHCL(block.TypeRange, field+"/"+name, "environment template requires this attribute")
		}
	}
	child := ctx.NewChild()
	child.Variables = map[string]cty.Value{"each": cty.ObjectVal(map[string]cty.Value{"key": cty.UnknownVal(cty.String), "value": cty.DynamicVal})}
	allowed := structFields(reflect.TypeFor[Environment]())
	locations := map[string]SourceRange{field: sourceRangeHCL(block.Range())}
	for _, name := range sortedKeys(block.Body.Attributes) {
		if name == "for_each" {
			continue
		}
		attribute := block.Body.Attributes[name]
		path := field + "/" + pointerPart(name)
		typ, ok := allowed[name]
		if !ok {
			return locatedHCL(attribute.NameRange, path, "unknown configuration attribute")
		}
		value, err := evaluateProjectHCL(attribute.Expr, child)
		if err != nil {
			return err
		}
		collectHCLLocations(attribute.Expr, path, locations)
		if err := validateHCLTemplateValue(value, typ, path); err != nil {
			return relocateProjectError(err, locations)
		}
	}
	return nil
}

func validateHCLTemplateValue(value cty.Value, typ reflect.Type, field string) error {
	fail := func(message string) error {
		return &LocatedError{Code: "invalid_config", Field: field, Message: message}
	}
	if value.IsNull() {
		return fail("configuration value must not be null")
	}
	if value.Type() == cty.DynamicPseudoType || typ.Kind() == reflect.Interface {
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct, reflect.Map:
		return validateHCLTemplateObject(value, typ, field)
	case reflect.Slice:
		return validateHCLTemplateList(value, typ, field)
	default:
		expected := map[reflect.Kind]cty.Type{reflect.String: cty.String, reflect.Bool: cty.Bool, reflect.Int: cty.Number}[typ.Kind()]
		if !value.Type().Equals(expected) {
			return fail("expected " + typ.Kind().String())
		}
		if value.IsKnown() {
			plain, err := plainHCLValue(value)
			if err != nil {
				return fail("invalid configuration value")
			}
			return validateScalarNode(&projectNode{value: plain}, typ, field)
		}
	}
	return nil
}

func validateHCLTemplateObject(value cty.Value, typ reflect.Type, field string) error {
	if !value.Type().IsObjectType() && !value.Type().IsMapType() {
		return &LocatedError{Code: "invalid_config", Field: field, Message: "expected an object"}
	}
	if !value.IsKnown() {
		return nil
	}
	var allowed map[string]reflect.Type
	if typ.Kind() == reflect.Struct {
		allowed = structFields(typ)
	}
	items := value.AsValueMap()
	for _, key := range sortedKeys(items) {
		itemType := allowed[key]
		if typ.Kind() == reflect.Map {
			itemType = typ.Elem()
		}
		path := field + "/" + pointerPart(key)
		if itemType == nil {
			return &LocatedError{Code: "unknown_field", Field: path, Message: "unknown configuration field"}
		}
		if err := validateHCLTemplateValue(items[key], itemType, path); err != nil {
			return err
		}
	}
	return nil
}

func validateHCLTemplateList(value cty.Value, typ reflect.Type, field string) error {
	if !value.Type().IsTupleType() && !value.Type().IsListType() {
		return &LocatedError{Code: "invalid_config", Field: field, Message: "expected a list"}
	}
	if !value.IsKnown() {
		return nil
	}
	for i, item := range value.AsValueSlice() {
		if err := validateHCLTemplateValue(item, typ.Elem(), fmt.Sprintf("%s/%d", field, i)); err != nil {
			return err
		}
	}
	return nil
}
