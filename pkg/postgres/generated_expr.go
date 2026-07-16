package postgres

import (
	"errors"
	"fmt"
	"slices"
	"sort"
	"strings"

	"autosql/pkg/schema"
)

type generatedExpressionAnalysis struct {
	Columns   []defaultReference
	Functions []defaultReference
}

func analyzeGeneratedExpression(value string) (generatedExpressionAnalysis, error) {
	expression, err := classifyDefaultExpression(value)
	if err != nil {
		return generatedExpressionAnalysis{}, fmt.Errorf("generated expression: %w", err)
	}
	analysis := generatedExpressionAnalysis{}
	var walk func(defaultExpression) error
	walk = func(node defaultExpression) error {
		switch node.Kind {
		case defaultExpressionLiteral:
			return nil
		case defaultExpressionReference:
			if node.Reference == nil {
				return errors.New("generated expression contains an empty column reference")
			}
			analysis.Columns = append(analysis.Columns, *node.Reference)
			return nil
		case defaultExpressionFunction:
			if node.Function == nil || node.Function.Precision != nil {
				return errors.New("generated expression contains an unsupported SQL value function")
			}
			analysis.Functions = append(analysis.Functions, node.Function.Name)
			for _, argument := range node.Function.Arguments {
				if err := walk(argument); err != nil {
					return err
				}
			}
			return nil
		case defaultExpressionCast:
			if node.Cast == nil {
				return errors.New("generated expression contains an empty cast")
			}
			return walk(node.Cast.Expression)
		case defaultExpressionArray:
			for _, element := range node.Array {
				if err := walk(element); err != nil {
					return err
				}
			}
			return nil
		default:
			return errors.New("generated expression contains an unsupported node")
		}
	}
	if err := walk(expression); err != nil {
		return generatedExpressionAnalysis{}, err
	}
	return analysis, nil
}

func validateGeneratedDependencies(resource schema.Resource, resources map[string]schema.Resource) error {
	values := spec(resource)
	if stringValue(values, "generated") == "" {
		return nil
	}
	if stringValue(values, "generated") != "s" || stringValue(values, "default") == "" {
		return unsupported(resource, "only stored generated columns with an expression are supported")
	}
	expected, err := expectedGeneratedDependencies(resource, resources)
	if err != nil {
		return err
	}
	actual := []string{}
	for _, dependency := range resource.Dependencies {
		if dependency.Type == schema.DependencyReferences {
			actual = append(actual, dependency.Target)
		}
	}
	want := make([]string, 0, len(expected))
	for id := range expected {
		want = append(want, id)
	}
	sort.Strings(actual)
	sort.Strings(want)
	if !slices.Equal(actual, want) {
		return unsupported(resource, "generated expression dependencies are not exact")
	}
	return nil
}

func validateDefaultRoutineDependencies(resource schema.Resource, resources map[string]schema.Resource) error {
	values := spec(resource)
	if stringValue(values, "generated") != "" || stringValue(values, "default") == "" {
		return nil
	}
	analysis, err := analyzeGeneratedExpression(stringValue(values, "default"))
	if err != nil {
		// The default grammar performs its own bounded validation. Only enforce
		// application routine edges when the expression analyzer supports it.
		return nil
	}
	var expected []string
	for _, call := range analysis.Functions {
		var matches []string
		for id, candidate := range resources {
			if candidate.Kind == schema.KindFunction && generatedFunctionReferenceMatches(call, resource.Name.Schema, candidate) {
				matches = append(matches, id)
			}
		}
		if len(matches) == 0 {
			continue
		}
		if len(matches) != 1 {
			return unsupported(resource, "default routine reference is ambiguous")
		}
		expected = append(expected, matches[0])
	}
	var actual []string
	for _, dependency := range resource.Dependencies {
		if candidate, ok := resources[dependency.Target]; ok && dependency.Type == schema.DependencyReferences && candidate.Kind == schema.KindFunction {
			actual = append(actual, dependency.Target)
		}
	}
	sort.Strings(expected)
	sort.Strings(actual)
	if !slices.Equal(expected, actual) {
		return unsupported(resource, "default routine dependencies are not exact")
	}
	return nil
}

func validateGeneratedColumnCreate(resource schema.Resource, resources map[string]schema.Resource) error {
	values := spec(resource)
	if stringValue(values, "generated") != "s" || stringValue(values, "default") == "" {
		return unsupported(resource, "only STORED generated columns with one expression are supported")
	}
	if stringValue(values, "identity") != "" {
		return unsupported(resource, "generated columns cannot also be identity columns")
	}
	if err := validateGeneratedDependencies(resource, resources); err != nil {
		return err
	}
	expression, err := classifyDefaultExpression(stringValue(values, "default"))
	if err != nil {
		return unsupported(resource, "generated expression rejected: "+err.Error())
	}
	if err := validateGeneratedExpressionNode(expression); err != nil {
		return unsupported(resource, "generated expression rejected: "+err.Error())
	}
	for _, dependency := range resource.Dependencies {
		routine, ok := resources[dependency.Target]
		if dependency.Type != schema.DependencyReferences || !ok || routine.Kind != schema.KindFunction {
			continue
		}
		routineSpec := spec(routine)
		if stringValue(routineSpec, "volatility") != "i" || stringValue(routineSpec, "language") != "sql" || boolValue(routineSpec, "security_definer") {
			return unsupported(resource, "generated routine must be an immutable SQL invoker-security function")
		}
	}
	return nil
}

