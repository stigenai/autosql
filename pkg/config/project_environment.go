package config

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"autosql/pkg/secret"
)

var revisionName = regexp.MustCompile(`^[a-z_][a-z0-9_]{0,62}$`)

func (c *Config) Validate() error {
	if c.Version == LegacyVersion {
		return c.validateLegacy()
	}
	if c.Version != CurrentVersion {
		return c.problem("/version", "supported configuration versions are 1 and 2")
	}
	if len(c.Environments) == 0 {
		return c.problem("/environments", "project has no environments")
	}
	if c.Environment == "" {
		return c.problem("/environment", "select exactly one environment")
	}
	if _, ok := c.Environments[c.Environment]; !ok {
		return c.problem("/environment", "selected environment is not defined")
	}
	for _, name := range sortedKeys(c.Environments) {
		if err := c.validateEnvironment(name, c.Environments[name]); err != nil {
			return err
		}
	}
	return c.validateProject()
}

func (c *Config) validateEnvironment(name string, e Environment) error {
	field := environmentPath(name)
	if e.Target.Validate() != nil {
		return c.problem(field+"/target", "target must be an env:// or file:// secret reference")
	}
	if e.DevDatabase != "" && e.DevDatabase.Validate() != nil {
		return c.problem(field+"/dev_database", "development database must be a secret reference")
	}
	if len(e.SchemaSources) == 0 {
		return c.problem(field+"/schema_sources", "at least one local schema source is required")
	}
	if !localProjectPath(e.MigrationDir) {
		return c.problem(field+"/migration_dir", "migration directory must be project-relative")
	}
	if e.RevisionSchema != "" && !revisionName.MatchString(e.RevisionSchema) {
		return c.problem(field+"/revision_schema", "revision schema is invalid")
	}
	if err := c.validateDuration(field+"/timeout", e.Timeout); err != nil {
		return err
	}
	return c.validateEnvironmentLists(field, e)
}

func (c *Config) validateEnvironmentLists(field string, e Environment) error {
	for i, source := range e.SchemaSources {
		path := fmt.Sprintf("%s/schema_sources/%d", field, i)
		if source.Kind != "sql" && source.Kind != "json" && source.Kind != "hcl" {
			return c.problem(path+"/kind", "schema source kind must be sql, json or hcl")
		}
		if !localProjectPath(source.Path) {
			return c.problem(path+"/path", "schema source must be a project-relative path")
		}
	}
	for _, pair := range []struct {
		name   string
		values []string
	}{{"include", e.Include}, {"exclude", e.Exclude}} {
		for i, value := range pair.values {
			if strings.TrimSpace(value) == "" {
				return c.problem(fmt.Sprintf("%s/%s/%d", field, pair.name, i), "filter must not be blank")
			}
		}
	}
	return nil
}

func (c *Config) recordOverrides(getenv func(string) (string, bool), o Overrides) {
	if o.SourceInputs != nil {
		c.origins["/schema_sources"] = "flag"
	}
	for env, field := range map[string]string{"AUTOSQL_TARGET": "target", "AUTOSQL_DEV_DATABASE": "dev_database", "AUTOSQL_SCHEMA_SOURCES": "schema_sources", "AUTOSQL_MIGRATION_DIR": "migration_dir", "AUTOSQL_REVISION_SCHEMA": "revision_schema", "AUTOSQL_INCLUDE": "include", "AUTOSQL_EXCLUDE": "exclude", "AUTOSQL_TIMEOUT": "timeout"} {
		if _, ok := getenv(env); ok {
			c.origins["/"+field] = "environment"
		}
	}
	for field, value := range map[string]string{"target": o.Target, "dev_database": o.DevDatabase, "migration_dir": o.MigrationDir, "revision_schema": o.RevisionSchema, "timeout": o.Timeout} {
		if value != "" {
			c.origins["/"+field] = "flag"
		}
	}
	for field, values := range map[string][]string{"schema_sources": o.SchemaSources, "include": o.Include, "exclude": o.Exclude} {
		if values != nil {
			c.origins["/"+field] = "flag"
		}
	}
}

type LocalInputs struct {
	Root          string
	SchemaSources []SchemaSource
	MigrationDir  string
}

// InspectLocal checks selected local inputs without acquiring a secret resolver.
// Test discovery can use it without the all-target behavior of legacy Preflight.
func (c *Config) InspectLocal() (LocalInputs, error) {
	if err := c.Validate(); err != nil {
		return LocalInputs{}, err
	}
	root, err := filepath.Abs(filepath.Dir(c.path))
	if err != nil {
		return LocalInputs{}, c.problem("", "project directory is unavailable")
	}
	if c.Version == CurrentVersion {
		root, err = filepath.EvalSymlinks(root)
		if err != nil {
			return LocalInputs{}, c.problem("", "project directory is unavailable")
		}
	}
	e := c.Environments[c.Environment]
	result := LocalInputs{Root: root, SchemaSources: append([]SchemaSource(nil), e.SchemaSources...)}
	for i, source := range result.SchemaSources {
		path, err := c.inspectLocalPath(root, source.Path, fmt.Sprintf("%s/schema_sources/%d/path", environmentPath(c.Environment), i), false)
		if err != nil {
			return LocalInputs{}, err
		}
		result.SchemaSources[i].Path = path
	}
	result.MigrationDir, err = c.inspectLocalPath(root, e.MigrationDir, environmentPath(c.Environment)+"/migration_dir", true)
	if err != nil {
		return LocalInputs{}, err
	}
	return result, nil
}

func (c *Config) inspectLocalPath(root, path, field string, directory bool) (string, error) {
	if !filepath.IsAbs(path) {
		path = filepath.Join(root, path)
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", c.problem(field, "local input is unavailable")
	}
	if directory {
		if !info.IsDir() {
			return "", c.problem(field, "migration directory is unavailable")
		}
	} else if !info.Mode().IsRegular() {
		return "", c.problem(field, "schema source is not an available regular file")
	}
	if c.Version == CurrentVersion && !containedProjectPath(root, path) {
		return "", c.problem(field, "local input must remain inside the project")
	}
	return path, nil
}

// This is local inspection, not a file-open authorization token. Consumers that
// later read inputs must enforce containment again when opening those inputs.
func containedProjectPath(root, path string) bool {
	resolved, err := filepath.EvalSymlinks(path)
	if err != nil {
		return false
	}
	relative, err := filepath.Rel(root, resolved)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

// ResolveTarget is an explicit production-target operation. Neither parsing nor
// InspectLocal calls it. The existing Preflight API retains its v1 behavior.
func (c *Config) ResolveTarget(ctx context.Context, resolver *secret.Resolver) (string, error) {
	if err := c.Validate(); err != nil {
		return "", err
	}
	if resolver == nil {
		resolver = secret.NewResolver()
	}
	local := *resolver
	if local.BaseDir == "" {
		local.BaseDir = filepath.Dir(c.path)
	}
	value, err := local.Resolve(ctx, c.Environments[c.Environment].Target)
	if err != nil {
		return "", c.problem(environmentPath(c.Environment)+"/target", "target secret is unavailable")
	}
	return value, nil
}
