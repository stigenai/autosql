package postgres

import (
	"fmt"
	"sort"
	"strings"

	"autosql/pkg/schema"
	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func expressionRoutineDependencies(root protoreflect.Message, defaultSchema string, resource schema.Resource, resources map[string]schema.Resource) ([]string, error) {
	calls := map[string]defaultReference{}
	var walk func(protoreflect.Message)
	walk = func(message protoreflect.Message) {
		if !message.IsValid() {
			return
		}
		if call, ok := message.Interface().(*pg_query.FuncCall); ok {
			parts := make([]string, 0, len(call.GetFuncname()))
			for _, node := range call.GetFuncname() {
				if value := node.GetString_(); value != nil {
					parts = append(parts, value.GetSval())
				}
			}
			if len(parts) > 0 {
				key := fmt.Sprint(parts)
				calls[key] = defaultReference{Parts: parts}
			}
		}
		message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
			switch {
			case field.IsList() && field.Kind() == protoreflect.MessageKind:
				list := value.List()
				for index := 0; index < list.Len(); index++ {
					walk(list.Get(index).Message())
				}
			case field.Kind() == protoreflect.MessageKind:
				walk(value.Message())
			}
			return true
		})
	}
	walk(root)
	var expected []string
	for _, call := range calls {
		var matches []string
		for id, candidate := range resources {
			if candidate.Kind == schema.KindFunction && generatedFunctionReferenceMatches(call, defaultSchema, candidate) {
				matches = append(matches, id)
			}
		}
		if len(matches) == 0 {
			name := strings.Join(call.Parts, ".")
			if !immutableBuiltinExpressionFunction(name) {
				return nil, unsupported(resource, "expression routine is undeclared or outside the immutable built-in allowlist")
			}
			continue
		}
		if len(matches) != 1 {
			return nil, unsupported(resource, "expression routine overload is ambiguous")
		}
		expected = append(expected, matches[0])
	}
	sort.Strings(expected)
	return expected, nil
}

func immutableBuiltinExpressionFunction(name string) bool {
	name = strings.TrimPrefix(name, "pg_catalog.")
	return map[string]bool{
		"abs": true, "btrim": true, "char_length": true, "length": true,
		"lower": true, "ltrim": true, "octet_length": true, "rtrim": true,
		"substring": true, "upper": true,
	}[name]
}

func expressionTypeDependencies(root protoreflect.Message, defaultSchema string, resource schema.Resource, resources map[string]schema.Resource) ([]string, error) {
	seen := map[string]bool{}
	var dependencyErr error
	walkPostgresMessages(root, func(message protoreflect.Message) {
		if dependencyErr != nil {
			return
		}
		cast, ok := message.Interface().(*pg_query.TypeCast)
		if !ok || cast.GetTypeName() == nil {
			return
		}
		target, matched, err := expressionTypeTarget(cast.GetTypeName(), defaultSchema, resource, resources)
		if err != nil {
			dependencyErr = err
			return
		}
		if matched {
			seen[target.ID] = true
		}
	})
	if dependencyErr != nil {
		return nil, dependencyErr
	}
	expected := make([]string, 0, len(seen))
	for id := range seen {
		expected = append(expected, id)
	}
	sort.Strings(expected)
	return expected, nil
}

func qualifyExpressionTypeCasts(root protoreflect.Message, defaultSchema string, resource schema.Resource, resources map[string]schema.Resource) error {
	declared := map[string]bool{}
	for _, dependency := range resource.Dependencies {
		if dependency.Type == schema.DependencyUses {
			declared[dependency.Target] = true
		}
	}
	var qualificationErr error
	walkPostgresMessages(root, func(message protoreflect.Message) {
		if qualificationErr != nil {
			return
		}
		cast, ok := message.Interface().(*pg_query.TypeCast)
		if !ok || cast.GetTypeName() == nil {
			return
		}
		target, matched, err := expressionTypeTarget(cast.GetTypeName(), defaultSchema, resource, resources)
		if err != nil {
			qualificationErr = err
			return
		}
		if !matched {
			return
		}
		if !declared[target.ID] {
			qualificationErr = unsupported(resource, "expression type cast is missing its exact uses dependency")
			return
		}
		cast.TypeName.Names = []*pg_query.Node{
			{Node: &pg_query.Node_String_{String_: &pg_query.String{Sval: target.Name.Schema}}},
			{Node: &pg_query.Node_String_{String_: &pg_query.String{Sval: target.Name.Name}}},
		}
	})
	return qualificationErr
}

func expressionTypeTarget(typeName *pg_query.TypeName, defaultSchema string, resource schema.Resource, resources map[string]schema.Resource) (schema.Resource, bool, error) {
	parts := make([]string, 0, len(typeName.GetNames()))
	for _, node := range typeName.GetNames() {
		value := node.GetString_()
		if value == nil {
			return schema.Resource{}, false, unsupported(resource, "expression type cast has a non-identifier target")
		}
		parts = append(parts, value.GetSval())
	}
	if len(parts) == 0 || len(parts) > 2 {
		return schema.Resource{}, false, unsupported(resource, "expression type cast target is not canonical")
	}
	quoted := make([]string, len(parts))
	for index, part := range parts {
		quoted[index] = quote(part)
	}
	reference := strings.Join(quoted, ".")
	for range typeName.GetArrayBounds() {
		reference += "[]"
	}
	var matches []schema.Resource
	for _, candidate := range resources {
		if candidate.Kind != schema.KindEnum && candidate.Kind != schema.KindDomain && candidate.Kind != schema.KindComposite {
			continue
		}
		if typeReferenceMatches(reference, defaultSchema, candidate.Name) {
			matches = append(matches, candidate)
		}
	}
	if len(matches) > 1 {
		return schema.Resource{}, false, unsupported(resource, "expression type cast dependency is ambiguous")
	}
	if len(matches) == 1 {
		return matches[0], true, nil
	}
	return schema.Resource{}, false, nil
}

func rejectUnsafeExpressionNodes(root protoreflect.Message, resource schema.Resource) error {
	unsafe := ""
	var walk func(protoreflect.Message)
	walk = func(message protoreflect.Message) {
		if !message.IsValid() || unsafe != "" {
			return
		}
		name := string(message.Descriptor().Name())
		switch name {
		case "SelectStmt", "SubLink", "ParamRef", "CurrentOfExpr", "NextValueExpr":
			unsafe = name
			return
		}
		if strings.HasPrefix(name, "Json") {
			unsafe = name
			return
		}
		message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
			switch {
			case field.IsList() && field.Kind() == protoreflect.MessageKind:
				list := value.List()
				for index := 0; index < list.Len(); index++ {
					walk(list.Get(index).Message())
				}
			case field.Kind() == protoreflect.MessageKind:
				walk(value.Message())
			}
			return unsafe == ""
		})
	}
	walk(root)
	if unsafe != "" {
		return unsupported(resource, "expression contains unsafe node "+unsafe)
	}
	return nil
}
