package config

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
	ctyjson "github.com/zclconf/go-cty/cty/json"
)

func locatedHCL(at hcl.Range, field, message string) error {
	return &LocatedError{Code: "invalid_config", Field: field, Range: sourceRangeHCL(at), Message: message}
}
func sourceRangeHCL(at hcl.Range) SourceRange {
	return SourceRange{Filename: at.Filename, StartLine: at.Start.Line, StartColumn: at.Start.Column, EndLine: at.End.Line, EndColumn: at.End.Column}
}
func diagnosticHCL(diagnostics hcl.Diagnostics) error {
	for _, d := range diagnostics {
		if d.Severity == hcl.DiagError {
			at := hcl.Range{}
			if d.Subject != nil {
				at = *d.Subject
			}
			return locatedHCL(at, "", d.Summary)
		}
	}
	return nil
}

func parseHCLProject(data []byte, filename string) (*Config, error) {
	if err := checkHCLBounds(data, filename); err != nil {
		return nil, err
	}
	file, diags := hclsyntax.ParseConfig(data, filename, hcl.InitialPos)
	if diags.HasErrors() {
		return nil, diagnosticHCL(diags)
	}
	body := file.Body.(*hclsyntax.Body)
	root := map[string]any{"environments": map[string]any{}, "dev_factories": map[string]any{}}
	locations := map[string]SourceRange{"": sourceRangeHCL(body.Range())}
	ctx, err := projectHCLVariables(body, root, locations)
	if err != nil {
		return nil, err
	}
	if err := projectHCLRootAttributes(body, ctx, root, locations); err != nil {
		return nil, err
	}

	remaining := maxProjectBytes
	if err := consumeHCLBytes(root, &remaining, body.Range()); err != nil {
		return nil, err
	}
	if err := projectHCLBlocks(body.Blocks, ctx, root, locations, &remaining); err != nil {
		return nil, err
	}
	return normalizeHCLProject(root, filename, locations)
}

func normalizeHCLProject(root map[string]any, filename string, locations map[string]SourceRange) (*Config, error) {
	raw, err := json.Marshal(root)
	if err != nil {
		return nil, err
	}
	c, err := parseJSONProject(raw, filename)
	if err != nil {
		return nil, relocateProjectError(err, locations)
	}
	// Preserve the normalized model's complete field presence. An attribute may
	// supply an entire object through var/each, in which case all its descendants
	// inherit the expression range rather than disappearing from provenance.
	source := Config{path: filename, locations: locations}
	for field := range c.locations {
		c.locations[field] = source.location(field)
	}
	if c.Version != CurrentVersion {
		return nil, c.problem("/version", "HCL projects require version 2")
	}
	return c, nil
}

func projectHCLRootAttributes(body *hclsyntax.Body, ctx *hcl.EvalContext, root map[string]any, locations map[string]SourceRange) error {
	for _, name := range sortedKeys(body.Attributes) {
		attribute := body.Attributes[name]
		if name == "variables" {
			continue
		}
		if name != "version" && name != "environment" {
			return locatedHCL(attribute.NameRange, "/"+name, "unknown project attribute")
		}
		value, err := projectHCLValue(attribute.Expr, ctx)
		if err != nil {
			return err
		}
		root[name], err = plainHCLValue(value)
		if err != nil {
			return locatedHCL(attribute.Expr.Range(), "/"+name, "invalid project value")
		}
		collectHCLLocations(attribute.Expr, "/"+name, locations)
	}
	return nil
}

func projectHCLBlocks(blocks hclsyntax.Blocks, ctx *hcl.EvalContext, root map[string]any, locations map[string]SourceRange, remaining *int) error {
	seenBlocks := map[string]bool{}
	for _, block := range blocks {
		if len(block.Labels) == 1 {
			key := block.Type + ":" + block.Labels[0]
			if seenBlocks[key] {
				return locatedHCL(block.TypeRange, "", "duplicate project block name")
			}
			seenBlocks[key] = true
		}
		if err := projectHCLBlock(block, ctx, root, locations, remaining); err != nil {
			return err
		}
	}
	return nil
}

func projectHCLVariables(body *hclsyntax.Body, root map[string]any, locations map[string]SourceRange) (*hcl.EvalContext, error) {
	variables := cty.EmptyObjectVal
	if attribute, ok := body.Attributes["variables"]; ok {
		value, err := projectHCLValue(attribute.Expr, nil)
		if err != nil {
			return nil, err
		}
		if !finiteHCLMap(value) {
			return nil, locatedHCL(attribute.Expr.Range(), "/variables", "variables must declare a finite object")
		}
		variables = value
		plain, err := plainHCLValue(value)
		if err != nil {
			return nil, locatedHCL(attribute.Expr.Range(), "/variables", "invalid variable declaration")
		}
		root["variables"] = plain
		collectHCLLocations(attribute.Expr, "/variables", locations)
	}
	return &hcl.EvalContext{Variables: map[string]cty.Value{"var": variables}}, nil
}

