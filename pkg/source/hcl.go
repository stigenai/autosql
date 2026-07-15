package source

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"autosql/pkg/schema"
	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
	"github.com/zclconf/go-cty/cty/function"
	"github.com/zclconf/go-cty/cty/function/stdlib"
)

var (
	ErrHCL            = errors.New("invalid AutoSQL HCL")
	ErrImportCycle    = errors.New("HCL import cycle")
	ErrImportLimit    = errors.New("HCL import depth exceeded")
	ErrUnknownHCLKind = errors.New("unsupported HCL resource kind")
)

// HCLVariables are evaluated before parsing. Values must be non-sensitive
// JSON-like values; secret references belong in the separate secret package.
type HCLVariables map[string]any

func ParseHCL(uri string, data []byte, variables HCLVariables) (schema.Document, error) {
	return ParseHCLContext(context.Background(), uri, data, variables)
}

func ParseHCLContext(ctx context.Context, uri string, data []byte, variables HCLVariables) (schema.Document, error) {
	file, diags := hclsyntaxParse(data, uri)
	if diags.HasErrors() {
		return schema.Document{}, fmt.Errorf("%w: %s", ErrHCL, diags.Error())
	}
	if err := ctx.Err(); err != nil {
		return schema.Document{}, err
	}
	doc, imports, err := hclDocument(ctx, uri, file.Body.(*hclsyntax.Body), data, variables)
	if err != nil {
		return schema.Document{}, err
	}
	if len(imports) > 0 {
		if doc.Annotations == nil {
			doc.Annotations = map[string]string{}
		}
		doc.Annotations["hcl_imports"] = strings.Join(imports, ",")
	}
	doc.Normalize()
	if err := doc.Validate(); err != nil {
		return schema.Document{}, err
	}
	return doc, nil
}

// HCLLoader resolves import/module blocks with cycle and depth protection.
type HCLLoader struct {
	ReadFile  func(string) ([]byte, error)
	MaxDepth  int
	Variables HCLVariables
}

func (l HCLLoader) Load(ctx context.Context, root string) (schema.Document, error) {
	read := l.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	max := l.MaxDepth
	if max <= 0 {
		max = 32
	}
	return l.load(ctx, root, read, max, map[string]bool{})
}

func (l HCLLoader) load(ctx context.Context, uri string, read func(string) ([]byte, error), remaining int, stack map[string]bool) (schema.Document, error) {
	if remaining <= 0 {
		return schema.Document{}, ErrImportLimit
	}
	canonical, err := filepath.Abs(uri)
	if err == nil {
		uri = canonical
	}
	if stack[uri] {
		return schema.Document{}, fmt.Errorf("%w: %s", ErrImportCycle, uri)
	}
	stack[uri] = true
	defer delete(stack, uri)
	data, err := read(uri)
	if err != nil {
		return schema.Document{}, err
	}
	file, diags := hclsyntaxParse(data, uri)
	if diags.HasErrors() {
		return schema.Document{}, fmt.Errorf("%w: %s", ErrHCL, diags.Error())
	}
	doc, imports, err := hclDocument(ctx, uri, file.Body.(*hclsyntax.Body), data, l.Variables)
	if err != nil {
		return schema.Document{}, err
	}
	for _, name := range imports {
		if err := ctx.Err(); err != nil {
			return schema.Document{}, err
		}
		child := name
		if !filepath.IsAbs(child) {
			child = filepath.Join(filepath.Dir(uri), child)
		}
		part, e := l.load(ctx, child, read, remaining-1, stack)
		if e != nil {
			return schema.Document{}, e
		}
		doc.Graph.Resources = append(doc.Graph.Resources, part.Graph.Resources...)
	}
	doc.Normalize()
	if err := doc.Validate(); err != nil {
		return schema.Document{}, err
	}
	return doc, nil
}

func hclsyntaxParse(data []byte, uri string) (*hcl.File, hcl.Diagnostics) {
	return hclsyntax.ParseConfig(data, uri, hcl.Pos{Line: 1, Column: 1})
}

