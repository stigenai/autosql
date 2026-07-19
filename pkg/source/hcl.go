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
	body, effective, err := prepareHCLBody(file.Body.(*hclsyntax.Body), data, variables)
	if err != nil {
		return schema.Document{}, err
	}
	doc, imports, err := hclDocument(ctx, uri, body, data, effective)
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
	if err := validateHCLDocument(doc); err != nil {
		return schema.Document{}, err
	}
	return doc, nil
}

// HCLLoader resolves import/module blocks with cycle and depth protection.
type HCLLoader struct {
	ReadFile  func(string) ([]byte, error)
	ReadDir   func(string) ([]os.DirEntry, error)
	MaxDepth  int
	Variables HCLVariables
}

func (l HCLLoader) Load(ctx context.Context, root string) (schema.Document, error) {
	read := l.ReadFile
	if read == nil {
		read = os.ReadFile
	}
	readDir := l.ReadDir
	if readDir == nil {
		readDir = os.ReadDir
	}
	max := l.MaxDepth
	if max <= 0 {
		max = 32
	}
	var units []hclSourceUnit
	if _, err := l.collect(ctx, root, read, readDir, max, map[string]bool{}, &units, cloneHCLVariables(l.Variables), false); err != nil {
		return schema.Document{}, err
	}
	sources := make([]hclSymbolSource, len(units))
	for index, unit := range units {
		sources[index] = hclSymbolSource{body: unit.body, data: unit.data, variables: unit.variables}
	}
	symbols, err := buildHCLSymbolTableSources(sources)
	if err != nil {
		return schema.Document{}, err
	}
	doc := schema.Document{Version: schema.SchemaVersion}
	for _, unit := range units {
		part, _, err := hclDocumentWithSymbols(ctx, unit.uri, unit.body, unit.data, unit.variables, symbols)
		if err != nil {
			return schema.Document{}, err
		}
		if err := mergeHCLDocumentMetadata(&doc, part); err != nil {
			return schema.Document{}, err
		}
		doc.Graph.Resources = append(doc.Graph.Resources, part.Graph.Resources...)
	}
	ensureSchemasFromHCL(&doc)
	doc.Normalize()
	if err := validateHCLDocument(doc); err != nil {
		return schema.Document{}, err
	}
	return doc, nil
}

func validateHCLDocument(doc schema.Document) error {
	seen := map[string]*schema.SourceLocation{}
	for _, resource := range doc.Graph.Resources {
		if first, exists := seen[resource.ID]; exists {
			firstAt, secondAt := "unknown source", "unknown source"
			if first != nil {
				firstAt = fmt.Sprintf("%s:%d:%d", first.URI, first.Line, first.Column)
			}
			if resource.Source != nil {
				secondAt = fmt.Sprintf("%s:%d:%d", resource.Source.URI, resource.Source.Line, resource.Source.Column)
			}
			return fmt.Errorf("%w: duplicate resource identity %s declared at %s and %s", ErrHCL, resource.ID, firstAt, secondAt)
		}
		seen[resource.ID] = resource.Source
	}
	err := doc.Validate()
	if err == nil {
		return nil
	}
	for _, resource := range doc.Graph.Resources {
		if resource.Source != nil && strings.Contains(err.Error(), fmt.Sprintf("%q", resource.ID)) {
			return fmt.Errorf("%s:%d:%d: %w", resource.Source.URI, resource.Source.Line, resource.Source.Column, err)
		}
	}
	return err
}

