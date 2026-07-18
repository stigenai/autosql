package postgres

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"autosql/pkg/schema"
	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

type parsedViewDefinition struct {
	tree  *pg_query.ParseResult
	query *pg_query.SelectStmt
	SQL   string
}

func parseViewDefinition(definition string) (parsedViewDefinition, error) {
	if err := validateSQLFragment(definition); err != nil {
		return parsedViewDefinition{}, err
	}
	parsed, err := pg_query.Parse(definition)
	if err != nil || parsed == nil || len(parsed.Stmts) != 1 {
		return parsedViewDefinition{}, err
	}
	query := parsed.Stmts[0].GetStmt().GetSelectStmt()
	if query == nil {
		return parsedViewDefinition{}, fmt.Errorf("definition must parse as exactly one query statement")
	}
	return parsedViewDefinition{tree: parsed, query: query, SQL: strings.TrimSuffix(strings.TrimSpace(definition), ";")}, nil
}

func schemaBindViewDefinition(view schema.Resource, resources map[string]schema.Resource) (string, error) {
	parsed, err := parseViewDefinition(stringValue(spec(view), "definition"))
	if err != nil {
		return "", unsupported(view, "view definition is not one parser-proven query")
	}
	cteNames := map[string]bool{}
	var relations []*pg_query.RangeVar
	walkPostgresMessages(parsed.query.ProtoReflect(), func(message protoreflect.Message) {
		switch value := message.Interface().(type) {
		case *pg_query.CommonTableExpr:
			cteNames[value.GetCtename()] = true
		case *pg_query.RangeVar:
			relations = append(relations, value)
		}
	})
	for _, relation := range relations {
		if relation.GetCatalogname() != "" || relation.GetRelname() == "" {
			return "", unsupported(view, "view relation uses unsupported qualification")
		}
		if relation.GetSchemaname() == "" && cteNames[relation.GetRelname()] {
			if viewDeclaredRelationMatches(view, relation.GetRelname(), resources) {
				return "", unsupported(view, "view CTE name collides with a declared relation dependency")
			}
			continue
		}
		target, targetErr := viewRelationDependency(view, relation, resources)
		if targetErr != nil {
			return "", targetErr
		}
		relation.Schemaname = target.Name.Schema
		relation.Relname = target.Name.Name
	}
	canonicalizeSingleRelationColumnQualifiers(parsed.query, relations, cteNames)
	if err := qualifyExpressionTypeCasts(parsed.query.ProtoReflect(), view.Name.Schema, view, resources); err != nil {
		return "", err
	}
	canonical, err := pg_query.Deparse(parsed.tree)
	if err != nil {
		return "", unsupported(view, "view definition cannot be rendered canonically")
	}
	return strings.TrimSuffix(strings.TrimSpace(canonical), ";"), nil
}

func canonicalizeSingleRelationColumnQualifiers(query *pg_query.SelectStmt, relations []*pg_query.RangeVar, cteNames map[string]bool) {
	var relation *pg_query.RangeVar
	for _, candidate := range relations {
		if candidate.GetSchemaname() == "" && cteNames[candidate.GetRelname()] {
			continue
		}
		if relation != nil {
			return
		}
		relation = candidate
	}
	if relation == nil {
		return
	}
	qualifiers := map[string]bool{relation.GetRelname(): true}
	if relation.GetAlias() != nil && relation.GetAlias().GetAliasname() != "" {
		qualifiers[relation.GetAlias().GetAliasname()] = true
	}
	walkPostgresMessages(query.ProtoReflect(), func(message protoreflect.Message) {
		column, ok := message.Interface().(*pg_query.ColumnRef)
		if !ok || len(column.GetFields()) != 2 {
			return
		}
		qualifier := column.GetFields()[0].GetString_()
		name := column.GetFields()[1].GetString_()
		if qualifier == nil || name == nil || !qualifiers[qualifier.GetSval()] {
			return
		}
		column.Fields = column.Fields[1:]
	})
}

func viewDeclaredRelationMatches(view schema.Resource, name string, resources map[string]schema.Resource) bool {
	for _, dependency := range view.Dependencies {
		target := resources[dependency.Target]
		if dependency.Type == schema.DependencyReferences && managedViewRelation(target) && target.Name.Name == name {
			return true
		}
	}
	return false
}

func viewRelationDependency(view schema.Resource, relation *pg_query.RangeVar, resources map[string]schema.Resource) (schema.Resource, error) {
	seen := map[string]bool{}
	var matches []schema.Resource
	for _, dependency := range view.Dependencies {
		target, ok := resources[dependency.Target]
		if dependency.Type != schema.DependencyReferences || !ok || !managedViewRelation(target) || seen[target.ID] {
			continue
		}
		if target.Name.Name != relation.GetRelname() || relation.GetSchemaname() != "" && target.Name.Schema != relation.GetSchemaname() {
			continue
		}
		seen[target.ID] = true
		matches = append(matches, target)
	}
	if len(matches) != 1 {
		return schema.Resource{}, unsupported(view, "view relation dependency is missing or ambiguous")
	}
	return matches[0], nil
}