func projectHCLBlock(block *hclsyntax.Block, ctx *hcl.EvalContext, root map[string]any, locations map[string]SourceRange, remaining *int) error {
	if len(block.Labels) != 1 {
		return locatedHCL(block.TypeRange, "", "project blocks require exactly one name")
	}
	name := block.Labels[0]
	switch block.Type {
	case "environment":
		return addHCLEnvironment(block, name, ctx, root, locations, remaining)
	case "environment_set":
		return expandHCLEnvironments(block, name, ctx, root, locations, remaining)
	case "dev_factory":
		factories := root["dev_factories"].(map[string]any)
		field := "/dev_factories/" + pointerPart(name)
		if _, ok := factories[name]; ok {
			return locatedHCL(block.TypeRange, field, "duplicate development factory")
		}
		value, err := hclBlockObject(block, ctx, reflect.TypeFor[DevelopmentFactory](), field, "", locations)
		if err != nil {
			return err
		}
		if err := consumeHCLBytes(map[string]any{name: value}, remaining, block.Range()); err != nil {
			return err
		}
		factories[name] = value
		return nil
	default:
		return locatedHCL(block.TypeRange, "", "unknown project block")
	}
}

func addHCLEnvironment(block *hclsyntax.Block, name string, ctx *hcl.EvalContext, root map[string]any, locations map[string]SourceRange, remaining *int) error {
	environments := root["environments"].(map[string]any)
	field := environmentPath(name)
	if _, ok := environments[name]; ok {
		return locatedHCL(block.TypeRange, field, "duplicate environment name")
	}
	if len(environments) >= MaxEnvironments {
		return locatedHCL(block.TypeRange, field, "at most 256 environments are supported")
	}
	ignored := ""
	if block.Type == "environment_set" {
		ignored = "for_each"
	}
	value, err := hclBlockObject(block, ctx, reflect.TypeFor[Environment](), field, ignored, locations)
	if err != nil {
		return err
	}
	if err := consumeHCLBytes(map[string]any{name: value}, remaining, block.Range()); err != nil {
		return err
	}
	environments[name] = value
	return nil
}

func expandHCLEnvironments(block *hclsyntax.Block, name string, ctx *hcl.EvalContext, root map[string]any, locations map[string]SourceRange, remaining *int) error {
	attribute, ok := block.Body.Attributes["for_each"]
	if !ok {
		return locatedHCL(block.TypeRange, "", "environment_set requires for_each")
	}
	if strings.Contains(name, "/") || !projectName.MatchString(name) {
		return locatedHCL(block.LabelRanges[0], "", "environment set name is invalid")
	}
	value, err := projectHCLValue(attribute.Expr, ctx)
	if err != nil {
		return err
	}
	if !finiteHCLMap(value) {
		return locatedHCL(attribute.Expr.Range(), "/for_each", "for_each requires a finite map")
	}
	if value.LengthInt() > MaxEnvironments-len(root["environments"].(map[string]any)) {
		return locatedHCL(attribute.Expr.Range(), "/for_each", "at most 256 environments are supported")
	}
	if value.LengthInt() == 0 {
		return validateEmptyHCLSet(block, ctx)
	}
	return addHCLEnvironmentSet(block, name, ctx, value, root, locations, remaining)
}

func finiteHCLMap(value cty.Value) bool {
	return !value.IsNull() && value.IsKnown() && (value.Type().IsMapType() || value.Type().IsObjectType())
}

func addHCLEnvironmentSet(block *hclsyntax.Block, name string, ctx *hcl.EvalContext, value cty.Value, root map[string]any, locations map[string]SourceRange, remaining *int) error {
	iterator := value.ElementIterator()
	for iterator.Next() {
		key, item := iterator.Element()
		if !projectName.MatchString(key.AsString()) || strings.Contains(key.AsString(), "/") {
			return locatedHCL(block.Body.Attributes["for_each"].Expr.Range(), "/for_each", "environment set key is invalid")
		}
		child := ctx.NewChild()
		child.Variables = map[string]cty.Value{"each": cty.ObjectVal(map[string]cty.Value{"key": key, "value": item})}
		if err := addHCLEnvironment(block, name+"/"+key.AsString(), child, root, locations, remaining); err != nil {
			return err
		}
	}
	return nil
}

func hclBlockObject(block *hclsyntax.Block, ctx *hcl.EvalContext, typ reflect.Type, field, ignored string, locations map[string]SourceRange) (map[string]any, error) {
	if len(block.Body.Blocks) > 0 {
		return nil, locatedHCL(block.Body.Blocks[0].TypeRange, field, "nested blocks are not supported; use typed attributes")
	}
	allowed := structFields(typ)
	values := map[string]any{}
	locations[field] = sourceRangeHCL(block.Range())
	for _, name := range sortedKeys(block.Body.Attributes) {
		attribute := block.Body.Attributes[name]
		if name == ignored {
			continue
		}
		path := field + "/" + pointerPart(name)
		if _, ok := allowed[name]; !ok {
			return nil, locatedHCL(attribute.NameRange, path, "unknown configuration attribute")
		}
		value, err := projectHCLValue(attribute.Expr, ctx)
		if err != nil {
			return nil, err
		}
		plain, err := plainHCLValue(value)
		if err != nil {
			return nil, locatedHCL(attribute.Expr.Range(), path, "invalid configuration value")
		}
		values[name] = plain
		collectHCLLocations(attribute.Expr, path, locations)
	}
	return values, nil
}

