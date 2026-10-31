package cli

import (
	"context"
	"errors"
	"path/filepath"
	"strings"

	"autosql/pkg/config"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/secret"
	"autosql/pkg/source"
)

type projectPlanRequest struct {
	Path, Environment, From, To string
	Filter                      LoadRequest
	Render                      map[string]string
	Max                         optionalInt
}

func projectPlanConfig(request projectPlanRequest) (*config.Config, error) {
	if len(request.Filter.Schemas) != 0 {
		return nil, usageError(errors.New("project planning uses --include/--exclude; --schema requires two direct live sources"))
	}
	var sources []string
	if request.To != "" {
		sources = []string{request.To}
	}
	overrides, err := projectSourceOverrides(sources)
	if err != nil {
		return nil, err
	}
	return loadProjectConfig(request.Path, request.Environment, config.Overrides{SourceInputs: overrides, Include: request.Filter.Include, Exclude: request.Filter.Exclude})
}

func runProjectPlan(parent context.Context, request projectPlanRequest, o output, reader ReadPlanService) error {
	c, err := projectPlanConfig(request)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(parent, projectTimeout(c))
	defer cancel()
	current, desired, err := projectPlanSources(ctx, c, request, reader)
	if err != nil {
		return err
	}
	current, target, err := selectProjectPlanSources(ctx, c, current, desired.Document)
	if err != nil {
		return projectSourceError(c, 0, err)
	}
	p, err := reader.Plan(ctx, current, target, plan.Options{Render: request.Render})
	if err != nil {
		return &Error{Kind: "migration", Message: "project planning failed", Code: ExitMigration, Cause: err}
	}
	if request.Max.set && len(p.Changes.Changes) > request.Max.value {
		return &Error{Kind: "validation", Message: "maximum change count exceeded", Code: ExitValidation}
	}
	data := map[string]any{"status": "planned", "plan": p, "environment": c.Environment}
	if len(p.Changes.Changes) == 0 {
		data["status"] = "no_op"
	}
	if desired.Identity != nil {
		data["identity"], data["source_inputs"] = desired.Identity, desired.Inputs
	}
	raw, err := p.MarshalCanonical()
	if err != nil {
		return err
	}
	return o.success(data, string(raw))
}

func projectPlanSources(ctx context.Context, c *config.Config, request projectPlanRequest, reader ReadPlanService) (schema.Document, projectSources, error) {
	var files *config.InputFiles
	if c.Version == config.CurrentVersion {
		var err error
		files, err = c.OpenInputFiles(ctx)
		if err != nil {
			return schema.Document{}, projectSources{}, projectSourceError(c, 0, err)
		}
		defer files.Close()
	}
	// Validate desired local inputs before any optional live inspection.
	desired, err := loadProjectPlanDesired(ctx, c, request.Path, files)
	if err != nil {
		return schema.Document{}, desired, err
	}
	current, err := loadProjectPlanCurrent(ctx, c, request, files, reader)
	if files != nil {
		desired.Inputs = files.Records()
	}
	return current, desired, err
}

func loadProjectPlanDesired(ctx context.Context, c *config.Config, path string, files *config.InputFiles) (projectSources, error) {
	if files == nil {
		return loadLegacyProjectSources(ctx, c, path)
	}
	return loadCapturedProjectSources(ctx, c, files)
}

func loadProjectPlanCurrent(ctx context.Context, c *config.Config, request projectPlanRequest, files *config.InputFiles, reader ReadPlanService) (schema.Document, error) {
	from := request.From
	if from == "" {
		from = "live:" + string(c.Effective().Values.Target)
	}
	if strings.HasPrefix(from, "live:") {
		ref := secret.Reference(strings.TrimPrefix(from, "live:"))
		if err := ref.Validate(); err != nil {
			return schema.Document{}, usageError(errors.New("live project source requires an env:// or file:// secret reference"))
		}
		doc, err := reader.Load(ctx, LoadRequest{Spec: from, BaseDir: filepath.Dir(request.Path)})
		if err != nil {
			return schema.Document{}, &Error{Kind: "connection", Message: "inspect current project target failed", Code: ExitConnection, Cause: err}
		}
		return doc, nil
	}
	sources, err := projectSourceOverrides([]string{from})
	if err != nil {
		return schema.Document{}, err
	}
	doc, err := loadProjectPlanLocalCurrent(ctx, c, request.Path, sources[0], files, reader)
	if err != nil {
		return schema.Document{}, &Error{Kind: "validation", Message: "load current project source failed", Code: ExitValidation, Cause: err}
	}
	return doc, nil
}

func loadProjectPlanLocalCurrent(ctx context.Context, c *config.Config, path string, input config.SchemaSource, files *config.InputFiles, reader ReadPlanService) (schema.Document, error) {
	if files == nil {
		name := input.Path
		if !filepath.IsAbs(name) {
			name = filepath.Join(filepath.Dir(path), name)
		}
		return reader.Load(ctx, LoadRequest{Spec: input.Kind + ":" + name})
	}
	loaded, err := loadCapturedProjectSource(ctx, files, input, c.Effective().Values.Variables)
	if err != nil {
		return schema.Document{}, err
	}
	return source.LoadContext(ctx, loaded)
}

func selectProjectPlanSources(ctx context.Context, c *config.Config, current, desired schema.Document) (schema.Document, schema.Document, error) {
	e := c.Effective().Values
	docs := []schema.Document{current, desired}
	for i, doc := range docs {
		normalized, err := postgres.New().Normalize(ctx, doc)
		if err != nil {
			return schema.Document{}, schema.Document{}, err
		}
		docs[i], err = schema.Select(normalized, e.Include, e.Exclude)
		if err != nil {
			return schema.Document{}, schema.Document{}, err
		}
	}
	return docs[0], docs[1], nil
}