func hclDocument(ctx context.Context, uri string, body *hclsyntax.Body, data []byte, variables HCLVariables) (schema.Document, []string, error) {
	doc := schema.Document{Version: schema.SchemaVersion}
	var imports []string
	var walk func(*hclsyntax.Body, string, string) error
	walk = func(current *hclsyntax.Body, parent, parentSchema string) error {
		blocks := append([]*hclsyntax.Block(nil), current.Blocks...)
		sort.SliceStable(blocks, func(i, j int) bool { return blocks[i].DefRange().Start.Byte < blocks[j].DefRange().Start.Byte })
		for _, b := range blocks {
			if err := ctx.Err(); err != nil {
				return err
			}
			if b.Type == "import" || b.Type == "module" {
				src, e := stringAttr(b.Body, "source", data, variables)
				if e != nil {
					return e
				}
				if src != "" {
					imports = append(imports, src)
				}
				continue
			}
			if b.Type == "variable" {
				continue
			}
			// Document-level metadata (e.g. the dialect annotation that
			// schema inspect stamps). FormatHCL emits this so a round-tripped
			// document compares equal in plan.Build's documentMetadataEqual.
			if b.Type == "document" {
				aj, e := stringAttr(b.Body, "annotations_json", data, variables)
				if e != nil {
					return e
				}
				if aj != "" {
					if err := json.Unmarshal([]byte(aj), &doc.Annotations); err != nil {
						return fmt.Errorf("%w: document annotations_json: %v", ErrHCL, err)
					}
				}
				continue
			}
			kind, name, err := blockIdentity(b)
			if err != nil {
				return err
			}
			spec, nested, err := attributesAndNested(b.Body, data, variables)
			if err != nil {
				return err
			}
			resourceForm := kind == "resource"
			// Resource form is a lossless escape hatch for every canonical kind
			// (this is what FormatHCL emits). Its identity attrs — schema,
			// parent, catalog — and its dependencies are captured verbatim so
			// FormatHCL -> ParseHCL reproduces an identical graph. They must be
			// read before spec_json replaces the block attribute map.
			var rfSchema, rfParent, rfCatalog, rfDeps, rfAnnotations string
			if resourceForm {
				if len(b.Labels) != 2 {
					return fmt.Errorf("%w: resource needs kind and name", ErrHCL)
				}
				kind = b.Labels[0]
				name = b.Labels[1]
				rfSchema, _ = spec["schema"].(string)
				rfParent, _ = spec["parent"].(string)
				rfCatalog, _ = spec["catalog"].(string)
				rfDeps, _ = spec["deps_json"].(string)
				rfAnnotations, _ = spec["annotations_json"].(string)
				if encoded, ok := spec["spec_json"].(string); ok {
					var decoded map[string]any
					decoder := json.NewDecoder(strings.NewReader(encoded))
					decoder.UseNumber()
					if err := decoder.Decode(&decoded); err != nil {
						return fmt.Errorf("%w: resource spec_json: %v", ErrHCL, err)
					}
					spec = decoded
				} else {
					delete(spec, "schema")
					delete(spec, "parent")
					delete(spec, "catalog")
					delete(spec, "deps_json")
					delete(spec, "annotations_json")
				}
				if !schema.IsKnownKind(schema.Kind(kind)) {
					return fmt.Errorf("%w: %s", ErrUnknownHCLKind, kind)
				}
			} else if !knownHCLKind(kind) {
				return fmt.Errorf("%w: %s", ErrUnknownHCLKind, kind)
			}
			nameObj := schema.Name{Name: name, Parent: parent, Schema: parentSchema}
			sk := kindToSchema(kind)
			var deps []schema.Dependency
			if resourceForm {
				nameObj.Schema = strings.TrimPrefix(rfSchema, "schema.")
				nameObj.Catalog = rfCatalog
				if rfParent != "" {
					nameObj.Parent = rfParent
				}
				if sk == schema.KindSchema {
					nameObj = schema.Name{Name: name}
				}
				if rfDeps != "" {
					if err := json.Unmarshal([]byte(rfDeps), &deps); err != nil {
						return fmt.Errorf("%w: resource deps_json: %v", ErrHCL, err)
					}
				}
			} else {
				if raw, ok := spec["schema"]; ok {
					if v, ok := raw.(string); ok {
						nameObj.Schema = strings.TrimPrefix(v, "schema.")
					}
				}
				delete(spec, "schema")
				if sk == schema.KindSchema {
					nameObj = schema.Name{Name: name}
				}
				deps = []schema.Dependency{}
				if parent != "" {
					deps = append(deps, schema.Dependency{Target: parent, Type: schema.DependencyContains})
				} else if nameObj.Schema != "" {
					sid := schema.StableID(schema.KindSchema, schema.Name{Name: nameObj.Schema})
					nameObj.Parent = sid
					deps = append(deps, schema.Dependency{Target: sid, Type: schema.DependencyContains})
				}
			}
			id := schema.StableID(sk, nameObj)
			bts, _ := json.Marshal(spec)
			rng := b.DefRange()
			loc := &schema.SourceLocation{URI: uri, Line: rng.Start.Line, Column: rng.Start.Column}
			var annotations map[string]string
			if rfAnnotations != "" {
				if err := json.Unmarshal([]byte(rfAnnotations), &annotations); err != nil {
					return fmt.Errorf("%w: resource annotations_json: %v", ErrHCL, err)
				}
			}
			doc.Graph.Resources = append(doc.Graph.Resources, schema.Resource{ID: id, Kind: sk, Name: nameObj, Dependencies: deps, Annotations: annotations, Source: loc, Spec: bts})
			childSchema := nameObj.Schema
			if sk == schema.KindSchema {
				childSchema = nameObj.Name
			}
			if err := walk(b.Body, id, childSchema); err != nil {
				return err
			}
			for _, n := range nested {
				_ = n
			}
		}
		return nil
	}
	if err := walk(body, "", ""); err != nil {
		return doc, imports, err
	}
	ensureSchemasFromHCL(&doc)
	return doc, imports, nil
}