func mergeHCLDocumentMetadata(target *schema.Document, part schema.Document) error {
	if target.Annotations == nil && len(part.Annotations) > 0 {
		target.Annotations = map[string]string{}
	}
	for key, value := range part.Annotations {
		if key == schema.RenameHintsAnnotation {
			left, err := schema.DocumentRenameHints(*target)
			if err != nil {
				return err
			}
			right, err := schema.DocumentRenameHints(part)
			if err != nil {
				return err
			}
			all := append(left, right...)
			if err := validateHCLRenameHints(all); err != nil {
				return err
			}
			sort.Slice(all, func(i, j int) bool {
				return all[i].From < all[j].From || all[i].From == all[j].From && all[i].To < all[j].To
			})
			raw, _ := json.Marshal(all)
			target.Annotations[key] = string(raw)
			continue
		}
		if existing, ok := target.Annotations[key]; ok && existing != value {
			return fmt.Errorf("%w: conflicting document annotation %s across composed sources", ErrHCL, key)
		}
		target.Annotations[key] = value
	}
	for _, pair := range []struct {
		name   string
		target *map[string]json.RawMessage
		source map[string]json.RawMessage
	}{
		{name: "document extra", target: &target.Extra, source: part.Extra},
		{name: "graph extra", target: &target.Graph.Extra, source: part.Graph.Extra},
	} {
		if *pair.target == nil && len(pair.source) > 0 {
			*pair.target = map[string]json.RawMessage{}
		}
		for key, value := range pair.source {
			if existing, ok := (*pair.target)[key]; ok && string(existing) != string(value) {
				return fmt.Errorf("%w: conflicting %s %s across composed sources", ErrHCL, pair.name, key)
			}
			(*pair.target)[key] = append(json.RawMessage(nil), value...)
		}
	}
	return nil
}

type hclSourceUnit struct {
	uri       string
	data      []byte
	body      *hclsyntax.Body
	variables HCLVariables
}

func (l HCLLoader) collect(ctx context.Context, uri string, read func(string) ([]byte, error), readDir func(string) ([]os.DirEntry, error), remaining int, stack map[string]bool, units *[]hclSourceUnit, scope HCLVariables, strictInputs bool) (map[string]any, error) {
	if remaining <= 0 {
		return nil, ErrImportLimit
	}
	canonical, err := filepath.Abs(uri)
	if err == nil {
		uri = canonical
	}
	if stack[uri] {
		return nil, fmt.Errorf("%w: %s", ErrImportCycle, uri)
	}
	stack[uri] = true
	defer delete(stack, uri)
	if entries, dirErr := readDir(uri); dirErr == nil {
		var files []string
		for _, entry := range entries {
			if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".hcl") {
				files = append(files, filepath.Join(uri, entry.Name()))
			}
		}
		sort.Strings(files)
		if len(files) == 0 {
			return nil, fmt.Errorf("%w: module directory %s has no .hcl files", ErrHCL, uri)
		}
		if strictInputs {
			declared := map[string]bool{}
			for _, fileName := range files {
				fileData, readErr := read(fileName)
				if readErr != nil {
					return nil, readErr
				}
				parsed, diagnostics := hclsyntaxParse(fileData, fileName)
				if diagnostics.HasErrors() {
					return nil, fmt.Errorf("%w: %s", ErrHCL, diagnostics.Error())
				}
				for _, name := range declaredHCLVariables(parsed.Body.(*hclsyntax.Body)) {
					declared[name] = true
				}
			}
			if err := rejectUnknownHCLInputs(scope, declared); err != nil {
				return nil, err
			}
		}
		outputs := map[string]any{}
		for _, fileName := range files {
			part, collectErr := l.collect(ctx, fileName, read, readDir, remaining-1, stack, units, scope, false)
			if collectErr != nil {
				return nil, collectErr
			}
			for key, value := range part {
				if _, exists := outputs[key]; exists {
					return nil, fmt.Errorf("%w: duplicate module output %s", ErrHCL, key)
				}
				outputs[key] = value
			}
		}
		return outputs, nil
	}
	data, err := read(uri)
	if err != nil {
		return nil, err
	}
	file, diags := hclsyntaxParse(data, uri)
	if diags.HasErrors() {
		return nil, fmt.Errorf("%w: %s", ErrHCL, diags.Error())
	}
	body := file.Body.(*hclsyntax.Body)
	if strictInputs {
		declared := map[string]bool{}
		for _, name := range declaredHCLVariables(body) {
			declared[name] = true
		}
		if err := rejectUnknownHCLInputs(scope, declared); err != nil {
			return nil, err
		}
	}
	prepared, effective, err := prepareHCLBody(body, data, scope)
	if err != nil {
		return nil, err
	}
	unit := &hclSourceUnit{uri: uri, data: data, body: prepared, variables: effective}
	*units = append(*units, *unit)
	unitIndex := len(*units) - 1
	imports, err := hclImports(prepared, data, effective)
	if err != nil {
		return nil, err
	}
	modules := map[string]any{}
	for _, item := range imports {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		child := item.source
		if !filepath.IsAbs(child) {
			child = filepath.Join(filepath.Dir(uri), child)
		}
		childScope := effective
		childStrict := false
		if item.module != "" {
			childScope = item.inputs
			childStrict = true
		}
		childOutputs, collectErr := l.collect(ctx, child, read, readDir, remaining-1, stack, units, cloneHCLVariables(childScope), childStrict)
		if collectErr != nil {
			return nil, collectErr
		}
		if item.module != "" {
			if _, exists := modules[item.module]; exists {
				return nil, fmt.Errorf("%w: duplicate module label %s", ErrHCL, item.module)
			}
			modules[item.module] = childOutputs
		}
	}
	if len(modules) > 0 {
		effective["module"] = modules
		(*units)[unitIndex].variables = effective
	}
	outputs, err := evaluateHCLOutputs(prepared, data, effective)
	if err != nil {
		return nil, err
	}
	return outputs, nil
}

