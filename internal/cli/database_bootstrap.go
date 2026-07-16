package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/secret"
	"autosql/pkg/source"
)

var planDatabaseBootstrap = postgres.PlanDatabaseBootstrap
var executeDatabaseBootstrapURL = postgres.ExecuteDatabaseBootstrapURL

func runDatabaseBootstrap(parent context.Context, args []string, output output, redactor *secret.Redactor) error {
	flags := newFlags("database bootstrap", output.streams.Err)
	file := flags.String("file", "", "HCL file containing one database block and the complete desired graph")
	maintenanceRef := flags.String("maintenance-url", "", "maintenance database URL secret reference")
	postgresVersion := flags.String("postgres-version", "", "target PostgreSQL major version")
	extensionAllowlist := flags.String("extension-allowlist", "", "comma-separated reviewed extension names")
	concurrentIndexes := flags.Bool("concurrent-indexes", true, "create standalone indexes concurrently")
	timeout := flags.Duration("timeout", 10*time.Minute, "maximum command duration")
	jsonMode := flags.Bool("json", false, "emit JSON envelope")
	var routineDigests stringList
	flags.Var(&routineDigests, "reviewed-routine-digest", "reviewed routine body digest (repeatable)")
	if err := flags.Parse(args); err != nil {
		return usageError(err)
	}
	if flags.NArg() != 0 || *file == "" || *maintenanceRef == "" || *timeout <= 0 {
		return usageError(errors.New("--file, --maintenance-url, and a positive --timeout are required"))
	}
	output.json = *jsonMode
	raw, err := os.ReadFile(*file)
	if err != nil {
		return &Error{Kind: "config", Message: "read bootstrap HCL", Code: ExitConfig, Cause: err}
	}
	desired, err := source.LoadContext(parent, source.Input{URI: *file, Format: source.FormatHCLSource, Data: raw})
	if err != nil {
		return &Error{Kind: "config", Message: "load bootstrap HCL", Code: ExitConfig, Cause: err}
	}
	var databaseResources []schema.Resource
	for _, resource := range desired.Graph.Resources {
		if resource.Kind == schema.KindDatabase {
			databaseResources = append(databaseResources, resource)
		}
	}
	if len(databaseResources) != 1 {
		return &Error{Kind: "config", Message: "bootstrap HCL must contain exactly one database block", Code: ExitConfig}
	}
	target, err := postgres.DatabaseTargetFromResource(databaseResources[0])
	if err != nil {
		return &Error{Kind: "validation", Message: err.Error(), Code: ExitValidation, Cause: err}
	}
	reference := secret.Reference(*maintenanceRef)
	if err := reference.Validate(); err != nil {
		return &Error{Kind: "secret", Message: "--maintenance-url must be an env:// or file:// secret reference", Code: ExitSecret, Cause: err}
	}
	resolver := secret.NewResolver()
	resolver.Redactor = redactor
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	maintenanceURL, err := resolver.Resolve(ctx, reference)
	if err != nil {
		return &Error{Kind: "secret", Message: redactor.String(err.Error()), Code: ExitSecret, Cause: err}
	}
	render := map[string]string{
		"postgres_version":         strings.TrimSpace(*postgresVersion),
		"extension_allowlist":      strings.TrimSpace(*extensionAllowlist),
		"reviewed_routine_digests": strings.Join(routineDigests.value(), ","),
	}
	if *concurrentIndexes {
		render["concurrent_indexes"] = "true"
	}
	whole, err := planDatabaseBootstrap(ctx, target, desired, plan.Options{Render: render})
	if err != nil {
		return &Error{Kind: "validation", Message: redactor.String(err.Error()), Code: ExitValidation, Cause: err}
	}
	result, err := executeDatabaseBootstrapURL(ctx, maintenanceURL, whole, postgres.BootstrapExecutionHooks{})
	if err != nil {
		return &Error{Kind: "database", Message: redactor.String(err.Error()), Code: ExitConnection, Cause: err}
	}
	safe := struct {
		Status         string `json:"status"`
		PlanDigest     string `json:"plan_digest"`
		Database       string `json:"database"`
		Created        bool   `json:"created"`
		Resumed        bool   `json:"resumed"`
		AppliedSteps   int    `json:"applied_steps"`
		LastCheckpoint string `json:"last_checkpoint,omitempty"`
		LastConfirmed  string `json:"last_confirmed_step,omitempty"`
	}{"completed", result.PlanDigest, target.Name, result.CreatedDatabase, result.Resumed, result.AppliedSteps, result.LastCheckpoint, result.LastConfirmed}
	return output.success(safe, "database "+target.Name+" bootstrapped")
}
