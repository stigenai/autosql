package source

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"autosql/pkg/schema"

	"github.com/hashicorp/hcl/v2"
	"github.com/hashicorp/hcl/v2/hclsyntax"
	"github.com/hashicorp/hcl/v2/hclwrite"
	"github.com/zclconf/go-cty/cty"
)

type HCLFormatStyle string

const (
	HCLFormatCanonical HCLFormatStyle = "canonical"
	HCLFormatAuthor    HCLFormatStyle = "author"
)

type HCLFormatOptions struct {
	Style HCLFormatStyle
}

func FormatHCLWithOptions(doc schema.Document, options HCLFormatOptions) ([]byte, error) {
	switch options.Style {
	case "", HCLFormatCanonical:
		return FormatHCL(doc)
	case HCLFormatAuthor:
		return formatAuthorHCL(doc)
	default:
		return nil, fmt.Errorf("unknown HCL format style %q", options.Style)
	}
}

func FormatAuthorHCL(doc schema.Document) ([]byte, error) {
	return FormatHCLWithOptions(doc, HCLFormatOptions{Style: HCLFormatAuthor})
}

func formatAuthorHCL(doc schema.Document) ([]byte, error) {
	if err := doc.Validate(); err != nil {
		return nil, err
	}
	forced := map[string]bool{}
	for attempt := 0; attempt <= len(doc.Graph.Resources); attempt++ {
		formatted, err := renderAuthorHCL(doc, forced)
		if err != nil {
			return nil, err
		}
		back, err := ParseHCL("autosql-author-format-check.hcl", formatted, nil)
		if err != nil {
			for _, resource := range doc.Graph.Resources {
				forced[resource.ID] = true
			}
			return renderAuthorHCL(doc, forced)
		}
		mismatches := authorHCLMismatches(doc, back)
		if len(mismatches) == 0 {
			return formatted, nil
		}
		progress := false
		for _, id := range mismatches {
			if !forced[id] {
				forced[id] = true
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	return nil, fmt.Errorf("author HCL formatter could not produce a lossless mixed representation")
}

func renderAuthorHCL(doc schema.Document, forced map[string]bool) ([]byte, error) {
	file := hclwrite.NewEmptyFile()
	root := file.Body()
	resources := resourceMapHCL(doc.Graph.Resources)
	renameHints, err := schema.DocumentRenameHints(doc)
	if err != nil {
		return nil, err
	}
	appendHCLDocumentMetadata(root, doc, renameHints, resources)
	children := map[string][]schema.Resource{}
	for _, resource := range doc.Graph.Resources {
		if parent, ok := resources[resource.Name.Parent]; ok && authorNestedKind(parent.Kind, resource.Kind) {
			children[parent.ID] = append(children[parent.ID], resource)
		}
	}
	ordered := append([]schema.Resource(nil), doc.Graph.Resources...)
	sort.Slice(ordered, func(i, j int) bool { return ordered[i].ID < ordered[j].ID })
	for _, resource := range ordered {
		if parent, ok := resources[resource.Name.Parent]; ok && authorNestedKind(parent.Kind, resource.Kind) && !forced[parent.ID] {
			continue
		}
		if forced[resource.ID] || !authorResourceSupported(resource, resources) {
			appendCanonicalHCLResource(root, resource)
			continue
		}
		if _, err := appendAuthorHCLResource(root, root, resource, resources, children, forced); err != nil {
			appendCanonicalHCLResource(root, resource)
		}
	}
	return file.Bytes(), nil
}

func authorHCLMismatches(want, got schema.Document) []string {
	wantByID, gotByID := resourceMapHCL(want.Graph.Resources), resourceMapHCL(got.Graph.Resources)
	ids := map[string]bool{}
	for id := range wantByID {
		ids[id] = true
	}
	for id := range gotByID {
		ids[id] = true
	}
	var mismatches []string
	for id := range ids {
		left, leftOK := wantByID[id]
		right, rightOK := gotByID[id]
		leftJSON, _ := json.Marshal(left)
		rightJSON, _ := json.Marshal(right)
		if !leftOK || !rightOK || string(leftJSON) != string(rightJSON) {
			mismatches = append(mismatches, id)
		}
	}
	sort.Strings(mismatches)
	return mismatches
}

func authorNestedKind(parent, child schema.Kind) bool {
	if parent == schema.KindTable {
		switch child {
		case schema.KindColumn, schema.KindPrimaryKey, schema.KindUniqueConstraint, schema.KindCheckConstraint, schema.KindForeignKey, schema.KindIndex, schema.KindTrigger, schema.KindPolicy:
			return true
		}
	}
	return (parent == schema.KindView || parent == schema.KindMaterializedView) && (child == schema.KindColumn || child == schema.KindTrigger || child == schema.KindIndex)
}

func authorResourceSupported(resource schema.Resource, resources map[string]schema.Resource) bool {
	if resource.Name.Catalog != "" || len(resource.Extra) != 0 || len(resource.Name.Extra) != 0 {
		return false
	}
	for key := range resource.Annotations {
		if key != "comment" {
			return false
		}
	}
	for _, dependency := range resource.Dependencies {
		if len(dependency.Extra) != 0 {
			return false
		}
		if _, ok := resources[dependency.Target]; !ok {
			return false
		}
	}
	return knownHCLKind(string(resource.Kind))
}

func resourceMapHCL(values []schema.Resource) map[string]schema.Resource {
	out := make(map[string]schema.Resource, len(values))
	for _, resource := range values {
		out[resource.ID] = resource
	}
	return out
}

func appendAuthorHCLResource(parent, canonicalRoot *hclwrite.Body, resource schema.Resource, resources map[string]schema.Resource, children map[string][]schema.Resource, forced map[string]bool) (*hclwrite.Block, error) {
	block := parent.AppendNewBlock(string(resource.Kind), []string{resource.Name.Name})
	body := block.Body()
	if resource.Name.Schema != "" && resource.Kind != schema.KindGrant {
		if namespace := findHCLResource(resources, schema.KindSchema, resource.Name.Schema); namespace.ID != "" {
			body.SetAttributeRaw("schema", tokensForHCLReference(namespace, resources))
		} else {
			body.SetAttributeValue("schema", cty.StringVal(resource.Name.Schema))
		}
	}
	var values map[string]any
	if len(resource.Spec) > 0 {
		decoder := json.NewDecoder(strings.NewReader(string(resource.Spec)))
		decoder.UseNumber()
		if err := decoder.Decode(&values); err != nil {
			return nil, err
		}
	}
	if values == nil {
		values = map[string]any{}
	}
	if comment := resource.Annotations["comment"]; comment != "" {
		body.SetAttributeValue("comment", cty.StringVal(comment))
	}
	if resource.Source != nil {
		raw, _ := json.Marshal(resource.Source)
		body.SetAttributeValue("source_json", cty.StringVal(string(raw)))
	} else {
		body.SetAttributeValue("source_json", cty.StringVal("null"))
	}
	if err := writeAuthorIdentityAttributes(body, resource, values, resources); err != nil {
		return nil, err
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		if authorSpecialAttribute(resource.Kind, key) {
			continue
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		if key == "definition" && (resource.Kind == schema.KindFunction || resource.Kind == schema.KindProcedure || resource.Kind == schema.KindView || resource.Kind == schema.KindMaterializedView) {
			if text, ok := values[key].(string); ok {
				body.SetAttributeRaw(key, heredocHCLTokens(text))
				continue
			}
		}
		value, err := ctyValue(values[key])
		if err != nil {
			return nil, err
		}
		body.SetAttributeValue(key, value)
	}
	if dependencies := authorDependencyTokens(resource, resources); len(dependencies) > 0 {
		body.SetAttributeRaw("dependencies", hclwrite.TokensForTuple(dependencies))
	}
	orderedChildren := append([]schema.Resource(nil), children[resource.ID]...)
	sort.Slice(orderedChildren, func(i, j int) bool { return orderedChildren[i].ID < orderedChildren[j].ID })
	for _, child := range orderedChildren {
		if forced[child.ID] || !authorResourceSupported(child, resources) {
			appendCanonicalHCLResource(canonicalRoot, child)
			continue
		}
		if _, err := appendAuthorHCLResource(body, canonicalRoot, child, resources, children, forced); err != nil {
			return nil, err
		}
	}
	parent.AppendNewline()
	return block, nil
}

func writeAuthorIdentityAttributes(body *hclwrite.Body, resource schema.Resource, values map[string]any, resources map[string]schema.Resource) error {
	setRole := func(attribute string) {
		name, _ := values[attribute].(string)
		if role := findHCLResource(resources, schema.KindRole, name); role.ID != "" {
			body.SetAttributeRaw(attribute, tokensForHCLReference(role, resources))
		} else if name != "" {
			body.SetAttributeValue(attribute, cty.StringVal(name))
		}
	}
	setRole("owner")
	switch resource.Kind {
	case schema.KindPolicy:
		items := []hclwrite.Tokens{}
		for _, roleName := range anyStringSliceHCL(values["roles"]) {
			if strings.EqualFold(roleName, "public") {
				items = append(items, hclwrite.TokensForValue(cty.StringVal("public")))
			} else if role := findHCLResource(resources, schema.KindRole, roleName); role.ID != "" {
				items = append(items, tokensForHCLReference(role, resources))
			} else {
				return fmt.Errorf("policy role %q is not declared", roleName)
			}
		}
		body.SetAttributeRaw("roles", hclwrite.TokensForTuple(items))
	case schema.KindMembership:
		for _, key := range []string{"parent", "member", "grantor"} {
			setRole(key)
		}
	case schema.KindGrant:
		target, ok := resources[resource.Name.Parent]
		if !ok {
			return fmt.Errorf("grant target is missing")
		}
		body.SetAttributeRaw("target", tokensForHCLReference(target, resources))
		setRole("grantee")
		setRole("grantor")
	case schema.KindDefaultPrivilege:
		setRole("owner")
		setRole("grantee")
		if namespace := findHCLResource(resources, schema.KindSchema, stringValueHCL(values["schema"])); namespace.ID != "" {
			body.SetAttributeRaw("in_schema", tokensForHCLReference(namespace, resources))
		}
	}
	return nil
}

func authorSpecialAttribute(kind schema.Kind, key string) bool {
	if key == "owner" {
		return true
	}
	switch kind {
	case schema.KindPolicy:
		return key == "roles"
	case schema.KindMembership:
		return key == "parent" || key == "member" || key == "grantor"
	case schema.KindGrant:
		return key == "grantee" || key == "grantor"
	case schema.KindDefaultPrivilege:
		return key == "owner" || key == "grantee" || key == "schema"
	}
	return false
}

func authorDependencyTokens(resource schema.Resource, resources map[string]schema.Resource) []hclwrite.Tokens {
	items := []hclwrite.Tokens{}
	for _, dependency := range resource.Dependencies {
		if dependency.Type == schema.DependencyContains && dependency.Target == resource.Name.Parent {
			continue
		}
		target, ok := resources[dependency.Target]
		if !ok {
			continue
		}
		items = append(items, hclwrite.TokensForFunctionCall(string(dependency.Type), tokensForHCLReference(target, resources)))
	}
	return items
}

func tokensForHCLReference(resource schema.Resource, resources map[string]schema.Resource) hclwrite.Tokens {
	root := string(resource.Kind)
	parent, nested := resources[resource.Name.Parent]
	if nested && (parent.Kind == schema.KindTable || parent.Kind == schema.KindView || parent.Kind == schema.KindMaterializedView) {
		root = string(parent.Kind)
	}
	traversal := hcl.Traversal{hcl.TraverseRoot{Name: root}}
	index := func(value string) { traversal = append(traversal, hcl.TraverseIndex{Key: cty.StringVal(value)}) }
	if resource.Kind == schema.KindSchema || resource.Kind == schema.KindRole || resource.Name.Schema == "" {
		index(resource.Name.Name)
	} else if nested && (parent.Kind == schema.KindTable || parent.Kind == schema.KindView || parent.Kind == schema.KindMaterializedView) {
		index(parent.Name.Schema)
		index(parent.Name.Name)
		index(string(resource.Kind))
		index(resource.Name.Name)
	} else {
		index(resource.Name.Schema)
		index(resource.Name.Name)
	}
	return hclwrite.TokensForTraversal(traversal)
}

func heredocHCLTokens(value string) hclwrite.Tokens {
	delimiter := "AUTOSQL_HCL"
	for strings.Contains(value, "\n"+delimiter) {
		delimiter += "_X"
	}
	tokens := hclwrite.Tokens{
		{Type: hclsyntax.TokenOHeredoc, Bytes: []byte("<<-" + delimiter + "\n")},
		{Type: hclsyntax.TokenStringLit, Bytes: []byte(strings.TrimSuffix(value, "\n") + "\n")},
		{Type: hclsyntax.TokenCHeredoc, Bytes: []byte(delimiter + "\n")},
	}
	if !strings.HasSuffix(value, "\n") {
		return hclwrite.TokensForFunctionCall("chomp", tokens)
	}
	return tokens
}

func findHCLResource(resources map[string]schema.Resource, kind schema.Kind, name string) schema.Resource {
	for _, resource := range resources {
		if resource.Kind == kind && resource.Name.Name == name {
			return resource
		}
	}
	return schema.Resource{}
}

func anyStringSliceHCL(value any) []string {
	items, _ := value.([]any)
	result := make([]string, 0, len(items))
	for _, item := range items {
		if text, ok := item.(string); ok {
			result = append(result, text)
		}
	}
	return result
}

func appendHCLDocumentMetadata(root *hclwrite.Body, doc schema.Document, renameHints []schema.RenameHint, resources map[string]schema.Resource) {
	annotations := withoutKey(withoutKey(doc.Annotations, "hcl_imports"), schema.RenameHintsAnnotation)
	if len(annotations) == 0 && len(doc.Extra) == 0 && len(doc.Graph.Extra) == 0 {
		// Rename intent is emitted below as first-class author HCL.
	} else {
		body := root.AppendNewBlock("document", nil).Body()
		if len(annotations) > 0 {
			raw, _ := json.Marshal(annotations)
			body.SetAttributeValue("annotations_json", cty.StringVal(string(raw)))
		}
		if len(doc.Extra) > 0 {
			raw, _ := json.Marshal(doc.Extra)
			body.SetAttributeValue("extra_json", cty.StringVal(string(raw)))
		}
		if len(doc.Graph.Extra) > 0 {
			raw, _ := json.Marshal(doc.Graph.Extra)
			body.SetAttributeValue("graph_extra_json", cty.StringVal(string(raw)))
		}
		root.AppendNewline()
	}
	for _, hint := range renameHints {
		body := root.AppendNewBlock("moved", nil).Body()
		body.SetAttributeValue("from", cty.StringVal(hint.From))
		if target, ok := resources[hint.To]; ok {
			body.SetAttributeRaw("to", tokensForHCLReference(target, resources))
		} else {
			body.SetAttributeValue("to", cty.StringVal(hint.To))
		}
		root.AppendNewline()
	}
}

func appendCanonicalHCLResource(root *hclwrite.Body, resource schema.Resource) {
	block := root.AppendNewBlock("resource", []string{string(resource.Kind), resource.Name.Name})
	body := block.Body()
	if resource.Name.Schema != "" {
		body.SetAttributeValue("schema", cty.StringVal(resource.Name.Schema))
	}
	if resource.Name.Catalog != "" {
		body.SetAttributeValue("catalog", cty.StringVal(resource.Name.Catalog))
	}
	if resource.Name.Parent != "" {
		body.SetAttributeValue("parent", cty.StringVal(resource.Name.Parent))
	}
	if len(resource.Spec) > 0 {
		body.SetAttributeValue("spec_json", cty.StringVal(string(resource.Spec)))
	}
	if len(resource.Dependencies) > 0 {
		raw, _ := json.Marshal(resource.Dependencies)
		body.SetAttributeValue("deps_json", cty.StringVal(string(raw)))
	}
	if len(resource.Annotations) > 0 {
		raw, _ := json.Marshal(resource.Annotations)
		body.SetAttributeValue("annotations_json", cty.StringVal(string(raw)))
	}
	if resource.Source != nil {
		raw, _ := json.Marshal(resource.Source)
		body.SetAttributeValue("source_json", cty.StringVal(string(raw)))
	} else {
		body.SetAttributeValue("source_json", cty.StringVal("null"))
	}
	if len(resource.Extra) > 0 {
		raw, _ := json.Marshal(resource.Extra)
		body.SetAttributeValue("extra_json", cty.StringVal(string(raw)))
	}
	if len(resource.Name.Extra) > 0 {
		raw, _ := json.Marshal(resource.Name.Extra)
		body.SetAttributeValue("name_extra_json", cty.StringVal(string(raw)))
	}
	root.AppendNewline()
}