type hclImport struct {
	source string
	module string
	inputs HCLVariables
}

func hclImports(body *hclsyntax.Body, data []byte, variables HCLVariables) ([]hclImport, error) {
	var imports []hclImport
	for _, block := range body.Blocks {
		if block.Type != "import" && block.Type != "module" {
			continue
		}
		source, err := stringAttr(block.Body, "source", data, variables)
		if err != nil {
			return nil, err
		}
		if source != "" {
			item := hclImport{source: source}
			if block.Type == "module" {
				if len(block.Labels) != 1 {
					return nil, fmt.Errorf("%w: module requires one label", ErrHCL)
				}
				item.module = block.Labels[0]
				item.inputs = HCLVariables{}
				if attribute := block.Body.Attributes["inputs"]; attribute != nil {
					value, inputErr := expressionValueWithSymbols(attribute.Expr, data, variables, nil)
					object, ok := value.(map[string]any)
					if inputErr != nil || !ok {
						return nil, fmt.Errorf("%w: module %s inputs must be an object", ErrHCL, item.module)
					}
					for key, input := range object {
						item.inputs[key] = input
					}
				}
			}
			imports = append(imports, item)
		}
	}
	return imports, nil
}

func hclsyntaxParse(data []byte, uri string) (*hcl.File, hcl.Diagnostics) {
	return hclsyntax.ParseConfig(data, uri, hcl.Pos{Line: 1, Column: 1})
}

func hclDocument(ctx context.Context, uri string, body *hclsyntax.Body, data []byte, variables HCLVariables) (schema.Document, []string, error) {
	symbols, err := buildHCLSymbolTable(body, data, variables)
	if err != nil {
		return schema.Document{Version: schema.SchemaVersion}, nil, err
	}
	doc, imports, err := hclDocumentWithSymbols(ctx, uri, body, data, variables, symbols)
	if err == nil {
		ensureSchemasFromHCL(&doc)
	}
	return doc, imports, err
}