func blockIdentity(b *hclsyntax.Block) (string, string, error) {
	if len(b.Labels) == 0 {
		return "", "", fmt.Errorf("%w: block %q needs a label", ErrHCL, b.Type)
	}
	return b.Type, b.Labels[len(b.Labels)-1], nil
}
func knownHCLKind(k string) bool {
	switch k {
	case "resource", "schema", "table", "column", "index", "view", "materialized_view", "sequence", "domain", "enum", "function", "procedure", "trigger", "policy", "role", "user", "permission", "grant", "membership", "default_privilege", "reference_data", "data":
		return true
	}
	return false
}
func kindToSchema(k string) schema.Kind {
	switch k {
	case "schema":
		return schema.KindSchema
	case "table":
		return schema.KindTable
	case "column":
		return schema.KindColumn
	case "index":
		return schema.KindIndex
	case "view":
		return schema.KindView
	case "materialized_view":
		return schema.KindMaterializedView
	case "sequence":
		return schema.KindSequence
	case "domain":
		return schema.KindDomain
	case "enum":
		return schema.KindEnum
	case "function":
		return schema.KindFunction
	case "procedure":
		return schema.KindProcedure
	case "trigger":
		return schema.KindTrigger
	case "policy":
		return schema.KindPolicy
	case "role":
		return schema.KindRole
	case "user":
		return schema.KindRole
	case "permission", "grant":
		return schema.KindGrant
	case "membership":
		return schema.KindMembership
	case "default_privilege":
		return schema.KindDefaultPrivilege
	case "reference_data", "data":
		return schema.KindReferenceData
	}
	return schema.Kind(k)
}
func attributesAndNested(body *hclsyntax.Body, data []byte, variables HCLVariables) (map[string]any, []*hclsyntax.Block, error) {
	out := map[string]any{}
	for n, a := range body.Attributes {
		v, err := expressionValue(a.Expr, data, variables)
		if err != nil {
			return nil, nil, fmt.Errorf("%w: attribute %s: %v", ErrHCL, n, err)
		}
		out[n] = v
	}
	return out, body.Blocks, nil
}
func stringAttr(body *hclsyntax.Body, name string, data []byte, variables HCLVariables) (string, error) {
	a, ok := body.Attributes[name]
	if !ok {
		return "", nil
	}
	v, e := expressionValue(a.Expr, data, variables)
	if e != nil {
		return "", e
	}
	s, ok := v.(string)
	if !ok {
		return "", fmt.Errorf("%w: %s must be string", ErrHCL, name)
	}
	return s, nil
}
func expressionValue(expr hcl.Expression, data []byte, variables HCLVariables) (any, error) {
	ctx := &hcl.EvalContext{Variables: map[string]cty.Value{}, Functions: hclFunctions()}
	varValues := map[string]cty.Value{}
	for k, v := range variables {
		cv, e := ctyValue(v)
		if e != nil {
			return nil, e
		}
		ctx.Variables[k] = cv
		varValues[k] = cv
	}
	if len(varValues) > 0 {
		ctx.Variables["var"] = cty.ObjectVal(varValues)
	}
	v, diags := expr.Value(ctx)
	if !diags.HasErrors() {
		return ctyToAny(v), nil
	}
	r := expr.Range()
	if r.Start.Byte >= 0 && r.End.Byte <= len(data) {
		raw := strings.TrimSpace(string(data[r.Start.Byte:r.End.Byte]))
		return raw, nil
	}
	return nil, fmt.Errorf("expression evaluation failed: %s", diags.Error())
}

