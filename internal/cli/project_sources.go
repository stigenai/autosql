package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"autosql/pkg/config"
	"autosql/pkg/schema"
	"autosql/pkg/source"
)

type projectSources struct {
	Document schema.Document
	Identity *config.ProjectIdentity
	Inputs   []config.InputFile
}

func projectSourceOverrides(values []string) ([]config.SchemaSource, error) {
	if values == nil {
		return nil, nil
	}
	result := make([]config.SchemaSource, 0, len(values))
	for _, value := range values {
		kind, path, ok := strings.Cut(value, ":")
		if !ok || path == "" {
			return nil, usageError(errors.New("source must use sql:path, json:path or hcl:path"))
		}
		if kind == "native" {
			kind = "json"
		}
		if kind != "sql" && kind != "json" && kind != "hcl" {
			return nil, usageError(errors.New("project sources must be local SQL, JSON or HCL files"))
		}
		result = append(result, config.SchemaSource{Kind: kind, Path: path})
	}
	return result, nil
}

func loadProjectConfig(path, environment string, overrides config.Overrides) (*config.Config, error) {
	overrides.Environment = environment
	c, err := config.Load(path, os.LookupEnv, overrides)
	if err != nil {
		return nil, &Error{Kind: "config", Message: err.Error(), Code: ExitConfig, Cause: err}
	}
	return c, nil
}

func projectSourceError(c *config.Config, index int, cause error) error {
	if errors.Is(cause, context.Canceled) || errors.Is(cause, context.DeadlineExceeded) {
		return cause
	}
	name := strings.ReplaceAll(strings.ReplaceAll(c.Environment, "~", "~0"), "/", "~1")
	field := fmt.Sprintf("/environments/%s/schema_sources/%d", name, index)
	located := &config.LocatedError{Code: "invalid_source", Field: field, Range: c.FieldRange(field), Message: "configured schema source could not be loaded"}
	var conflict *source.ConflictError
	if errors.As(cause, &conflict) {
		return &Error{Kind: "conflict", Message: located.Error(), Code: ExitConflict, Cause: located}
	}
	return &Error{Kind: "validation", Message: located.Error(), Code: ExitValidation, Cause: located}
}

func loadProjectSources(ctx context.Context, c *config.Config, path string) (projectSources, error) {
	if c.Version == config.LegacyVersion {
		return loadLegacyProjectSources(ctx, c, path)
	}
	files, err := c.OpenInputFiles(ctx)
	if err != nil {
		return projectSources{}, &Error{Kind: "validation", Message: "open project inputs failed", Code: ExitValidation, Cause: err}
	}
	defer files.Close()
	return loadCapturedProjectSources(ctx, c, files)
}

func loadCapturedProjectSources(ctx context.Context, c *config.Config, files *config.InputFiles) (projectSources, error) {
	effective := c.Effective()
	inputs := make([]source.Input, 0, len(effective.Values.SchemaSources))
	for index, input := range effective.Values.SchemaSources {
		loaded, err := loadCapturedProjectSource(ctx, files, input, effective.Values.Variables)
		if err != nil {
			return projectSources{}, projectSourceError(c, index, err)
		}
		inputs = append(inputs, loaded)
	}
	doc, err := source.LoadContext(ctx, inputs...)
	if err != nil {
		return projectSources{}, projectSourceError(c, 0, err)
	}
	identity, err := c.Identity()
	if err != nil {
		return projectSources{}, err
	}
	return projectSources{Document: doc, Identity: &identity, Inputs: files.Records()}, nil
}

func loadCapturedProjectSource(ctx context.Context, files *config.InputFiles, input config.SchemaSource, variables map[string]string) (source.Input, error) {
	format := source.FormatSQL
	if input.Kind == "json" {
		format = source.FormatNative
	}
	if input.Kind != "hcl" {
		raw, err := files.ReadFile(input.Path)
		return source.Input{URI: input.Path, Format: format, Data: raw}, err
	}
	values := source.HCLVariables{}
	for key, value := range variables {
		values[key] = value
	}
	loader := source.HCLLoader{ReadFile: files.ReadFile, ReadDir: files.ReadDir, Variables: values}
	doc, err := loader.Load(ctx, filepath.Join(files.RootPath(), input.Path))
	if err != nil {
		return source.Input{}, err
	}
	// The loader resolves absolute paths internally. Public project output uses
	// portable relative source locations; file content digests bind the originals.
	for index := range doc.Graph.Resources {
		at := doc.Graph.Resources[index].Source
		if at != nil {
			relative, err := filepath.Rel(files.RootPath(), at.URI)
			if err != nil {
				return source.Input{}, err
			}
			at.URI = filepath.ToSlash(relative)
		}
	}
	raw, err := doc.MarshalCanonical()
	return source.Input{URI: input.Path, Format: source.FormatNative, Data: raw}, err
}

func loadLegacyProjectSources(ctx context.Context, c *config.Config, path string) (projectSources, error) {
	var inputs []source.Input
	for index, input := range c.Environments[c.Environment].SchemaSources {
		name := input.Path
		if !filepath.IsAbs(name) {
			name = filepath.Join(filepath.Dir(path), name)
		}
		raw, err := os.ReadFile(name)
		if err != nil {
			return projectSources{}, projectSourceError(c, index, err)
		}
		format := source.FormatSQL
		if input.Kind == "json" {
			format = source.FormatNative
		}
		inputs = append(inputs, source.Input{URI: name, Format: format, Data: raw})
	}
	doc, err := source.LoadContext(ctx, inputs...)
	if err != nil {
		return projectSources{}, projectSourceError(c, 0, err)
	}
	return projectSources{Document: doc}, nil
}

func runProjectSchemaLoad(parent context.Context, path, environment string, sources []string, timeout string, o output) error {
	overrides, err := projectSourceOverrides(sources)
	if err != nil {
		return err
	}
	c, err := loadProjectConfig(path, environment, config.Overrides{SourceInputs: overrides, Timeout: timeout})
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, projectTimeout(c))
	defer cancel()
	result, err := loadProjectSources(ctx, c, path)
	if err != nil {
		return err
	}
	raw, err := result.Document.MarshalCanonical()
	if err != nil {
		return err
	}
	data := map[string]any{"schema": result.Document, "environment": c.Environment}
	if result.Identity != nil {
		data["identity"] = result.Identity
		data["source_inputs"] = result.Inputs
	}
	return o.success(data, strings.TrimSuffix(string(raw), "\n"))
}

func projectTimeout(c *config.Config) time.Duration {
	value := c.Effective().Values.Timeout
	if value == "" {
		return 30 * time.Second
	}
	duration, _ := time.ParseDuration(value) // Config.Load has validated the value.
	return duration
}
