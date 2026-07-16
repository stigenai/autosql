package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/secret"
	"autosql/pkg/source"
)

var prepareDatabaseURL = postgres.PrepareDatabaseURL

func runDatabasePrepare(parent context.Context, args []string, output output, redactor *secret.Redactor) error {
	flags := newFlags("database prepare", output.streams.Err)
	targetPath := flags.String("target-hcl", "", "HCL file containing exactly one database block")
	maintenanceRef := flags.String("maintenance-url", "", "maintenance database URL secret reference")
	timeout := flags.Duration("timeout", 30*time.Second, "maximum command duration")
	jsonMode := flags.Bool("json", false, "emit JSON envelope")
	if err := flags.Parse(args); err != nil {
		return usageError(err)
	}
	if flags.NArg() != 0 || *targetPath == "" || *maintenanceRef == "" || *timeout <= 0 {
		return usageError(errors.New("--target-hcl, --maintenance-url, and a positive --timeout are required"))
	}
	output.json = *jsonMode
	data, err := os.ReadFile(*targetPath)
	if err != nil {
		return &Error{Kind: "config", Message: "read database target HCL", Code: ExitConfig, Cause: err}
	}
	document, err := source.LoadContext(parent, source.Input{URI: *targetPath, Format: source.FormatHCLSource, Data: data})
	if err != nil {
		return &Error{Kind: "config", Message: "load database target HCL", Code: ExitConfig, Cause: err}
	}
	var databaseResources []schema.Resource
	for _, resource := range document.Graph.Resources {
		if resource.Kind == schema.KindDatabase {
			databaseResources = append(databaseResources, resource)
		}
	}
	if len(databaseResources) != 1 {
		return &Error{Kind: "config", Message: "target HCL must contain exactly one database block", Code: ExitConfig}
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
	prepared, err := prepareDatabaseURL(ctx, maintenanceURL, target)
	if err != nil {
		return &Error{Kind: "database", Message: redactor.String(err.Error()), Code: ExitConnection, Cause: err}
	}
	if prepared.Connection != nil {
		defer prepared.Connection.Close(context.Background())
	}
	result := struct {
		Status   string `json:"status"`
		Name     string `json:"name"`
		Mode     string `json:"mode"`
		Created  bool   `json:"created"`
		Endpoint string `json:"endpoint"`
	}{Status: "prepared", Name: target.Name, Mode: string(target.Mode), Created: prepared.Created, Endpoint: fmt.Sprintf("%s:%d", target.Endpoint.Host, target.Endpoint.Port)}
	return output.success(result, strings.TrimSpace(fmt.Sprintf("database %s prepared (%s)", target.Name, target.Mode)))
}