func hclFunctions() map[string]function.Function {
	return map[string]function.Function{
		"jsonencode":  stdlib.JSONEncodeFunc,
		"schema_id":   stableIDFunction(schema.KindSchema, false),
		"table_id":    stableIDFunction(schema.KindTable, true),
		"column_id":   columnIDFunction(),
		"resource_id": resourceIDFunction(),
		"contains":    dependencyFunction(schema.DependencyContains),
		"references":  dependencyFunction(schema.DependencyReferences),
		"uses":        dependencyFunction(schema.DependencyUses),
		"owns":        dependencyFunction(schema.DependencyOwns),
	}
}

func stableIDFunction(kind schema.Kind, schemaScoped bool) function.Function {
	params := []function.Parameter{{Name: "name", Type: cty.String}}
	if schemaScoped {
		params = []function.Parameter{{Name: "schema", Type: cty.String}, {Name: "name", Type: cty.String}}
	}
	return function.New(&function.Spec{
		Params: params,
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			name := schema.Name{Name: args[len(args)-1].AsString()}
			if schemaScoped {
				name.Schema = args[0].AsString()
				name.Parent = schema.StableID(schema.KindSchema, schema.Name{Name: name.Schema})
			}
			return cty.StringVal(schema.StableID(kind, name)), nil
		},
	})
}

func columnIDFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "schema", Type: cty.String}, {Name: "table", Type: cty.String}, {Name: "column", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			schemaName, tableName, columnName := args[0].AsString(), args[1].AsString(), args[2].AsString()
			parent := schema.StableID(schema.KindTable, schema.Name{Schema: schemaName, Name: tableName, Parent: schema.StableID(schema.KindSchema, schema.Name{Name: schemaName})})
			return cty.StringVal(schema.StableID(schema.KindColumn, schema.Name{Schema: schemaName, Name: columnName, Parent: parent})), nil
		},
	})
}

func resourceIDFunction() function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "kind", Type: cty.String}, {Name: "schema", Type: cty.String}, {Name: "parent", Type: cty.String}, {Name: "name", Type: cty.String}},
		Type:   function.StaticReturnType(cty.String),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			kind := schema.Kind(args[0].AsString())
			if !schema.IsKnownKind(kind) {
				return cty.NilVal, fmt.Errorf("unsupported resource kind %q", kind)
			}
			name := schema.Name{Schema: args[1].AsString(), Parent: args[2].AsString(), Name: args[3].AsString()}
			if kind == schema.KindSchema {
				name = schema.Name{Name: name.Name}
			}
			return cty.StringVal(schema.StableID(kind, name)), nil
		},
	})
}

