package postgres

import (
	"fmt"
	"slices"
	"sort"
	"strings"

	"autosql/pkg/schema"
)

type compositeAttribute struct {
	Name      string
	Type      string
	Collation string
	Ordinal   int
}

func parseCompositeAttributes(resource schema.Resource) ([]compositeAttribute, error) {
	values := spec(resource)
	if !allowedKeys(values, "attributes", "owner") {
		return nil, unsupported(resource, "unknown composite type semantics")
	}
	raw, ok := values["attributes"].([]any)
	if !ok || len(raw) == 0 {
		return nil, unsupported(resource, "composite attributes must be a non-empty ordered list")
	}
	attributes := make([]compositeAttribute, 0, len(raw))
	names := map[string]bool{}
	ordinals := map[int]bool{}
	for index, value := range raw {
		item, ok := value.(map[string]any)
		if !ok || !allowedKeys(item, "name", "type", "collation", "ordinal", "not_null") {
			return nil, unsupported(resource, fmt.Sprintf("composite attribute %d has unknown semantics", index+1))
		}
		attribute := compositeAttribute{Name: stringValue(item, "name"), Type: stringValue(item, "type"), Collation: stringValue(item, "collation"), Ordinal: numberAsInt(item, "ordinal")}
		if attribute.Ordinal == 0 {
			attribute.Ordinal = index + 1
		}
		if boolValue(item, "not_null") {
			return nil, unsupported(resource, "PostgreSQL composite attributes cannot be declared NOT NULL")
		}
		if strings.TrimSpace(attribute.Name) == "" || strings.TrimSpace(attribute.Type) == "" {
			return nil, unsupported(resource, fmt.Sprintf("composite attribute %d requires name and type", index+1))
		}
		if names[attribute.Name] {
			return nil, unsupported(resource, "composite attribute names must be unique")
		}
		if attribute.Ordinal != index+1 || ordinals[attribute.Ordinal] {
			return nil, unsupported(resource, "composite attribute ordinals must be contiguous, unique, and match list order")
		}
		if err := validateNativeAtom(attribute.Type); err != nil {
			return nil, unsupported(resource, "composite attribute type contains unsafe syntax")
		}
		if attribute.Collation != "" {
			if err := validateNativeAtom(attribute.Collation); err != nil {
				return nil, unsupported(resource, "composite attribute collation contains unsafe syntax")
			}
		}
		names[attribute.Name], ordinals[attribute.Ordinal] = true, true
		attributes = append(attributes, attribute)
	}
	return attributes, nil
}

func validateCompositeSpec(resource schema.Resource, resources map[string]schema.Resource) error {
	attributes, err := parseCompositeAttributes(resource)
	if err != nil {
		return err
	}
	expected := []string{}
	for _, attribute := range attributes {
		matched := false
		for id, target := range resources {
			switch target.Kind {
			case schema.KindEnum, schema.KindDomain, schema.KindComposite:
				if target.ID != resource.ID && typeReferenceMatches(attribute.Type, resource.Name.Schema, target.Name) {
					expected = append(expected, id)
					matched = true
				}
			}
		}
		if _, core := parseCoreColumnType(attribute.Type); !core && !matched && !hasExtensionTypeDependency(resource, resources) {
			return unsupported(resource, "non-core composite attribute type requires an exact type or extension dependency")
		}
	}
	actual := []string{}
	for _, dependency := range resource.Dependencies {
		if dependency.Type == schema.DependencyUses && resources[dependency.Target].Kind != schema.KindExtension {
			actual = append(actual, dependency.Target)
		}
	}
	sort.Strings(expected)
	sort.Strings(actual)
	expected = slices.Compact(expected)
	actual = slices.Compact(actual)
	if !slices.Equal(expected, actual) {
		return unsupported(resource, "composite attribute dependencies do not exactly match attribute types")
	}
	return nil
}

func hasExtensionTypeDependency(resource schema.Resource, resources map[string]schema.Resource) bool {
	for _, dependency := range resource.Dependencies {
		if dependency.Type == schema.DependencyUses && resources[dependency.Target].Kind == schema.KindExtension {
			return true
		}
	}
	return false
}