// Project expressions deliberately exclude comprehensions, function calls and
// arithmetic. Literal objects/lists, declared variables, and each traversals
// suffice for finite environment sets without a second program-source runtime.
func projectHCLValue(expression hclsyntax.Expression, ctx *hcl.EvalContext) (cty.Value, error) {
	value, err := evaluateProjectHCL(expression, ctx)
	if err != nil {
		return cty.NilVal, err
	}
	if !value.IsWhollyKnown() {
		return cty.NilVal, locatedHCL(expression.Range(), "", "configuration value must be known")
	}
	return value, nil
}

func evaluateProjectHCL(expression hclsyntax.Expression, ctx *hcl.EvalContext) (cty.Value, error) {
	if err := checkHCLExpressionSize(expression, ctx); err != nil {
		return cty.NilVal, err
	}
	diagnostics := hclsyntax.VisitAll(expression, func(node hclsyntax.Node) hcl.Diagnostics {
		return validateProjectHCLExpression(node, ctx)
	})
	if diagnostics.HasErrors() {
		return cty.NilVal, diagnosticHCL(diagnostics)
	}
	value, diags := expression.Value(ctx)
	if diags.HasErrors() {
		return cty.NilVal, diagnosticHCL(diags)
	}
	return value, nil
}

func validateProjectHCLExpression(node hclsyntax.Node, ctx *hcl.EvalContext) hcl.Diagnostics {
	switch node := node.(type) {
	case *hclsyntax.FunctionCallExpr, *hclsyntax.ForExpr, *hclsyntax.BinaryOpExpr, *hclsyntax.UnaryOpExpr, *hclsyntax.ConditionalExpr, *hclsyntax.SplatExpr:
		rejected := node.Range()
		return hcl.Diagnostics{&hcl.Diagnostic{Severity: hcl.DiagError, Summary: "unsupported project expression", Subject: &rejected}}
	case *hclsyntax.ObjectConsExpr:
		seen := map[string]bool{}
		for _, item := range node.Items {
			key, diags := item.KeyExpr.Value(ctx)
			if diags.HasErrors() || !key.IsKnown() || key.IsNull() || key.Type() != cty.String {
				continue
			}
			if seen[key.AsString()] {
				at := item.KeyExpr.Range()
				return hcl.Diagnostics{&hcl.Diagnostic{Severity: hcl.DiagError, Summary: "duplicate object field", Subject: &at}}
			}
			seen[key.AsString()] = true
		}
	}
	return nil
}

func plainHCLValue(value cty.Value) (any, error) {
	raw, err := ctyjson.Marshal(value, value.Type())
	if err != nil {
		return nil, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	var plain any
	err = decoder.Decode(&plain)
	return plain, err
}

func collectHCLLocations(expression hclsyntax.Expression, field string, locations map[string]SourceRange) {
	locations[field] = sourceRangeHCL(expression.Range())
	switch expression := expression.(type) {
	case *hclsyntax.ObjectConsExpr:
		for _, item := range expression.Items {
			key, diags := item.KeyExpr.Value(nil)
			if !diags.HasErrors() && key.Type() == cty.String && !key.IsNull() {
				collectHCLLocations(item.ValueExpr, field+"/"+pointerPart(key.AsString()), locations)
			}
		}
	case *hclsyntax.TupleConsExpr:
		for i, item := range expression.Exprs {
			collectHCLLocations(item, fmt.Sprintf("%s/%d", field, i), locations)
		}
	}
}

func relocateProjectError(err error, locations map[string]SourceRange) error {
	located, ok := err.(*LocatedError)
	if !ok {
		return err
	}
	field := located.Field
	for {
		if at, ok := locations[field]; ok {
			located.Range = at
			return located
		}
		index := strings.LastIndex(field, "/")
		if index < 0 {
			break
		}
		field = field[:index]
	}
	return located
}

func checkHCLBounds(data []byte, filename string) error {
	tokens, diags := hclsyntax.LexConfig(data, filename, hcl.InitialPos)
	if diags.HasErrors() {
		return diagnosticHCL(diags)
	}
	depth := 0
	for i, token := range tokens {
		switch token.Type {
		case hclsyntax.TokenOBrace, hclsyntax.TokenOBrack, hclsyntax.TokenOParen, hclsyntax.TokenOQuote, hclsyntax.TokenOHeredoc, hclsyntax.TokenTemplateInterp, hclsyntax.TokenTemplateControl:
			depth++
		case hclsyntax.TokenCBrace, hclsyntax.TokenCBrack, hclsyntax.TokenCParen, hclsyntax.TokenCQuote, hclsyntax.TokenCHeredoc, hclsyntax.TokenTemplateSeqEnd:
			depth--
		}
		if i >= maxProjectNodes || depth > 32 {
			return locatedHCL(token.Range, "", "project structure exceeds parsing limits")
		}
	}
	return nil
}