func hclDocumentWithSymbols(ctx context.Context, uri string, body *hclsyntax.Body, data []byte, variables HCLVariables, symbols *hclSymbolTable) (schema.Document, []string, error) {
	doc := schema.Document{Version: schema.SchemaVersion}
	var imports []string
	var renameHints []schema.RenameHint
	var walk func(*hclsyntax.Body, string, string, schema.Kind) error
	walk = func(current *hclsyntax.Body, parent, parentSchema string, parentKind schema.Kind) error {
		blocks := append([]*hclsyntax.Block(nil), current.Blocks...)
		sort.SliceStable(blocks, func(i, j int) bool { return blocks[i].DefRange().Start.Byte < blocks[j].DefRange().Start.Byte })
		for _, b := range blocks {
			if err := ctx.Err(); err != nil {
				return err
			}
			if inlineHCLBlock(parentKind, b.Type) {
				continue
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
			if b.Type == "variable" || b.Type == "output" {
				continue
			}
			if b.Type == "moved" {
				from, e := hclRenameAttribute(b.Body, "from", data, variables, symbols)
				if e != nil {
					return e
				}
				to, e := hclRenameAttribute(b.Body, "to", data, variables, symbols)
				if e != nil {
					return e
				}
				renameHints = append(renameHints, schema.RenameHint{From: from, To: to})
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
				for attribute, target := range map[string]*map[string]json.RawMessage{"extra_json": &doc.Extra, "graph_extra_json": &doc.Graph.Extra} {
					encoded, extraErr := stringAttr(b.Body, attribute, data, variables)
					if extraErr != nil {
						return extraErr
					}
					if encoded != "" && json.Unmarshal([]byte(encoded), target) != nil {
						return fmt.Errorf("%w: document %s is invalid", ErrHCL, attribute)
					}
				}
				continue
			}
			kind, name, err := hclBlockIdentity(b, symbols.reference(parent).Name)
			if err != nil {
				return err
			}
			evaluationSymbols := hclBlockEvaluationSymbols(b, data, variables, symbols.variables(parent))
			var renamedFrom string
			if b.Body.Attributes["renamed_from"] != nil {
				renamedFrom, err = hclRenameAttribute(b.Body, "renamed_from", data, variables, symbols)
				if err != nil {
					return err
				}
			}
			spec, nested, err := attributesAndNested(b.Body, data, variables, evaluationSymbols)
			if err != nil {
				return err
			}
			resourceForm := kind == "resource"
			// Resource form is a lossless escape hatch for every canonical kind
			// (this is what FormatHCL emits). Its identity attrs — schema,
			// parent, catalog — and its dependencies are captured verbatim so
			// FormatHCL -> ParseHCL reproduces an identical graph. They must be
			// read before spec_json replaces the block attribute map.
			var rfSchema, rfParent, rfCatalog, rfDeps, rfAnnotations, rfSource, rfExtra, rfNameExtra string
			rfSource, _ = spec["source_json"].(string)
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
				rfExtra, _ = spec["extra_json"].(string)
				rfNameExtra, _ = spec["name_extra_json"].(string)
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
					delete(spec, "source_json")
					delete(spec, "extra_json")
					delete(spec, "name_extra_json")
				}
				if !schema.IsKnownKind(schema.Kind(kind)) {
					return fmt.Errorf("%w: %s", ErrUnknownHCLKind, kind)
				}
			} else {
				delete(spec, "source_json")
				if !knownHCLKind(kind) {
					return fmt.Errorf("%w: %s", ErrUnknownHCLKind, kind)
				}
			}
			nameObj := schema.Name{Name: name, Parent: parent, Schema: parentSchema}
			sk := kindToSchema(kind)
			var deps []schema.Dependency
			var authorAnnotations map[string]string
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
				if comment, ok := spec["comment"].(string); ok {
					authorAnnotations = map[string]string{"comment": comment}
					delete(spec, "comment")
				}
				if authored, ok := spec["dependencies"]; ok {
					encoded, marshalErr := json.Marshal(authored)
					if marshalErr != nil || json.Unmarshal(encoded, &deps) != nil {
						return fmt.Errorf("%w: dependencies must be a list produced by contains/references/uses/owns", ErrHCL)
					}
					delete(spec, "dependencies")
				}
				if raw, ok := spec["schema"]; ok {
					if ref, isReference := referenceFromAny(raw); isReference {
						if ref.Kind != schema.KindSchema {
							return fmt.Errorf("%w: schema attribute requires a schema reference, got %s", ErrHCL, ref.Kind)
						}
						nameObj.Schema = ref.Name
					} else if v, ok := raw.(string); ok {
						nameObj.Schema = strings.TrimPrefix(v, "schema.")
					} else {
						return fmt.Errorf("%w: schema must be a string or schema reference", ErrHCL)
					}
				}
				delete(spec, "schema")
				if sk == schema.KindGrant {
					rawTarget := spec["target"]
					if rawTarget == nil {
						rawTarget = spec["object"]
					}
					if target, ok := referenceFromAny(rawTarget); ok {
						nameObj.Parent = target.ID
						nameObj.Schema = target.Schema
					}
				}
				if lowerErr := lowerNativeHCLResource(sk, nameObj, b, spec, &deps, symbols, data, variables); lowerErr != nil {
					rangeValue := b.DefRange()
					return fmt.Errorf("%s:%d:%d: %w", uri, rangeValue.Start.Line, rangeValue.Start.Column, lowerErr)
				}
				for attribute, value := range spec {
					resolved, resolveErr := resolveHCLReferences(value, attribute, &deps)
					if resolveErr != nil {
						if sourceAttribute, ok := b.Body.Attributes[attribute]; ok {
							rangeValue := sourceAttribute.Range()
							return fmt.Errorf("%s:%d:%d: %w", uri, rangeValue.Start.Line, rangeValue.Start.Column, resolveErr)
						}
						return resolveErr
					}
					spec[attribute] = resolved
				}
				if sk == schema.KindSchema {
					nameObj = schema.Name{Name: name}
				}
				if deps == nil {
					deps = []schema.Dependency{}
				}
				if parent != "" {
					deps = append(deps, schema.Dependency{Target: parent, Type: schema.DependencyContains})
				} else if nameObj.Parent == "" && nameObj.Schema != "" {
					sid := schema.StableID(schema.KindSchema, schema.Name{Name: nameObj.Schema})
					nameObj.Parent = sid
					deps = append(deps, schema.Dependency{Target: sid, Type: schema.DependencyContains})
				}
			}
			id := schema.StableID(sk, nameObj)
			if renamedFrom != "" {
				renameHints = append(renameHints, schema.RenameHint{From: renamedFrom, To: id})
			}
			bts, _ := json.Marshal(spec)
			rng := b.DefRange()
			var loc *schema.SourceLocation
			if rfSource != "" {
				if strings.TrimSpace(rfSource) != "null" {
					loc = &schema.SourceLocation{}
				}
				if err := json.Unmarshal([]byte(rfSource), &loc); err != nil {
					return fmt.Errorf("%w: resource source_json: %v", ErrHCL, err)
				}
			} else {
				loc = &schema.SourceLocation{URI: uri, Line: rng.Start.Line, Column: rng.Start.Column}
			}
			if each, ok := evaluationSymbols["each"].(map[string]any); ok && loc != nil {
				if key, ok := each["key"].(string); ok {
					if loc.Extra == nil {
						loc.Extra = map[string]json.RawMessage{}
					}
					loc.Extra["for_each_key"], _ = json.Marshal(key)
				}
			}
			var annotations map[string]string
			if rfAnnotations != "" {
				if err := json.Unmarshal([]byte(rfAnnotations), &annotations); err != nil {
					return fmt.Errorf("%w: resource annotations_json: %v", ErrHCL, err)
				}
			}
			for key, value := range authorAnnotations {
				if annotations == nil {
					annotations = map[string]string{}
				}
				annotations[key] = value
			}
			var resourceExtra, nameExtra map[string]json.RawMessage
			if rfExtra != "" && json.Unmarshal([]byte(rfExtra), &resourceExtra) != nil {
				return fmt.Errorf("%w: resource extra_json is invalid", ErrHCL)
			}
			if rfNameExtra != "" && json.Unmarshal([]byte(rfNameExtra), &nameExtra) != nil {
				return fmt.Errorf("%w: resource name_extra_json is invalid", ErrHCL)
			}
			nameObj.Extra = nameExtra
			doc.Graph.Resources = append(doc.Graph.Resources, schema.Resource{ID: id, Kind: sk, Name: nameObj, Dependencies: deps, Annotations: annotations, Source: loc, Spec: bts, Extra: resourceExtra})
			childSchema := nameObj.Schema
			if sk == schema.KindSchema {
				childSchema = nameObj.Name
			}
			if err := walk(b.Body, id, childSchema, sk); err != nil {
				return err
			}
			for _, n := range nested {
				_ = n
			}
		}
		return nil
	}
	if err := walk(body, "", "", ""); err != nil {
		return doc, imports, err
	}
	if len(renameHints) > 0 {
		if err := validateHCLRenameHints(renameHints); err != nil {
			return doc, imports, err
		}
		sort.Slice(renameHints, func(i, j int) bool {
			return renameHints[i].From < renameHints[j].From || renameHints[i].From == renameHints[j].From && renameHints[i].To < renameHints[j].To
		})
		raw, _ := json.Marshal(renameHints)
		if doc.Annotations == nil {
			doc.Annotations = map[string]string{}
		}
		doc.Annotations[schema.RenameHintsAnnotation] = string(raw)
	}
	return doc, imports, nil
}

