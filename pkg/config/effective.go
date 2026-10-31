package config

import (
	"path/filepath"
	"slices"
	"strings"
	"time"
)

func normalizedEnvironment(e Environment) (Environment, []string) {
	e = cloneEnvironment(e)
	var defaults []string
	if e.Timeout == "" {
		e.Timeout = "30s"
		defaults = append(defaults, "/timeout")
	}
	if e.RevisionSchema == "" {
		e.RevisionSchema = "autosql_revision"
		defaults = append(defaults, "/revision_schema")
	}
	if e.Tests.Parallel == 0 {
		e.Tests.Parallel = 1
		defaults = append(defaults, "/tests/parallel")
	}
	if e.Tests.Timeout == "" {
		e.Tests.Timeout = "5m0s"
		defaults = append(defaults, "/tests/timeout")
	}
	e.Timeout = normalizedDuration(e.Timeout)
	e.Tests.Timeout = normalizedDuration(e.Tests.Timeout)
	e.MigrationDir = normalizedPath(e.MigrationDir)
	e.Registry.Lock = normalizedPath(e.Registry.Lock)
	e.Reporting.JUnit = normalizedPath(e.Reporting.JUnit)
	for i := range e.SchemaSources {
		e.SchemaSources[i].Path = normalizedPath(e.SchemaSources[i].Path)
	}
	for i := range e.PolicyFiles {
		e.PolicyFiles[i] = normalizedPath(e.PolicyFiles[i])
	}
	for i := range e.Tests.Files {
		e.Tests.Files[i] = normalizedPath(e.Tests.Files[i])
	}
	return e, defaults
}

func normalizedDuration(value string) string {
	if duration, err := time.ParseDuration(value); err == nil {
		return duration.String()
	}
	return value
}
func normalizedPath(value string) string {
	if value == "" {
		return ""
	}
	return filepath.ToSlash(filepath.Clean(value))
}

func normalizedFactory(f DevelopmentFactory) (DevelopmentFactory, []string) {
	f.Allowlist = slices.Clone(f.Allowlist)
	f.ProtectedTargets = slices.Clone(f.ProtectedTargets)
	var defaults []string
	if f.Isolation == "" {
		f.Isolation = "database"
		defaults = append(defaults, "/isolation")
	}
	if f.StartupTimeout == "" {
		f.StartupTimeout = "2m"
		defaults = append(defaults, "/startup_timeout")
	}
	if f.CaseTimeout == "" {
		f.CaseTimeout = "5m"
		defaults = append(defaults, "/case_timeout")
	}
	if f.CleanupTimeout == "" {
		f.CleanupTimeout = "10s"
		defaults = append(defaults, "/cleanup_timeout")
	}
	f.StartupTimeout = normalizedDuration(f.StartupTimeout)
	f.CaseTimeout = normalizedDuration(f.CaseTimeout)
	f.CleanupTimeout = normalizedDuration(f.CleanupTimeout)
	return f, defaults
}

func (c *Config) effectiveFactory(origins map[string]FieldOrigin, name string) *DevelopmentFactory {
	f, exists := c.DevFactories[name]
	if !exists {
		return nil
	}
	f, defaults := normalizedFactory(f)
	prefix := "/dev_factories/" + pointerPart(name)
	for field, at := range c.locations {
		if strings.HasPrefix(field, prefix+"/") {
			r := at
			origins["/development_factory"+strings.TrimPrefix(field, prefix)] = FieldOrigin{Kind: "file", Range: &r}
		}
	}
	for _, field := range defaults {
		at := c.location(prefix + field)
		origins["/development_factory"+field] = FieldOrigin{Kind: "file", Range: &at, Default: true}
	}
	return &f
}

func (c *Config) location(field string) SourceRange {
	for {
		if at, ok := c.locations[field]; ok {
			return at
		}
		index := strings.LastIndex(field, "/")
		if index < 0 {
			break
		}
		field = field[:index]
	}
	filename := c.path
	if filename == "" {
		filename = "<config>"
	}
	return SourceRange{Filename: filename, StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 1}
}

func (c *Config) fieldOrigin(field string) string {
	field = strings.TrimPrefix(field, environmentPath(c.Environment))
	for {
		if origin, ok := c.origins[field]; ok {
			return origin
		}
		index := strings.LastIndex(field, "/")
		if index < 0 {
			break
		}
		field = field[:index]
	}
	return "file"
}