func validateGeneratedExpressionNode(expression defaultExpression) error {
	switch expression.Kind {
	case defaultExpressionLiteral, defaultExpressionReference:
		return nil
	case defaultExpressionFunction:
		if expression.Function == nil || expression.Function.Precision != nil {
			return errors.New("SQL value functions are not supported")
		}
		for _, argument := range expression.Function.Arguments {
			if err := validateGeneratedExpressionNode(argument); err != nil {
				return err
			}
		}
		return nil
	case defaultExpressionCast:
		if expression.Cast == nil {
			return errors.New("cast is empty")
		}
		if _, ok := coreDefaultCastType(expression.Cast.Type); !ok {
			return errors.New("cast target is outside the canonical core type grammar")
		}
		return validateGeneratedExpressionNode(expression.Cast.Expression)
	case defaultExpressionArray:
		if len(expression.Array) == 0 {
			return errors.New("empty array expressions are not supported")
		}
		for _, element := range expression.Array {
			if err := validateGeneratedExpressionNode(element); err != nil {
				return err
			}
		}
		return nil
	default:
		return errors.New("AST node is outside the generated expression grammar")
	}
}

func expectedGeneratedDependencies(resource schema.Resource, resources map[string]schema.Resource) (map[string]bool, error) {
	analysis, err := analyzeGeneratedExpression(stringValue(spec(resource), "default"))
	if err != nil {
		return nil, unsupported(resource, err.Error())
	}
	parent := resources[resource.Name.Parent]
	expected := map[string]bool{}
	actualReferences := map[string]bool{}
	for _, dependency := range resource.Dependencies {
		if dependency.Type == schema.DependencyReferences {
			actualReferences[dependency.Target] = true
		}
	}
	for _, reference := range analysis.Columns {
		matches := []string{}
		for id, candidate := range resources {
			if candidate.Kind == schema.KindColumn && candidate.Name.Parent == resource.Name.Parent && generatedColumnReferenceMatches(reference, parent, candidate) {
				matches = append(matches, id)
			}
		}
		matches = preferDeclaredGeneratedDependency(matches, actualReferences)
		if len(matches) != 1 || matches[0] == resource.ID {
			return nil, unsupported(resource, "generated column reference is missing, recursive, or ambiguous")
		}
		expected[matches[0]] = true
	}
	for _, call := range analysis.Functions {
		matches := []string{}
		for id, candidate := range resources {
			if candidate.Kind == schema.KindFunction && generatedFunctionReferenceMatches(call, resource.Name.Schema, candidate) {
				matches = append(matches, id)
			}
		}
		// The expression AST does not yet perform PostgreSQL overload type
		// resolution. A declared edge cannot safely disambiguate multiple
		// same-name routines because PostgreSQL may bind the call to another
		// overload. Fail closed until the argument types prove one candidate.
		if len(matches) != 1 {
			return nil, unsupported(resource, "generated routine reference is missing or ambiguous")
		}
		expected[matches[0]] = true
	}
	return expected, nil
}

func preferDeclaredGeneratedDependency(matches []string, declared map[string]bool) []string {
	var preferred []string
	for _, match := range matches {
		if declared[match] {
			preferred = append(preferred, match)
		}
	}
	if len(preferred) > 0 {
		return preferred
	}
	return matches
}

func generatedColumnReferenceMatches(reference defaultReference, table, column schema.Resource) bool {
	parts := reference.Parts
	switch len(parts) {
	case 1:
		return parts[0] == column.Name.Name
	case 2:
		return parts[0] == table.Name.Name && parts[1] == column.Name.Name
	case 3:
		return parts[0] == table.Name.Schema && parts[1] == table.Name.Name && parts[2] == column.Name.Name
	default:
		return false
	}
}

func generatedFunctionReferenceMatches(reference defaultReference, columnSchema string, function schema.Resource) bool {
	name := stringValue(spec(function), "name")
	if name == "" {
		logical := function.Name.Name
		if open := strings.IndexByte(logical, '('); open >= 0 {
			logical = logical[:open]
		}
		name = logical
	}
	switch len(reference.Parts) {
	case 1:
		return function.Name.Schema == columnSchema && reference.Parts[0] == name
	case 2:
		return reference.Parts[0] == function.Name.Schema && reference.Parts[1] == name
	default:
		return false
	}
}
