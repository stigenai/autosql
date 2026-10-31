package cli

import (
	"context"
	"os"
	"path/filepath"

	"autosql/pkg/config"
	"autosql/pkg/policy"
	"autosql/pkg/schema"
)

type generationInputs struct {
	Directory    string
	Desired      schema.Document
	Policy       policy.Document
	Project      *capturedProjectRelease
	Verification *projectReleaseVerification
}

func (i generationInputs) close() {
	if i.Project != nil {
		i.Project.close()
	}
}

func loadGenerationInputs(ctx context.Context, args trustedGenerateArgs, trusted generationConfig, reader ReadPlanService) (generationInputs, error) {
	if args.project != "" {
		return loadProjectGenerationInputs(ctx, args, trusted)
	}
	raw, err := os.ReadFile(trusted.PolicyFile)
	if err != nil {
		return generationInputs{}, projectReleaseFailure("read generation policy failed", nil)
	}
	document, err := policy.Parse(raw)
	if err != nil {
		return generationInputs{}, projectReleaseFailure("parse generation policy failed", nil)
	}
	desired, err := reader.Load(ctx, LoadRequest{Spec: args.to})
	if err != nil {
		return generationInputs{}, &Error{Kind: "validation", Message: "load desired schema failed", Code: ExitValidation}
	}
	return generationInputs{Directory: args.dir, Desired: desired, Policy: *document}, nil
}

func loadProjectGenerationInputs(ctx context.Context, args trustedGenerateArgs, trusted generationConfig) (generationInputs, error) {
	var sources []string
	if args.to != "" {
		sources = []string{args.to}
	}
	overrides, err := projectSourceOverrides(sources)
	if err != nil {
		return generationInputs{}, err
	}
	settings := projectReleaseVerification{Path: args.project, Overrides: config.Overrides{Environment: args.environment, SourceInputs: overrides, MigrationDir: args.dir}, Execution: projectReleaseExecution{Version: args.version, Label: args.label, RenameHints: args.hints, PolicyFile: trusted.PolicyFile, PolicyIdentity: trusted.PolicyIdentity, PostgresVersion: trusted.PostgresVersion}}
	captured, err := captureProjectRelease(ctx, settings)
	if err != nil {
		return generationInputs{}, err
	}
	environment := captured.Config.Effective()
	if environment.Name != trusted.Environment || environment.Values.Target != trusted.ProductionURL {
		captured.close()
		return generationInputs{}, projectReleaseFailure("project environment and target must match trusted generation configuration", nil)
	}
	if environment.Values.DevDatabase != "" && environment.Values.DevDatabase != trusted.DevelopmentURL {
		captured.close()
		return generationInputs{}, projectReleaseFailure("project development reference must match trusted generation configuration", nil)
	}
	count := captured.Manifest.Migrations.Count
	settings.MigrationPrefix = &count
	return generationInputs{Directory: filepath.Join(captured.Root, environment.Values.MigrationDir), Desired: captured.Desired, Policy: captured.Policy, Project: &captured, Verification: &settings}, nil
}
