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
		doc.Annotations = map[string]string{"hcl_imports": strings.Join(imports, ",")}
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
	var walk func(*hclsyntax.Body, string) error
	walk = func(current *hclsyntax.Body, parent string) error {
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
			kind, name, err := blockIdentity(b)
			if err != nil {
				return err
			}
			spec, nested, err := attributesAndNested(b.Body, data, variables)
			if err != nil {
				return err
			}
			if kind == "resource" {
				if len(b.Labels) != 2 {
					return fmt.Errorf("%w: resource needs kind and name", ErrHCL)
				}
				kind = b.Labels[0]
				name = b.Labels[1]
				if encoded, ok := spec["spec_json"].(string); ok {
					var decoded map[string]any
					if err := json.Unmarshal([]byte(encoded), &decoded); err != nil {
						return fmt.Errorf("%w: resource spec_json: %v", ErrHCL, err)
					}
					spec = decoded
				}
			}
			if !knownHCLKind(kind) {
				return fmt.Errorf("%w: %s", ErrUnknownHCLKind, kind)
			}
			nameObj := schema.Name{Name: name, Parent: parent}
			if raw, ok := spec["schema"]; ok {
				if v, ok := raw.(string); ok {
					nameObj.Schema = strings.TrimPrefix(v, "schema.")
				}
			}
			delete(spec, "schema")
			sk := kindToSchema(kind)
			if sk == schema.KindSchema {
				nameObj = schema.Name{Name: name}
			}
			deps := []schema.Dependency{}
			if parent != "" {
				deps = append(deps, schema.Dependency{Target: parent, Type: schema.DependencyContains})
			}
			if nameObj.Schema != "" {
				sid := schema.StableID(schema.KindSchema, schema.Name{Name: nameObj.Schema})
				deps = append(deps, schema.Dependency{Target: sid, Type: schema.DependencyContains})
			}
			id := schema.StableID(sk, nameObj)
			bts, _ := json.Marshal(spec)
			rng := b.DefRange()
			loc := &schema.SourceLocation{URI: uri, Line: rng.Start.Line, Column: rng.Start.Column}
			doc.Graph.Resources = append(doc.Graph.Resources, schema.Resource{ID: id, Kind: sk, Name: nameObj, Dependencies: deps, Source: loc, Spec: bts})
			if err := walk(b.Body, id); err != nil {
				return err
			}
			for _, n := range nested {
				_ = n
			}
		}
		return nil
	}
	if err := walk(body, ""); err != nil {
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
	ctx := &hcl.EvalContext{Variables: map[string]cty.Value{}}
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

// FormatHCL produces deterministic resource-form HCL. Resource form is a
// lossless escape hatch for every canonical kind and can be converted to more
// ergonomic blocks by a formatter later.
func FormatHCL(doc schema.Document) ([]byte, error) {
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	f := hclwrite.NewEmptyFile()
	root := f.Body()
	rs := append([]schema.Resource(nil), doc.Graph.Resources...)
	sort.Slice(rs, func(i, j int) bool { return rs[i].ID < rs[j].ID })
	for _, r := range rs {
		b := root.AppendNewBlock("resource", []string{string(r.Kind), r.Name.Name})
		body := b.Body()
		if r.Name.Schema != "" {
			body.SetAttributeValue("schema", cty.StringVal(r.Name.Schema))
		}
		var spec any
		if len(r.Spec) > 0 {
			if err := json.Unmarshal(r.Spec, &spec); err != nil {
				return nil, err
			}
			raw, _ := json.Marshal(spec)
			body.SetAttributeValue("spec_json", cty.StringVal(string(raw)))
		}
		root.AppendNewline()
	}
	return f.Bytes(), nil
}