func managedViewRelation(resource schema.Resource) bool {
	return resource.Kind == schema.KindTable || resource.Kind == schema.KindView || resource.Kind == schema.KindMaterializedView
}

func viewExpectedDependencies(view schema.Resource, resources map[string]schema.Resource) ([]schema.Dependency, error) {
	parsed, err := parseViewDefinition(stringValue(spec(view), "definition"))
	if err != nil {
		return nil, unsupported(view, "view definition is not one parser-proven query")
	}
	cteNames := map[string]bool{}
	var relations []*pg_query.RangeVar
	walkPostgresMessages(parsed.query.ProtoReflect(), func(message protoreflect.Message) {
		switch value := message.Interface().(type) {
		case *pg_query.CommonTableExpr:
			cteNames[value.GetCtename()] = true
		case *pg_query.RangeVar:
			relations = append(relations, value)
		}
	})
	byKey := map[string]schema.Dependency{}
	for _, relation := range relations {
		if relation.GetSchemaname() == "" && cteNames[relation.GetRelname()] {
			if viewDeclaredRelationMatches(view, relation.GetRelname(), resources) {
				return nil, unsupported(view, "view CTE name collides with a declared relation dependency")
			}
			continue
		}
		target, targetErr := viewRelationDependency(view, relation, resources)
		if targetErr != nil {
			return nil, targetErr
		}
		dependency := schema.Dependency{Target: target.ID, Type: schema.DependencyReferences}
		byKey[string(dependency.Type)+":"+dependency.Target] = dependency
	}
	types, err := expressionTypeDependencies(parsed.query.ProtoReflect(), view.Name.Schema, view, resources)
	if err != nil {
		return nil, err
	}
	for _, target := range types {
		dependency := schema.Dependency{Target: target, Type: schema.DependencyUses}
		byKey[string(dependency.Type)+":"+dependency.Target] = dependency
	}
	expected := make([]schema.Dependency, 0, len(byKey))
	for _, dependency := range byKey {
		expected = append(expected, dependency)
	}
	sort.Slice(expected, func(i, j int) bool {
		if expected[i].Type != expected[j].Type {
			return expected[i].Type < expected[j].Type
		}
		return expected[i].Target < expected[j].Target
	})
	return expected, nil
}

func validateViewDependencies(view schema.Resource, resources map[string]schema.Resource) error {
	expected, err := viewExpectedDependencies(view, resources)
	if err != nil {
		return err
	}
	var actual []schema.Dependency
	for _, dependency := range view.Dependencies {
		if dependency.Type != schema.DependencyReferences && dependency.Type != schema.DependencyUses {
			continue
		}
		actual = append(actual, schema.Dependency{Target: dependency.Target, Type: dependency.Type})
	}
	sort.Slice(actual, func(i, j int) bool {
		if actual[i].Type != actual[j].Type {
			return actual[i].Type < actual[j].Type
		}
		return actual[i].Target < actual[j].Target
	})
	if len(expected) != len(actual) {
		return unsupported(view, "declared dependencies do not exactly match rendered view semantics")
	}
	for index := range expected {
		if expected[index].Target != actual[index].Target || expected[index].Type != actual[index].Type {
			return unsupported(view, "declared dependencies do not exactly match rendered view semantics")
		}
	}
	return nil
}

func canonicalizeViewBindings(doc *schema.Document) error {
	resources := make(map[string]schema.Resource, len(doc.Graph.Resources))
	for _, resource := range doc.Graph.Resources {
		resources[resource.ID] = resource
	}
	for index := range doc.Graph.Resources {
		resource := &doc.Graph.Resources[index]
		if resource.Kind != schema.KindView && resource.Kind != schema.KindMaterializedView {
			continue
		}
		parsed, err := parseViewDefinition(stringValue(spec(*resource), "definition"))
		if err != nil {
			continue
		}
		relations, err := capturedViewDependencies(*resource, resources)
		if err != nil {
			return err
		}
		for _, target := range relations {
			exists := false
			for _, dependency := range resource.Dependencies {
				exists = exists || dependency.Target == target && dependency.Type == schema.DependencyReferences
			}
			if !exists {
				resource.Dependencies = append(resource.Dependencies, schema.Dependency{Target: target, Type: schema.DependencyReferences})
			}
		}
		types, err := expressionTypeDependencies(parsed.query.ProtoReflect(), resource.Name.Schema, *resource, resources)
		if err != nil {
			return err
		}
		for _, target := range types {
			exists := false
			for _, dependency := range resource.Dependencies {
				exists = exists || dependency.Target == target && dependency.Type == schema.DependencyUses
			}
			if !exists {
				resource.Dependencies = append(resource.Dependencies, schema.Dependency{Target: target, Type: schema.DependencyUses})
			}
		}
		resources[resource.ID] = *resource
		definition, err := schemaBindViewDefinition(*resource, resources)
		if err != nil {
			return err
		}
		values := specMap(resource.Spec)
		values["definition"] = definition
		resource.Spec, err = json.Marshal(values)
		if err != nil {
			return err
		}
		resources[resource.ID] = *resource
	}
	return nil
}
