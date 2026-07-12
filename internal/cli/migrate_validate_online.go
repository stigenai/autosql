package cli

import (
	"fmt"
	"os"

	"autosql/pkg/zerodowntime"
)

type onlineValidation struct {
	Version         string `json:"version"`
	Name            string `json:"name"`
	Digest          string `json:"digest"`
	MinimumPostgres int    `json:"minimum_postgres"`
	Operations      int    `json:"operations"`
}

// runMigrateValidateOnline is deliberately target-free. It accepts no URL or
// environment and finishes all parsing and semantic validation offline.
func runMigrateValidateOnline(args []string, o output) error {
	fs := newFlags("migrate validate-online", o.streams.Err)
	path := fs.String("file", "", "zero-downtime migration artifact")
	format := fs.String("format", "json", "json or yaml")
	jsonFlag := fs.Bool("json", false, "emit JSON envelope")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 || *path == "" {
		return usageError(fmt.Errorf("--file is required"))
	}
	o.json = *jsonFlag
	b, err := os.ReadFile(*path)
	if err != nil {
		return &Error{Kind: "config", Message: "cannot read migration artifact", Code: ExitConfig, Cause: err}
	}
	var migration zerodowntime.Migration
	switch *format {
	case "json":
		migration, err = zerodowntime.ParseJSON(b)
	case "yaml":
		migration, err = zerodowntime.ParseYAML(b)
	default:
		return usageError(fmt.Errorf("--format must be json or yaml"))
	}
	if err != nil {
		return &Error{Kind: "validation", Message: err.Error(), Code: ExitValidation, Cause: err}
	}
	result := onlineValidation{migration.Version, migration.Name, migration.Digest, migration.Requirements.MinimumPostgres, len(migration.Operations)}
	return o.success(result, fmt.Sprintf("valid %s %s (%d operations, PostgreSQL %d+)", result.Version, result.Digest, result.Operations, result.MinimumPostgres))
}
