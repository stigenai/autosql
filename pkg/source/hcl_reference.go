package source

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"autosql/pkg/schema"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/zclconf/go-cty/cty"
)

const hclReferenceKey = "__autosql_reference"

type hclReference struct {
	ID     string      `json:"id"`
	Kind   schema.Kind `json:"kind"`
	Schema string      `json:"schema,omitempty"`
	Parent string      `json:"parent,omitempty"`
	Name   string      `json:"name"`
}

func (r hclReference) qualifiedName() string {
	if r.Schema == "" {
		return r.Name
	}
	return r.Schema + "." + r.Name
}

func (r hclReference) value(children map[string]any) map[string]any {
	encoded, _ := json.Marshal(r)
	value := map[string]any{hclReferenceKey: string(encoded)}
	for name, child := range children {
		value[name] = child
	}
	return value
}

func referenceFromAny(value any) (hclReference, bool) {
	object, ok := value.(map[string]any)
	if !ok {
		return hclReference{}, false
	}
	raw, ok := object[hclReferenceKey].(string)
	if !ok {
		return hclReference{}, false
	}
	var ref hclReference
	if json.Unmarshal([]byte(raw), &ref) != nil || ref.ID == "" || ref.Name == "" {
		return hclReference{}, false
	}
	return ref, true
}

type hclSymbolTable struct {
	roots    map[string]any
	byParent map[string]map[string]any
	byID     map[string]hclReference
	ordinals map[string]int
}

type hclSymbol struct {
	ref    hclReference
	parent string
}

func buildHCLSymbolTable(body *hclsyntax.Body, data []byte, variables HCLVariables) (*hclSymbolTable, error) {
	var symbols []hclSymbol
	if err := collectHCLSymbols(body, data, variables, &symbols); err != nil {
		return nil, err
	}
	return newHCLSymbolTable(symbols), nil
}

type hclSymbolSource struct {
	body      *hclsyntax.Body
	data      []byte
	variables HCLVariables
}

func buildHCLSymbolTableSources(sources []hclSymbolSource) (*hclSymbolTable, error) {
	var symbols []hclSymbol
	for _, source := range sources {
		if err := collectHCLSymbols(source.body, source.data, source.variables, &symbols); err != nil {
			return nil, err
		}
	}
	return newHCLSymbolTable(symbols), nil
}

func collectHCLSymbols(body *hclsyntax.Body, data []byte, variables HCLVariables, symbols *[]hclSymbol) error {
	var walk func(*hclsyntax.Body, string, string, schema.Kind, string) error
	walk = func(current *hclsyntax.Body, parent, inheritedSchema string, parentKind schema.Kind, parentName string) error {
		blocks := append([]*hclsyntax.Block(nil), current.Blocks...)
		sort.SliceStable(blocks, func(i, j int) bool { return blocks[i].DefRange().Start.Byte < blocks[j].DefRange().Start.Byte })
		for _, block := range blocks {
			if inlineHCLBlock(parentKind, block.Type) {
				continue
			}
			if block.Type == "import" || block.Type == "module" || block.Type == "variable" || block.Type == "output" || block.Type == "document" || block.Type == "locals" || block.Type == "moved" {
				continue
			}
			kind, name, err := hclBlockIdentity(block, parentName)
			if err != nil {
				return err
			}
			if kind == "resource" {
				if len(block.Labels) != 2 {
					return fmt.Errorf("%w: resource needs kind and name", ErrHCL)
				}
				kind, name = block.Labels[0], block.Labels[1]
			}
			if !schema.IsKnownKind(schema.Kind(kindToSchema(kind))) {
				continue
			}
			sk := kindToSchema(kind)
			scope := hclBlockEvaluationSymbols(block, data, variables, nil)
			schemaName, err := hclBlockSchema(block.Body, data, variables, scope, inheritedSchema)
			if err != nil {
				return err
			}
			nameObject := schema.Name{Name: name, Parent: parent, Schema: schemaName}
			if sk == schema.KindSchema {
				nameObject = schema.Name{Name: name}
				schemaName = name
			} else if parent == "" && schemaName != "" {
				nameObject.Parent = schema.StableID(schema.KindSchema, schema.Name{Name: schemaName})
			}
			id := schema.StableID(sk, nameObject)
			*symbols = append(*symbols, hclSymbol{ref: hclReference{ID: id, Kind: sk, Schema: nameObject.Schema, Parent: nameObject.Parent, Name: nameObject.Name}, parent: parent})
			if err := walk(block.Body, id, schemaName, sk, name); err != nil {
				return err
			}
		}
		return nil
	}
	return walk(body, "", "", "", "")
}