func hclRenameAttribute(body *hclsyntax.Body, name string, data []byte, variables HCLVariables, symbols *hclSymbolTable) (string, error) {
	attribute, ok := body.Attributes[name]
	if !ok {
		return "", fmt.Errorf("%w: moved block requires %s", ErrHCL, name)
	}
	value, err := expressionValueWithSymbols(attribute.Expr, data, variables, symbols.variables(""))
	if err == nil {
		return hclRenameTarget(value)
	}
	traversal, diagnostics := hcl.AbsTraversalForExpr(attribute.Expr)
	if diagnostics.HasErrors() {
		return "", fmt.Errorf("%w: moved.%s must be a resource reference or stable ID", ErrHCL, name)
	}
	if id := stableIDFromHCLTraversal(traversal); id != "" {
		return id, nil
	}
	return "", fmt.Errorf("%w: moved.%s is not a supported resource reference", ErrHCL, name)
}

func hclRenameTarget(value any) (string, error) {
	if reference, ok := referenceFromAny(value); ok {
		return reference.ID, nil
	}
	if text, ok := value.(string); ok && strings.TrimSpace(text) != "" {
		return strings.TrimSpace(text), nil
	}
	return "", fmt.Errorf("must be a resource reference or stable ID")
}

func validateHCLRenameHints(hints []schema.RenameHint) error {
	from, to := map[string]string{}, map[string]string{}
	for _, hint := range hints {
		if hint.From == "" || hint.To == "" || hint.From == hint.To {
			return fmt.Errorf("%w: invalid rename %q -> %q", ErrHCL, hint.From, hint.To)
		}
		if prior, ok := from[hint.From]; ok {
			return fmt.Errorf("%w: conflicting or duplicate rename source %s (%s, %s)", ErrHCL, hint.From, prior, hint.To)
		}
		if prior, ok := to[hint.To]; ok {
			return fmt.Errorf("%w: conflicting rename target %s (%s, %s)", ErrHCL, hint.To, prior, hint.From)
		}
		from[hint.From], to[hint.To] = hint.To, hint.From
	}
	return nil
}