func dependencyFunction(kind schema.DependencyType) function.Function {
	return function.New(&function.Spec{
		Params: []function.Parameter{{Name: "target", Type: cty.String}},
		Type: function.StaticReturnType(cty.Object(map[string]cty.Type{
			"target": cty.String,
			"type":   cty.String,
		})),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			return cty.ObjectVal(map[string]cty.Value{
				"target": args[0],
				"type":   cty.StringVal(string(kind)),
			}), nil
		},
	})
}
func ctyValue(v any) (cty.Value, error) {
	switch x := v.(type) {
	case string:
		return cty.StringVal(x), nil
	case bool:
		return cty.BoolVal(x), nil
	case int:
		return cty.NumberIntVal(int64(x)), nil
	case int64:
		return cty.NumberIntVal(x), nil
	case float64:
		return cty.NumberFloatVal(x), nil
	case []string:
		a := make([]cty.Value, len(x))
		for i := range x {
			a[i] = cty.StringVal(x[i])
		}
		return cty.TupleVal(a), nil
	case map[string]any:
		m := map[string]cty.Value{}
		for k, v := range x {
			cv, e := ctyValue(v)
			if e != nil {
				return cty.NilVal, e
			}
			m[k] = cv
		}
		return cty.ObjectVal(m), nil
	default:
		return cty.NilVal, fmt.Errorf("unsupported HCL variable type %T", v)
	}
}
func ctyToAny(v cty.Value) any {
	if !v.IsKnown() || v.IsNull() {
		return nil
	}
	if v.Type() == cty.String {
		return v.AsString()
	}
	if v.Type() == cty.Bool {
		return v.True()
	}
	if v.Type() == cty.Number {
		f, _ := v.AsBigFloat().Float64()
		return f
	}
	if v.CanIterateElements() {
		it := v.ElementIterator()
		var out []any
		for it.Next() {
			_, x := it.Element()
			out = append(out, ctyToAny(x))
		}
		return out
	}
	return nil
}
func ensureSchemasFromHCL(doc *schema.Document) {
	has := map[string]bool{}
	for _, r := range doc.Graph.Resources {
		if r.Kind == schema.KindSchema {
			has[r.Name.Name] = true
		}
	}
	for _, r := range doc.Graph.Resources {
		if r.Name.Schema != "" && !has[r.Name.Schema] {
			name := schema.Name{Name: r.Name.Schema}
			doc.Graph.Resources = append(doc.Graph.Resources, schema.Resource{ID: schema.StableID(schema.KindSchema, name), Kind: schema.KindSchema, Name: name, Source: r.Source})
			has[r.Name.Schema] = true
		}
	}
}

// withoutKey returns a copy of m with key removed, or nil if that leaves it
// empty. Used to drop parser-internal annotations before emitting HCL.
func withoutKey(m map[string]string, key string) map[string]string {
	if len(m) == 0 {
		return nil
	}
	out := make(map[string]string, len(m))
	for k, v := range m {
		if k != key {
			out[k] = v
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// FormatHCL produces deterministic resource-form HCL. Resource form is a
// lossless escape hatch for every canonical kind and can be converted to more
// ergonomic blocks by a formatter later.
func FormatHCL(doc schema.Document) ([]byte, error) {
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	f := hclwrite.NewEmptyFile()
	root := f.Body()
	// Document-level metadata (e.g. the dialect annotation stamped by schema
	// inspect) must round-trip, or plan.Build rejects the transition even when
	// every resource matches. hcl_imports is a parser-internal marker, not
	// source state, so it is not emitted.
	if docAnnotations := withoutKey(doc.Annotations, "hcl_imports"); len(docAnnotations) > 0 {
		aj, err := json.Marshal(docAnnotations)
		if err != nil {
			return nil, err
		}
		db := root.AppendNewBlock("document", nil)
		db.Body().SetAttributeValue("annotations_json", cty.StringVal(string(aj)))
		root.AppendNewline()
	}
	rs := append([]schema.Resource(nil), doc.Graph.Resources...)
	sort.Slice(rs, func(i, j int) bool { return rs[i].ID < rs[j].ID })
	for _, r := range rs {
		b := root.AppendNewBlock("resource", []string{string(r.Kind), r.Name.Name})
		body := b.Body()
		if r.Name.Schema != "" {
			body.SetAttributeValue("schema", cty.StringVal(r.Name.Schema))
		}
		if r.Name.Catalog != "" {
			body.SetAttributeValue("catalog", cty.StringVal(r.Name.Catalog))
		}
		if r.Name.Parent != "" {
			body.SetAttributeValue("parent", cty.StringVal(r.Name.Parent))
		}
		var spec any
		if len(r.Spec) > 0 {
			decoder := json.NewDecoder(strings.NewReader(string(r.Spec)))
			decoder.UseNumber()
			if err := decoder.Decode(&spec); err != nil {
				return nil, err
			}
			raw, _ := json.Marshal(spec)
			body.SetAttributeValue("spec_json", cty.StringVal(string(raw)))
		}
		if len(r.Dependencies) > 0 {
			dj, err := json.Marshal(r.Dependencies)
			if err != nil {
				return nil, err
			}
			body.SetAttributeValue("deps_json", cty.StringVal(string(dj)))
		}
		if len(r.Annotations) > 0 {
			aj, err := json.Marshal(r.Annotations)
			if err != nil {
				return nil, err
			}
			body.SetAttributeValue("annotations_json", cty.StringVal(string(aj)))
		}
		root.AppendNewline()
	}
	return f.Bytes(), nil
}