func newHCLSymbolTable(symbols []hclSymbol) *hclSymbolTable {
	table := &hclSymbolTable{roots: map[string]any{}, byParent: map[string]map[string]any{}, byID: map[string]hclReference{}, ordinals: map[string]int{}}
	byKind := map[schema.Kind][]hclReference{}
	children := map[string]map[string]map[string]any{}
	columnCounts := map[string]int{}
	for _, symbol := range symbols {
		table.byID[symbol.ref.ID] = symbol.ref
		byKind[symbol.ref.Kind] = append(byKind[symbol.ref.Kind], symbol.ref)
		if symbol.ref.Kind == schema.KindColumn {
			columnCounts[symbol.parent]++
			table.ordinals[symbol.ref.ID] = columnCounts[symbol.parent]
		}
		if symbol.parent != "" {
			if children[symbol.parent] == nil {
				children[symbol.parent] = map[string]map[string]any{}
			}
			root := string(symbol.ref.Kind)
			if children[symbol.parent][root] == nil {
				children[symbol.parent][root] = map[string]any{}
			}
			children[symbol.parent][root][symbol.ref.Name] = symbol.ref.value(nil)
			if table.byParent[symbol.parent] == nil {
				table.byParent[symbol.parent] = map[string]any{}
			}
			local, _ := table.byParent[symbol.parent][root].(map[string]any)
			if local == nil {
				local = map[string]any{}
				table.byParent[symbol.parent][root] = local
			}
			local[symbol.ref.Name] = symbol.ref.value(nil)
		}
	}

	for kind, refs := range byKind {
		rootName := string(kind)
		root := map[string]any{}
		nameCounts := map[string]int{}
		for _, ref := range refs {
			nameCounts[ref.Name]++
		}
		for _, ref := range refs {
			nested := map[string]any{}
			if ref.Kind == schema.KindTable || ref.Kind == schema.KindView || ref.Kind == schema.KindMaterializedView {
				for childKind, values := range children[ref.ID] {
					nested[childKind] = values
				}
			}
			node := ref.value(nested)
			if nameCounts[ref.Name] == 1 {
				root[ref.Name] = node
			}
			if ref.Schema != "" {
				qualified, _ := root[ref.Schema].(map[string]any)
				if qualified == nil || referenceObject(qualified) {
					qualified = map[string]any{}
					root[ref.Schema] = qualified
				}
				qualified[ref.Name] = node
			}
		}
		if len(root) > 0 {
			table.roots[rootName] = root
		}
	}
	return table
}

func (s *hclSymbolTable) reference(id string) hclReference {
	return s.byID[id]
}

func (s *hclSymbolTable) ordinal(id string) int {
	return s.ordinals[id]
}

func referenceObject(value map[string]any) bool {
	_, ok := value[hclReferenceKey]
	return ok
}

func hclBlockSchema(body *hclsyntax.Body, data []byte, variables HCLVariables, symbols map[string]any, inherited string) (string, error) {
	attribute, ok := body.Attributes["schema"]
	if !ok {
		return inherited, nil
	}
	if traversal, diagnostics := hcl.AbsTraversalForExpr(attribute.Expr); !diagnostics.HasErrors() && traversal.RootName() == "schema" {
		parts := traversalNames(traversal)
		if len(parts) == 2 {
			return parts[1], nil
		}
	}
	value, err := expressionValueWithSymbols(attribute.Expr, data, variables, symbols)
	if err != nil {
		return "", err
	}
	name, ok := value.(string)
	if !ok {
		return "", fmt.Errorf("%w: schema must be a string or schema reference", ErrHCL)
	}
	return strings.TrimPrefix(name, "schema."), nil
}

func traversalNames(traversal hcl.Traversal) []string {
	parts := []string{traversal.RootName()}
	for _, step := range traversal[1:] {
		switch value := step.(type) {
		case hcl.TraverseAttr:
			parts = append(parts, value.Name)
		case hcl.TraverseIndex:
			if value.Key.Type() != cty.String || !value.Key.IsKnown() {
				return nil
			}
			parts = append(parts, value.Key.AsString())
		default:
			return nil
		}
	}
	return parts
}