func blockIdentity(b *hclsyntax.Block) (string, string, error) {
	if len(b.Labels) == 0 {
		return "", "", fmt.Errorf("%w: block %q needs a label", ErrHCL, b.Type)
	}
	return b.Type, b.Labels[len(b.Labels)-1], nil
}
func knownHCLKind(k string) bool {
	switch k {
	case "resource", "database", "schema", "extension", "table", "column", "primary_key", "unique", "unique_constraint", "check", "check_constraint", "foreign_key", "index", "view", "materialized", "materialized_view", "sequence", "domain", "enum", "composite", "composite_type", "function", "procedure", "trigger", "policy", "role", "user", "permission", "grant", "membership", "default_privilege", "reference_data", "data":
		return true
	}
	return false
}
func kindToSchema(k string) schema.Kind {
	switch k {
	case "database":
		return schema.KindDatabase
	case "schema":
		return schema.KindSchema
	case "extension":
		return schema.KindExtension
	case "table":
		return schema.KindTable
	case "column":
		return schema.KindColumn
	case "primary_key":
		return schema.KindPrimaryKey
	case "unique", "unique_constraint":
		return schema.KindUniqueConstraint
	case "check", "check_constraint":
		return schema.KindCheckConstraint
	case "foreign_key":
		return schema.KindForeignKey
	case "index":
		return schema.KindIndex
	case "view":
		return schema.KindView
	case "materialized", "materialized_view":
		return schema.KindMaterializedView
	case "sequence":
		return schema.KindSequence
	case "domain":
		return schema.KindDomain
	case "enum":
		return schema.KindEnum
	case "composite", "composite_type":
		return schema.KindComposite
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
func attributesAndNested(body *hclsyntax.Body, data []byte, variables HCLVariables, symbols map[string]any) (map[string]any, []*hclsyntax.Block, error) {
	out := map[string]any{}
	for n, a := range body.Attributes {
		if n == hclEachKey || n == hclEachValue || n == "renamed_from" {
			continue
		}
		v, err := expressionValueWithSymbols(a.Expr, data, variables, symbols)
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
	return expressionValueWithSymbols(expr, data, variables, nil)
}

func expressionValueWithSymbols(expr hcl.Expression, data []byte, variables HCLVariables, symbols map[string]any) (any, error) {
	ctx := &hcl.EvalContext{Variables: map[string]cty.Value{}, Functions: hclFunctions()}
	// Symbol roots contain the complete resource graph. Converting every root
	// for every literal attribute makes canonical documents quadratic in size.
	referencedRoots := map[string]bool{}
	for _, traversal := range expr.Variables() {
		referencedRoots[traversal.RootName()] = true
	}
	if referencedRoots["var"] {
		varValues := map[string]cty.Value{}
		for name, value := range variables {
			if name == hclLocalsVariable {
				continue
			}
			converted, err := ctyValue(value)
			if err != nil {
				return nil, err
			}
			varValues[name] = converted
		}
		if len(varValues) > 0 {
			ctx.Variables["var"] = cty.ObjectVal(varValues)
		}
	}
	for name, value := range variables {
		if name == hclLocalsVariable || !referencedRoots[name] {
			continue
		}
		converted, err := ctyValue(value)
		if err != nil {
			return nil, err
		}
		ctx.Variables[name] = converted
	}
	if locals, ok := variables[hclLocalsVariable].(map[string]any); referencedRoots["local"] && ok && len(locals) > 0 {
		converted, err := ctyValue(locals)
		if err != nil {
			return nil, err
		}
		ctx.Variables["local"] = converted
	}
	for name, value := range symbols {
		if !referencedRoots[name] {
			continue
		}
		converted, err := ctyValue(value)
		if err != nil {
			return nil, err
		}
		ctx.Variables[name] = converted
	}
	v, diags := expr.Value(ctx)
	if !diags.HasErrors() {
		return ctyToAny(v), nil
	}
	if containsHCLReferenceTraversal(expr, symbols) {
		return nil, fmt.Errorf("symbolic reference evaluation failed: %s", diags.Error())
	}
	r := expr.Range()
	if r.Start.Byte >= 0 && r.End.Byte <= len(data) {
		raw := strings.TrimSpace(string(data[r.Start.Byte:r.End.Byte]))
		return raw, nil
	}
	return nil, fmt.Errorf("expression evaluation failed: %s", diags.Error())
}

func hclFunctions() map[string]function.Function {
	functions := map[string]function.Function{
		"jsonencode":                   stdlib.JSONEncodeFunc,
		"chomp":                        stdlib.ChompFunc,
		"schema_id":                    stableIDFunction(schema.KindSchema, false),
		"table_id":                     stableIDFunction(schema.KindTable, true),
		"extension_id":                 stableIDFunction(schema.KindExtension, true),
		"composite_id":                 stableIDFunction(schema.KindComposite, true),
		"column_id":                    columnIDFunction(),
		"resource_id":                  resourceIDFunction(),
		"contains":                     dependencyFunction(schema.DependencyContains),
		"references":                   dependencyFunction(schema.DependencyReferences),
		"uses":                         dependencyFunction(schema.DependencyUses),
		"owns":                         dependencyFunction(schema.DependencyOwns),
		"composite_attribute":          compositeAttributeFunction(false),
		"collated_composite_attribute": compositeAttributeFunction(true),
	}
	for name, expressionFunction := range hclExpressionFunctions() {
		functions[name] = expressionFunction
	}
	return functions
}

func compositeAttributeFunction(collated bool) function.Function {
	params := []function.Parameter{{Name: "name", Type: cty.String}, {Name: "type", Type: cty.String}, {Name: "ordinal", Type: cty.Number}}
	attributeType := map[string]cty.Type{"name": cty.String, "type": cty.String, "ordinal": cty.Number}
	if collated {
		params = append(params, function.Parameter{Name: "collation", Type: cty.String})
		attributeType["collation"] = cty.String
	}
	return function.New(&function.Spec{
		Params: params,
		Type:   function.StaticReturnType(cty.Object(attributeType)),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			value := map[string]cty.Value{"name": args[0], "type": args[1], "ordinal": args[2]}
			if collated {
				value["collation"] = args[3]
			}
			return cty.ObjectVal(value), nil
		},
	})
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
		Params: []function.Parameter{{Name: "target", Type: cty.DynamicPseudoType}},
		Type: function.StaticReturnType(cty.Object(map[string]cty.Type{
			"target": cty.String,
			"type":   cty.String,
		})),
		Impl: func(args []cty.Value, _ cty.Type) (cty.Value, error) {
			target := args[0]
			if target.Type() != cty.String {
				decoded := ctyToAny(target)
				ref, ok := referenceFromAny(decoded)
				if !ok {
					return cty.NilVal, fmt.Errorf("dependency target must be an ID or resource reference")
				}
				target = cty.StringVal(ref.ID)
			}
			return cty.ObjectVal(map[string]cty.Value{
				"target": target,
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
	case json.Number:
		value, err := cty.ParseNumberVal(x.String())
		return value, err
	case []string:
		a := make([]cty.Value, len(x))
		for i := range x {
			a[i] = cty.StringVal(x[i])
		}
		return cty.TupleVal(a), nil
	case []any:
		a := make([]cty.Value, len(x))
		for i := range x {
			value, err := ctyValue(x[i])
			if err != nil {
				return cty.NilVal, err
			}
			a[i] = value
		}
		if len(a) == 0 {
			return cty.EmptyTupleVal, nil
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
	if v.Type().IsObjectType() || v.Type().IsMapType() {
		out := map[string]any{}
		iterator := v.ElementIterator()
		for iterator.Next() {
			key, value := iterator.Element()
			out[key.AsString()] = ctyToAny(value)
		}
		return out
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
	if len(doc.Extra) > 0 || len(doc.Graph.Extra) > 0 {
		// Reuse the metadata block when one exists; otherwise create it.
		var body *hclwrite.Body
		for _, block := range root.Blocks() {
			if block.Type() == "document" {
				body = block.Body()
				break
			}
		}
		if body == nil {
			body = root.AppendNewBlock("document", nil).Body()
			root.AppendNewline()
		}
		if len(doc.Extra) > 0 {
			raw, _ := json.Marshal(doc.Extra)
			body.SetAttributeValue("extra_json", cty.StringVal(string(raw)))
		}
		if len(doc.Graph.Extra) > 0 {
			raw, _ := json.Marshal(doc.Graph.Extra)
			body.SetAttributeValue("graph_extra_json", cty.StringVal(string(raw)))
		}
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
		if r.Source != nil {
			raw, err := json.Marshal(r.Source)
			if err != nil {
				return nil, err
			}
			body.SetAttributeValue("source_json", cty.StringVal(string(raw)))
		} else {
			body.SetAttributeValue("source_json", cty.StringVal("null"))
		}
		if len(r.Extra) > 0 {
			raw, _ := json.Marshal(r.Extra)
			body.SetAttributeValue("extra_json", cty.StringVal(string(raw)))
		}
		if len(r.Name.Extra) > 0 {
			raw, _ := json.Marshal(r.Name.Extra)
			body.SetAttributeValue("name_extra_json", cty.StringVal(string(raw)))
		}
		root.AppendNewline()
	}
	return f.Bytes(), nil
}