func renderCompositeAlter(before, after schema.Resource, options map[string]string) ([]string, error) {
	beforeAttributes, err := parseCompositeAttributes(before)
	if err != nil {
		return nil, err
	}
	afterAttributes, err := parseCompositeAttributes(after)
	if err != nil {
		return nil, err
	}
	beforeByName := map[string]compositeAttribute{}
	afterByName := map[string]compositeAttribute{}
	for _, attribute := range beforeAttributes {
		beforeByName[attribute.Name] = attribute
	}
	for _, attribute := range afterAttributes {
		afterByName[attribute.Name] = attribute
	}
	renamed := map[string]string{}
	for index := 0; index < len(beforeAttributes) && index < len(afterAttributes); index++ {
		old, desired := beforeAttributes[index], afterAttributes[index]
		if old.Name != desired.Name && afterByName[old.Name].Name == "" && beforeByName[desired.Name].Name == "" {
			renamed[old.Name] = desired.Name
		}
	}
	transformed := make([]string, 0, len(beforeAttributes))
	for _, attribute := range beforeAttributes {
		name := attribute.Name
		if renamed[name] != "" {
			name = renamed[name]
		}
		if afterByName[name].Name != "" {
			transformed = append(transformed, name)
		}
	}
	desiredExisting := make([]string, 0, len(afterAttributes))
	seenAddition := false
	for _, attribute := range afterAttributes {
		oldName := attribute.Name
		for source, target := range renamed {
			if target == attribute.Name {
				oldName = source
			}
		}
		if beforeByName[oldName].Name == "" {
			seenAddition = true
			continue
		}
		if seenAddition {
			return nil, unsupported(after, "composite attributes can only be added after all retained attributes")
		}
		desiredExisting = append(desiredExisting, attribute.Name)
	}
	if !slices.Equal(transformed, desiredExisting) {
		return nil, unsupported(after, "PostgreSQL cannot reorder retained composite attributes")
	}
	suffix := " RESTRICT"
	if enabled(options, "composite_attribute_cascade", false) {
		suffix = " CASCADE"
	}
	prefix := "ALTER TYPE " + qualified(after.Name) + " "
	statements := []string{}
	// Release names before rename to avoid collisions with dropped attributes.
	for _, attribute := range beforeAttributes {
		name := attribute.Name
		if renamed[name] != "" {
			name = renamed[name]
		}
		if afterByName[name].Name == "" {
			statements = append(statements, prefix+"DROP ATTRIBUTE "+quote(attribute.Name)+suffix)
		}
	}
	for _, attribute := range beforeAttributes {
		if target := renamed[attribute.Name]; target != "" {
			statements = append(statements, prefix+"RENAME ATTRIBUTE "+quote(attribute.Name)+" TO "+quote(target)+suffix)
		}
	}
	for _, desired := range afterAttributes {
		oldName := desired.Name
		for source, target := range renamed {
			if target == desired.Name {
				oldName = source
			}
		}
		old := beforeByName[oldName]
		if old.Name == "" {
			definition := quote(desired.Name) + " " + desired.Type
			if desired.Collation != "" {
				definition += " COLLATE " + desired.Collation
			}
			statements = append(statements, prefix+"ADD ATTRIBUTE "+definition+suffix)
			continue
		}
		if old.Type != desired.Type || old.Collation != desired.Collation {
			if !enabled(options, "allow_composite_attribute_type_change", false) {
				return nil, unsupported(after, "composite attribute type change requires allow_composite_attribute_type_change=true")
			}
			definition := desired.Type
			if desired.Collation != "" {
				definition += " COLLATE " + desired.Collation
			}
			statements = append(statements, prefix+"ALTER ATTRIBUTE "+quote(desired.Name)+" TYPE "+definition+suffix)
		}
	}
	if len(statements) == 0 {
		return nil, unsupported(after, "composite alteration has no renderable semantics")
	}
	return statements, nil
}