// stableIDFromHCLTraversal resolves the address side of a moved block even
// when the old resource is intentionally absent from the desired graph.
func stableIDFromHCLTraversal(traversal hcl.Traversal) string {
	parts := traversalNames(traversal)
	if len(parts) < 2 {
		return ""
	}
	kind := kindToSchema(parts[0])
	if !schema.IsKnownKind(kind) {
		return ""
	}
	if kind == schema.KindSchema || kind == schema.KindRole {
		if len(parts) != 2 {
			return ""
		}
		return schema.StableID(kind, schema.Name{Name: parts[1]})
	}
	if kind == schema.KindColumn {
		return ""
	}
	if parts[0] == string(schema.KindTable) && len(parts) == 5 && parts[3] == "column" {
		schemaName, tableName, columnName := parts[1], parts[2], parts[4]
		schemaID := schema.StableID(schema.KindSchema, schema.Name{Name: schemaName})
		tableID := schema.StableID(schema.KindTable, schema.Name{Name: tableName, Schema: schemaName, Parent: schemaID})
		return schema.StableID(schema.KindColumn, schema.Name{Name: columnName, Schema: schemaName, Parent: tableID})
	}
	if len(parts) == 2 {
		return schema.StableID(kind, schema.Name{Name: parts[1]})
	}
	if len(parts) != 3 {
		return ""
	}
	schemaName, name := parts[1], parts[2]
	schemaID := schema.StableID(schema.KindSchema, schema.Name{Name: schemaName})
	return schema.StableID(kind, schema.Name{Name: name, Schema: schemaName, Parent: schemaID})
}

func (s *hclSymbolTable) variables(parent string) map[string]any {
	out := make(map[string]any, len(s.roots)+2)
	for name, value := range s.roots {
		out[name] = value
	}
	for name, value := range s.byParent[parent] {
		out[name] = value
	}
	return out
}

func containsHCLReferenceTraversal(expr hcl.Expression, symbols map[string]any) bool {
	for _, traversal := range expr.Variables() {
		if _, ok := symbols[traversal.RootName()]; ok {
			return true
		}
	}
	return false
}

func resolveHCLReferences(value any, attribute string, dependencies *[]schema.Dependency) (any, error) {
	if expression, ok := decodeHCLExpression(value); ok {
		for _, ref := range expression.References {
			appendHCLDependency(dependencies, schema.Dependency{Target: ref.ID, Type: schema.DependencyUses})
		}
		return expression.SQL, nil
	}
	if ref, ok := referenceFromAny(value); ok {
		dependencyType, err := referenceDependencyType(attribute, ref.Kind)
		if err != nil {
			return nil, err
		}
		appendHCLDependency(dependencies, schema.Dependency{Target: ref.ID, Type: dependencyType})
		return referenceSpecValue(attribute, ref), nil
	}
	switch typed := value.(type) {
	case []any:
		resolved := make([]any, len(typed))
		for index := range typed {
			item, err := resolveHCLReferences(typed[index], attribute, dependencies)
			if err != nil {
				return nil, err
			}
			resolved[index] = item
		}
		return resolved, nil
	case map[string]any:
		resolved := make(map[string]any, len(typed))
		for name, item := range typed {
			value, err := resolveHCLReferences(item, name, dependencies)
			if err != nil {
				return nil, err
			}
			resolved[name] = value
		}
		return resolved, nil
	default:
		return value, nil
	}
}

func referenceDependencyType(attribute string, kind schema.Kind) (schema.DependencyType, error) {
	switch attribute {
	case "schema":
		if kind != schema.KindSchema {
			return "", fmt.Errorf("%w: schema attribute requires a schema reference, got %s", ErrHCL, kind)
		}
		return schema.DependencyContains, nil
	case "type", "return", "returns", "return_set":
		switch kind {
		case schema.KindEnum, schema.KindDomain, schema.KindComposite, schema.KindTable, schema.KindView, schema.KindMaterializedView, schema.KindExtension, schema.KindFunction, schema.KindProcedure:
			return schema.DependencyUses, nil
		default:
			return "", fmt.Errorf("%w: %s attribute cannot reference %s", ErrHCL, attribute, kind)
		}
	case "depends_on":
		return schema.DependencyUses, nil
	case "owner":
		if kind != schema.KindRole {
			return "", fmt.Errorf("%w: owner attribute requires a role reference, got %s", ErrHCL, kind)
		}
		return schema.DependencyOwns, nil
	case "extension":
		if kind != schema.KindExtension {
			return "", fmt.Errorf("%w: extension attribute requires an extension reference, got %s", ErrHCL, kind)
		}
		return schema.DependencyOwns, nil
	case "columns", "ref_columns", "column":
		if kind != schema.KindColumn {
			return "", fmt.Errorf("%w: %s attribute requires column references, got %s", ErrHCL, attribute, kind)
		}
	}
	return schema.DependencyReferences, nil
}

func referenceSpecValue(attribute string, ref hclReference) any {
	switch attribute {
	case "schema", "owner", "grantor", "grantee", "member", "role", "roles", "to":
		return ref.Name
	case "type", "return", "returns", "return_set":
		return ref.qualifiedName()
	default:
		return ref.ID
	}
}

func appendHCLDependency(dependencies *[]schema.Dependency, dependency schema.Dependency) {
	for _, existing := range *dependencies {
		if existing.Target == dependency.Target && existing.Type == dependency.Type {
			return
		}
	}
	*dependencies = append(*dependencies, dependency)
}
