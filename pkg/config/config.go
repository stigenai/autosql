// Package config loads and validates named AutoSQL environments.
package config

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"autosql/pkg/secret"
)

const CurrentVersion = 1

type Config struct {
	Version      int                    `json:"version"`
	Environment  string                 `json:"environment,omitempty"`
	Environments map[string]Environment `json:"environments"`
	path         string
}

type Environment struct {
	Target        secret.Reference  `json:"target"`
	DevDatabase   secret.Reference  `json:"dev_database,omitempty"`
	SchemaSources []SchemaSource    `json:"schema_sources"`
	MigrationDir  string            `json:"migration_dir"`
	Include       []string          `json:"include,omitempty"`
	Exclude       []string          `json:"exclude,omitempty"`
	Variables     map[string]string `json:"variables,omitempty"`
	Timeout       string            `json:"timeout,omitempty"`
}

type SchemaSource struct {
	Name string `json:"name,omitempty"`
	Kind string `json:"kind"`
	Path string `json:"path"`
}

// Overrides are applied after file and AUTOSQL_* environment values.
type Overrides struct {
	Environment, Target, DevDatabase, MigrationDir, Timeout string
	SchemaSources, Include, Exclude                         []string
}

type Runtime struct {
	Environment   string
	Target        string `json:"-"`
	DevDatabase   string `json:"-"`
	SchemaSources []SchemaSource
	MigrationDir  string
	Include       []string
	Exclude       []string
	Variables     map[string]string
	Timeout       time.Duration
}

func Load(path string, getenv func(string) (string, bool), cli Overrides) (*Config, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("open config: %w", err)
	}
	defer f.Close()
	c, err := Decode(f)
	if err != nil {
		return nil, err
	}
	c.path = path
	if getenv == nil {
		getenv = os.LookupEnv
	}
	// Select the environment first. Field values then retain the documented
	// file < environment < CLI precedence even when CLI selects a named env.
	if v, ok := getenv("AUTOSQL_ENV"); ok {
		c.Environment = v
	}
	if cli.Environment != "" {
		c.Environment = cli.Environment
	}
	applyEnv(c, getenv)
	applyCLI(c, cli)
	if err := c.Validate(); err != nil {
		return nil, err
	}
	return c, nil
}

func Decode(r io.Reader) (*Config, error) {
	dec := json.NewDecoder(io.LimitReader(r, 4<<20))
	dec.DisallowUnknownFields()
	var c Config
	if err := dec.Decode(&c); err != nil {
		return nil, fmt.Errorf("decode config: %w", err)
	}
	if err := ensureEOF(dec); err != nil {
		return nil, err
	}
	return &c, nil
}

func ensureEOF(dec *json.Decoder) error {
	var extra any
	if err := dec.Decode(&extra); !errors.Is(err, io.EOF) {
		if err == nil {
			return errors.New("decode config: multiple JSON values")
		}
		return err
	}
	return nil
}

func (c *Config) Validate() error {
	if c.Version != CurrentVersion {
		return fmt.Errorf("unsupported config version %d (expected %d)", c.Version, CurrentVersion)
	}
	if len(c.Environments) == 0 {
		return errors.New("config has no environments")
	}
	if c.Environment == "" {
		return errors.New("no environment selected")
	}
	e, ok := c.Environments[c.Environment]
	if !ok {
		return fmt.Errorf("environment %q is not defined", c.Environment)
	}
	if err := e.Target.Validate(); err != nil {
		return fmt.Errorf("environment %q target: %w", c.Environment, err)
	}
	if e.DevDatabase != "" {
		if err := e.DevDatabase.Validate(); err != nil {
			return fmt.Errorf("environment %q dev_database: %w", c.Environment, err)
		}
	}
	if len(e.SchemaSources) == 0 {
		return fmt.Errorf("environment %q has no schema sources", c.Environment)
	}
	for i, s := range e.SchemaSources {
		if s.Kind != "sql" && s.Kind != "json" {
			return fmt.Errorf("schema_sources[%d]: unsupported kind %q", i, s.Kind)
		}
		if strings.TrimSpace(s.Path) == "" {
			return fmt.Errorf("schema_sources[%d]: path is required", i)
		}
	}
	for _, f := range append(append([]string(nil), e.Include...), e.Exclude...) {
		if strings.TrimSpace(f) == "" {
			return errors.New("include and exclude filters must not be blank")
		}
	}
	if strings.TrimSpace(e.MigrationDir) == "" {
		return errors.New("migration_dir is required")
	}
	if e.Timeout != "" {
		d, err := time.ParseDuration(e.Timeout)
		if err != nil || d <= 0 {
			return fmt.Errorf("invalid timeout %q", e.Timeout)
		}
	}
	return nil
}

