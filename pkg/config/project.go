package config

import (
	"fmt"
	"net/url"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"autosql/pkg/secret"
)

const LegacyVersion = 1
const MaxEnvironments = 256

// Project values describe intent and references. Parsing this model has no
// secret resolver, network client, database connection or program-source hook.
type TestSettings struct {
	Files    []string `json:"files,omitempty"`
	Parallel int      `json:"parallel,omitempty"`
	Timeout  string   `json:"timeout,omitempty"`
}

type RegistryReference struct {
	URL  string `json:"url,omitempty"`
	Lock string `json:"lock,omitempty"`
}

type ReportingDefaults struct {
	JSON  bool   `json:"json,omitempty"`
	JUnit string `json:"junit,omitempty"`
}

// Allowlist names credential-free development identities. For existing_admin
// these are PostgreSQL endpoint/database URLs; for container it names the
// explicitly selected Docker-compatible daemon. Runtime must verify the actual
// identity and live ownership before mutation; a parsed factory grants neither.
type DevelopmentFactory struct {
	Mode                string           `json:"mode"`
	Admin               secret.Reference `json:"admin,omitempty"`
	AdminClassification string           `json:"admin_classification,omitempty"`
	Endpoint            string           `json:"endpoint,omitempty"`
	PostgresVersion     int              `json:"postgres_version"`
	Image               string           `json:"image,omitempty"`
	Allowlist           []string         `json:"allowlist"`
	ProtectedTargets    []string         `json:"protected_targets,omitempty"`
	Isolation           string           `json:"isolation,omitempty"`
	StartupTimeout      string           `json:"startup_timeout,omitempty"`
	CaseTimeout         string           `json:"case_timeout,omitempty"`
	CleanupTimeout      string           `json:"cleanup_timeout,omitempty"`
}

type SourceRange struct {
	Filename    string `json:"file"`
	StartLine   int    `json:"line"`
	StartColumn int    `json:"column"`
	EndLine     int    `json:"end_line"`
	EndColumn   int    `json:"end_column"`
}

type FieldOrigin struct {
	Default bool         `json:"default,omitempty"`
	Kind    string       `json:"kind"`
	Range   *SourceRange `json:"range,omitempty"`
}

type LocatedError struct {
	Code    string      `json:"code"`
	Field   string      `json:"field,omitempty"`
	Range   SourceRange `json:"range"`
	Message string      `json:"message"`
}

func (e *LocatedError) Error() string {
	field := e.Field
	if field != "" {
		field += ": "
	}
	return fmt.Sprintf("%s:%d:%d: %s%s", e.Range.Filename, e.Range.StartLine, e.Range.StartColumn, field, e.Message)
}

func (c *Config) problem(field, message string) error {
	return &LocatedError{Code: "invalid_config", Field: field, Range: c.FieldRange(field), Message: message}
}

// FieldRange identifies a configuration field by its root-relative JSON pointer.
// Override values point to their flag/environment origin, not stale file text.
func (c *Config) FieldRange(field string) SourceRange {
	at := c.location(field)
	if origin := c.fieldOrigin(field); origin != "file" {
		at = SourceRange{Filename: "<" + origin + ">", StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 1}
	}
	return at
}

func pointerPart(value string) string {
	return strings.ReplaceAll(strings.ReplaceAll(value, "~", "~0"), "/", "~1")
}
func environmentPath(name string) string { return "/environments/" + pointerPart(name) }

