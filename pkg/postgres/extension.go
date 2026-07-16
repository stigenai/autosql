package postgres

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"autosql/pkg/schema"
	"github.com/jackc/pgx/v5"
)

type ExtensionAvailability struct {
	Name, Version, Schema, Comment  string
	Trusted, Superuser, Relocatable bool
	Requires                        []string
}

type ExtensionCatalog struct {
	Versions []ExtensionAvailability
	Paths    []ExtensionUpdatePath
}

type ExtensionPolicy struct {
	Allowed        map[string]bool
	Versions       map[string][]string
	Schemas        map[string][]string
	RequireTrusted bool
}

// ExtensionUpdatePath is one server-advertised path between installed
// extension versions. An empty Path means PostgreSQL cannot perform the
// transition with ALTER EXTENSION UPDATE.
type ExtensionUpdatePath struct {
	Name, Source, Target, Path string
}

type ExtensionDiagnostic struct {
	Name, Class, Message string
}

type ExtensionReport struct {
	Supported   bool
	Diagnostics []ExtensionDiagnostic
}

// InspectExtensionCatalogURL inventories server-installed extension packages;
// it does not execute any extension control or installation script.
func InspectExtensionCatalogURL(ctx context.Context, url string) (ExtensionCatalog, error) {
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		return ExtensionCatalog{}, safeError("inspect extension catalog", url, err)
	}
	defer conn.Close(context.Background())
	rows, err := conn.Query(ctx, `select name,version,coalesce(schema,''),superuser,trusted,relocatable,coalesce(requires,'{}'::name[]),coalesce(comment,'') from pg_available_extension_versions order by name,version`)
	if err != nil {
		return ExtensionCatalog{}, err
	}
	defer rows.Close()
	catalog := ExtensionCatalog{}
	for rows.Next() {
		var value ExtensionAvailability
		if err = rows.Scan(&value.Name, &value.Version, &value.Schema, &value.Superuser, &value.Trusted, &value.Relocatable, &value.Requires, &value.Comment); err != nil {
			return ExtensionCatalog{}, err
		}
		catalog.Versions = append(catalog.Versions, value)
	}
	if err := rows.Err(); err != nil {
		return ExtensionCatalog{}, err
	}
	pathRows, err := conn.Query(ctx, `select names.name,p.source,p.target,coalesce(p.path,'') from (select distinct name from pg_available_extension_versions) names cross join lateral pg_extension_update_paths(names.name) p order by names.name,p.source,p.target`)
	if err != nil {
		return ExtensionCatalog{}, err
	}
	defer pathRows.Close()
	for pathRows.Next() {
		var path ExtensionUpdatePath
		if err := pathRows.Scan(&path.Name, &path.Source, &path.Target, &path.Path); err != nil {
			return ExtensionCatalog{}, err
		}
		catalog.Paths = append(catalog.Paths, path)
	}
	return catalog, pathRows.Err()
}

// ValidateExtensionTransition verifies that PostgreSQL advertises a complete
// update path before a plan containing ALTER EXTENSION UPDATE is published.
func ValidateExtensionTransition(name, currentVersion, desiredVersion string, catalog ExtensionCatalog) error {
	if currentVersion == desiredVersion {
		return nil
	}
	for _, path := range catalog.Paths {
		if path.Name == name && path.Source == currentVersion && path.Target == desiredVersion && path.Path != "" {
			return nil
		}
	}
	return fmt.Errorf("extension %s has no server-advertised update path from %s to %s", name, currentVersion, desiredVersion)
}