// Preflight validates local references and resolves secrets without opening a database connection.
func (c *Config) Preflight(ctx context.Context, resolver *secret.Resolver) (*Runtime, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	if resolver == nil {
		resolver = secret.NewResolver()
	}
	if resolver.BaseDir == "" && c.path != "" {
		resolver.BaseDir = filepath.Dir(c.path)
	}
	e := c.Environments[c.Environment]
	target, err := resolver.Resolve(ctx, e.Target)
	if err != nil {
		return nil, fmt.Errorf("resolve target: %w", err)
	}
	dev := ""
	if e.DevDatabase != "" {
		dev, err = resolver.Resolve(ctx, e.DevDatabase)
		if err != nil {
			return nil, fmt.Errorf("resolve dev_database: %w", err)
		}
	}
	base := resolver.BaseDir
	for i, s := range e.SchemaSources {
		path := s.Path
		if !filepath.IsAbs(path) {
			path = filepath.Join(base, path)
		}
		info, err := os.Stat(path)
		if err != nil {
			return nil, fmt.Errorf("schema_sources[%d]: %w", i, err)
		}
		if info.IsDir() {
			return nil, fmt.Errorf("schema_sources[%d]: path is a directory", i)
		}
	}
	migrationDir := e.MigrationDir
	if !filepath.IsAbs(migrationDir) {
		migrationDir = filepath.Join(base, migrationDir)
	}
	if info, err := os.Stat(migrationDir); err != nil || !info.IsDir() {
		if err == nil {
			err = errors.New("not a directory")
		}
		return nil, fmt.Errorf("migration_dir: %w", err)
	}
	d := 30 * time.Second
	if e.Timeout != "" {
		d, _ = time.ParseDuration(e.Timeout)
	}
	return &Runtime{Environment: c.Environment, Target: target, DevDatabase: dev, SchemaSources: append([]SchemaSource(nil), e.SchemaSources...), MigrationDir: migrationDir, Include: append([]string(nil), e.Include...), Exclude: append([]string(nil), e.Exclude...), Variables: cloneMap(e.Variables), Timeout: d}, nil
}

func applyEnv(c *Config, getenv func(string) (string, bool)) {
	e, ok := c.Environments[c.Environment]
	if !ok {
		return
	}
	if _, ok := getenv("AUTOSQL_TARGET"); ok {
		e.Target = "env://AUTOSQL_TARGET"
	}
	if _, ok := getenv("AUTOSQL_DEV_DATABASE"); ok {
		e.DevDatabase = "env://AUTOSQL_DEV_DATABASE"
	}
	if v, ok := getenv("AUTOSQL_SCHEMA_SOURCES"); ok {
		e.SchemaSources = sourceList(split(v))
	}
	if v, ok := getenv("AUTOSQL_MIGRATION_DIR"); ok {
		e.MigrationDir = v
	}
	if v, ok := getenv("AUTOSQL_INCLUDE"); ok {
		e.Include = split(v)
	}
	if v, ok := getenv("AUTOSQL_EXCLUDE"); ok {
		e.Exclude = split(v)
	}
	if v, ok := getenv("AUTOSQL_TIMEOUT"); ok {
		e.Timeout = v
	}
	c.Environments[c.Environment] = e
}

func applyCLI(c *Config, o Overrides) {
	if o.Environment != "" {
		c.Environment = o.Environment
	}
	e, ok := c.Environments[c.Environment]
	if !ok {
		return
	}
	if o.Target != "" {
		e.Target = secret.Reference(o.Target)
	}
	if o.DevDatabase != "" {
		e.DevDatabase = secret.Reference(o.DevDatabase)
	}
	if o.SchemaSources != nil {
		e.SchemaSources = sourceList(o.SchemaSources)
	}
	if o.MigrationDir != "" {
		e.MigrationDir = o.MigrationDir
	}
	if o.Include != nil {
		e.Include = append([]string(nil), o.Include...)
	}
	if o.Exclude != nil {
		e.Exclude = append([]string(nil), o.Exclude...)
	}
	if o.Timeout != "" {
		e.Timeout = o.Timeout
	}
	c.Environments[c.Environment] = e
}

func split(s string) []string {
	if s == "" {
		return []string{}
	}
	p := strings.Split(s, ",")
	for i := range p {
		p[i] = strings.TrimSpace(p[i])
	}
	return p
}
func sourceList(paths []string) []SchemaSource {
	out := make([]SchemaSource, len(paths))
	for i, p := range paths {
		kind := "sql"
		if strings.HasSuffix(strings.ToLower(p), ".json") {
			kind = "json"
		}
		out[i] = SchemaSource{Kind: kind, Path: p}
	}
	return out
}
func cloneMap(m map[string]string) map[string]string {
	if m == nil {
		return nil
	}
	c := make(map[string]string, len(m))
	for k, v := range m {
		c[k] = v
	}
	return c
}

func BoolEnv(getenv func(string) (string, bool), key string) bool {
	v, ok := getenv(key)
	b, _ := strconv.ParseBool(v)
	return ok && b
}