var projectName = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9_.-]*(/[a-zA-Z0-9][a-zA-Z0-9_.-]*)?$`)
var imageDigest = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)

func localProjectPath(path string) bool {
	clean := filepath.ToSlash(filepath.Clean(path))
	return strings.TrimSpace(path) != "" && !filepath.IsAbs(path) && !strings.Contains(path, ":") && !strings.Contains(path, "\\") && clean != ".." && !strings.HasPrefix(clean, "../")
}

func (c *Config) validateProject() error {
	if len(c.Environments) > MaxEnvironments {
		return c.problem("/environments", "at most 256 environments are supported")
	}
	for _, name := range sortedKeys(c.DevFactories) {
		factory := c.DevFactories[name]
		field := "/dev_factories/" + pointerPart(name)
		if !projectName.MatchString(name) || strings.Contains(name, "/") {
			return c.problem(field, "factory name is invalid")
		}
		if err := c.validateFactory(field, factory); err != nil {
			return err
		}
	}
	for _, name := range sortedKeys(c.Environments) {
		environment := c.Environments[name]
		if !projectName.MatchString(name) {
			return c.problem(environmentPath(name), "environment name is invalid")
		}
		if err := c.validateProjectEnvironment(name, environment); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) validateProjectEnvironment(name string, e Environment) error {
	field := environmentPath(name)
	if e.Classification != "" && e.Classification != "development" && e.Classification != "production" {
		return c.problem(field+"/classification", "classification must be development or production")
	}
	if e.DevFactory != "" {
		if _, ok := c.DevFactories[e.DevFactory]; !ok {
			return c.problem(field+"/dev_factory", "development factory is not declared")
		}
	}
	if e.DevDatabase != "" && e.DevFactory != "" {
		return c.problem(field+"/dev_factory", "choose dev_database or dev_factory, not both")
	}
	if err := c.validateTestSettings(field, e.Tests); err != nil {
		return err
	}
	return c.validateProjectReferences(field, e)
}

func (c *Config) validateTestSettings(field string, tests TestSettings) error {
	_, parallelSet := c.locations[field+"/tests/parallel"]
	if tests.Parallel < 0 || tests.Parallel > 32 || parallelSet && tests.Parallel == 0 {
		return c.problem(field+"/tests/parallel", "test parallelism must be between 1 and 32 when set")
	}
	if err := c.validateDuration(field+"/tests/timeout", tests.Timeout); err != nil {
		return err
	}
	for i, path := range tests.Files {
		if !localProjectPath(path) {
			return c.problem(fmt.Sprintf("%s/tests/files/%d", field, i), "test patterns must be project-relative paths")
		}
	}
	return nil
}

func (c *Config) validateProjectReferences(field string, e Environment) error {
	for i, path := range e.PolicyFiles {
		if !localProjectPath(path) {
			return c.problem(fmt.Sprintf("%s/policy_files/%d", field, i), "policy paths must be project-relative")
		}
	}
	if e.Registry.URL != "" && !safePublicURL(e.Registry.URL) {
		return c.problem(field+"/registry/url", "registry must be a credential-free HTTPS reference")
	}
	if e.Registry.Lock != "" && !localProjectPath(e.Registry.Lock) {
		return c.problem(field+"/registry/lock", "registry lock must be a project-relative path")
	}
	if e.Reporting.JUnit != "" && !localProjectPath(e.Reporting.JUnit) {
		return c.problem(field+"/reporting/junit", "report must use a project-relative path")
	}
	return nil
}

func safePublicURL(value string) bool {
	u, err := url.Parse(value)
	return err == nil && u.Scheme == "https" && u.Hostname() != "" && u.User == nil && u.RawQuery == "" && u.Fragment == ""
}

func (c *Config) validateDuration(field, value string) error {
	if value == "" {
		return nil
	}
	duration, err := time.ParseDuration(value)
	if err != nil || duration <= 0 {
		return c.problem(field, "duration must be positive")
	}
	return nil
}

func (c *Config) validateFactory(field string, f DevelopmentFactory) error {
	if f.PostgresVersion < 14 || f.PostgresVersion > 18 {
		return c.problem(field+"/postgres_version", "PostgreSQL major must be between 14 and 18")
	}
	if f.Isolation != "" && f.Isolation != "database" && f.Isolation != "schema" {
		return c.problem(field+"/isolation", "isolation must be database or schema")
	}
	if len(f.Allowlist) == 0 {
		return c.problem(field+"/allowlist", "an explicit development allowlist is required")
	}
	if err := c.validateFactoryMode(field, f); err != nil {
		return err
	}
	durations := map[string]string{"startup_timeout": f.StartupTimeout, "case_timeout": f.CaseTimeout, "cleanup_timeout": f.CleanupTimeout}
	for _, key := range sortedKeys(durations) {
		if err := c.validateDuration(field+"/"+key, durations[key]); err != nil {
			return err
		}
	}
	return c.validateIdentityList(field+"/protected_targets", f.ProtectedTargets, safeDatabaseIdentity, "protected target must be a credential-free PostgreSQL identity")
}

func (c *Config) validateIdentityList(field string, identities []string, valid func(string) bool, message string) error {
	for i, identity := range identities {
		if !valid(identity) {
			return c.problem(fmt.Sprintf("%s/%d", field, i), message)
		}
	}
	return nil
}

func (c *Config) validateFactoryMode(field string, f DevelopmentFactory) error {
	switch f.Mode {
	case "container":
		return c.validateContainerFactory(field, f)
	case "existing_admin":
		return c.validateAdminFactory(field, f)
	default:
		return c.problem(field+"/mode", "factory mode must be container or existing_admin")
	}
}

func (c *Config) validateContainerFactory(field string, f DevelopmentFactory) error {
	if !safeDaemon(f.Endpoint) || !imageDigest.MatchString(f.Image) {
		return c.problem(field, "container factory requires an explicit daemon and digest-pinned image")
	}
	if f.Admin != "" || f.AdminClassification != "" {
		return c.problem(field+"/admin", "container factory cannot supply an administration credential")
	}
	return c.validateIdentityList(field+"/allowlist", f.Allowlist, safeDaemon, "container allowlist must name explicit daemon endpoints")
}

func (c *Config) validateAdminFactory(field string, f DevelopmentFactory) error {
	if f.Admin.Validate() != nil {
		return c.problem(field+"/admin", "administration credential must be a secret reference")
	}
	if f.AdminClassification != "development" {
		return c.problem(field+"/admin_classification", "administration reference must be explicitly classified development")
	}
	if f.Image != "" || f.Endpoint != "" {
		return c.problem(field, "existing_admin cannot select a container image or daemon")
	}
	return c.validateIdentityList(field+"/allowlist", f.Allowlist, safeDatabaseIdentity, "allowlist must name credential-free PostgreSQL identities")
}

func safeDaemon(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	switch u.Scheme {
	case "unix":
		return u.Host == "" && filepath.IsAbs(u.Path)
	case "tcp":
		return u.Hostname() != "" && u.Port() != "" && u.Path == ""
	default:
		return false
	}
}
func safeDatabaseIdentity(value string) bool {
	u, err := url.Parse(value)
	return err == nil && (u.Scheme == "postgres" || u.Scheme == "postgresql") && u.User == nil && u.Hostname() != "" && u.Path != "" && u.Path != "/" && u.RawQuery == "" && u.Fragment == ""
}

// EffectiveEnvironment returns values and origin metadata only; target and
// administration credentials are still opaque references.
type EffectiveEnvironment struct {
	DevelopmentFactory *DevelopmentFactory    `json:"development_factory,omitempty"`
	Name               string                 `json:"name"`
	Values             Environment            `json:"values"`
	Provenance         map[string]FieldOrigin `json:"provenance"`
}

func (c *Config) Effective() EffectiveEnvironment {
	origins := map[string]FieldOrigin{}
	prefix := environmentPath(c.Environment)
	for field, at := range c.locations {
		if strings.HasPrefix(field, prefix+"/") {
			r := at
			origins[strings.TrimPrefix(field, prefix)] = FieldOrigin{Kind: "file", Range: &r}
		}
	}
	if at, ok := c.locations["/environment"]; ok {
		origins["/environment"] = FieldOrigin{Kind: "file", Range: &at}
	}
	for field, kind := range c.origins {
		for existing := range origins {
			if strings.HasPrefix(existing, field+"/") {
				delete(origins, existing)
			}
		}
		origins[field] = FieldOrigin{Kind: kind}
	}
	values := cloneEnvironment(c.Environments[c.Environment])
	var factory *DevelopmentFactory
	if c.Version == CurrentVersion {
		var defaults []string
		values, defaults = normalizedEnvironment(values)
		for _, field := range defaults {
			origin, exists := origins[field]
			if !exists {
				at := c.location(prefix + field)
				origin = FieldOrigin{Kind: "file", Range: &at}
			}
			origin.Default = true
			origins[field] = origin
		}
		factory = c.effectiveFactory(origins, values.DevFactory)
	}
	return EffectiveEnvironment{Name: c.Environment, Values: values, Provenance: origins, DevelopmentFactory: factory}
}

func cloneEnvironment(e Environment) Environment {
	e.SchemaSources = slices.Clone(e.SchemaSources)
	e.Include = slices.Clone(e.Include)
	e.Exclude = slices.Clone(e.Exclude)
	e.Variables = cloneMap(e.Variables)
	e.Tests.Files = slices.Clone(e.Tests.Files)
	e.PolicyFiles = slices.Clone(e.PolicyFiles)
	return e
}