// PreflightExtensions aggregates package, allowlist, version, schema, trust,
// dependency, and authority-policy blockers before CREATE EXTENSION is planned.
func PreflightExtensions(document schema.Document, catalog ExtensionCatalog, policy ExtensionPolicy) (ExtensionReport, error) {
	available := map[string][]ExtensionAvailability{}
	for _, value := range catalog.Versions {
		available[value.Name] = append(available[value.Name], value)
	}
	diagnostics := []ExtensionDiagnostic{}
	add := func(name, class, message string) {
		diagnostics = append(diagnostics, ExtensionDiagnostic{Name: name, Class: class, Message: message})
	}
	for _, resource := range document.Graph.Resources {
		if resource.Kind != schema.KindExtension {
			continue
		}
		s := spec(resource)
		name, version, targetSchema := resource.Name.Name, stringValue(s, "version"), resource.Name.Schema
		if !policy.Allowed[name] {
			add(name, "allowlist", "extension is not approved by policy")
		}
		matches := []ExtensionAvailability{}
		for _, candidate := range available[name] {
			if candidate.Version == version {
				matches = append(matches, candidate)
			}
		}
		if len(matches) == 0 {
			add(name, "package", "requested extension version is not installed on the server")
			continue
		}
		candidate := matches[0]
		if allowed := policy.Versions[name]; len(allowed) > 0 && !containsString(allowed, version) {
			add(name, "version", "extension version is outside policy")
		}
		if allowed := policy.Schemas[name]; len(allowed) > 0 && !containsString(allowed, targetSchema) {
			add(name, "schema", "extension target schema is outside policy")
		}
		if candidate.Schema != "" && candidate.Schema != targetSchema {
			add(name, "schema", "non-relocatable extension requires its control-file schema")
		}
		if policy.RequireTrusted && !candidate.Trusted {
			add(name, "authority", "extension requires superuser authority")
		}
		for _, required := range candidate.Requires {
			if !extensionResourcePresent(document, required) {
				add(name, "dependency", fmt.Sprintf("required extension %s is absent", required))
			}
		}
	}
	sort.Slice(diagnostics, func(i, j int) bool {
		if diagnostics[i].Name == diagnostics[j].Name {
			return diagnostics[i].Class < diagnostics[j].Class
		}
		return diagnostics[i].Name < diagnostics[j].Name
	})
	return ExtensionReport{Supported: len(diagnostics) == 0, Diagnostics: diagnostics}, nil
}

func containsString(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}

func extensionResourcePresent(document schema.Document, name string) bool {
	for _, resource := range document.Graph.Resources {
		if resource.Kind == schema.KindExtension && resource.Name.Name == name {
			return true
		}
	}
	return false
}

func extensionRequirementSpec(availability ExtensionAvailability, owner string) map[string]any {
	return map[string]any{"version": availability.Version, "relocatable": availability.Relocatable, "trusted": availability.Trusted, "superuser": availability.Superuser, "requires": availability.Requires, "owner": owner}
}

func validateExtensionRequirement(resource schema.Resource) error {
	s := spec(resource)
	if !allowedKeys(s, "version", "relocatable", "trusted", "superuser", "requires", "owner", "cascade") {
		return unsupported(resource, "unknown extension requirement metadata")
	}
	if stringValue(s, "version") == "" {
		return unsupported(resource, "extension version is required")
	}
	for _, key := range []string{"relocatable", "trusted", "superuser"} {
		if value, exists := s[key]; exists {
			if _, ok := value.(bool); ok {
				continue
			}
			return unsupported(resource, "extension "+key+" must be boolean")
		}
	}
	if value, exists := s["cascade"]; exists {
		if _, ok := value.(bool); !ok {
			return unsupported(resource, "extension cascade must be boolean")
		}
	}
	if strings.TrimSpace(resource.Name.Schema) == "" {
		return unsupported(resource, "extension target schema is required")
	}
	return nil
}

func validateExtensionOptions(resource schema.Resource, options map[string]string) error {
	if err := validateExtensionRequirement(resource); err != nil {
		return err
	}
	name := resource.Name.Name
	if !containsString(splitPatterns(options["extension_allowlist"]), name) {
		return unsupported(resource, "extension is not present in extension_allowlist")
	}
	if exact := strings.TrimSpace(options["extension_version."+name]); exact != "" && exact != stringValue(spec(resource), "version") {
		return unsupported(resource, "extension version is outside the configured exact policy")
	}
	if allowed := splitPatterns(options["extension_schemas."+name]); len(allowed) > 0 && !containsString(allowed, resource.Name.Schema) {
		return unsupported(resource, "extension schema is outside the configured policy")
	}
	if trusted, present := spec(resource)["trusted"].(bool); present && !trusted && !enabled(options, "allow_untrusted_extensions", false) {
		return unsupported(resource, "untrusted extension requires allow_untrusted_extensions=true")
	}
	return nil
}
