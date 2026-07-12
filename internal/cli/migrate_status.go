package cli

import (
	"context"
	"errors"
	"fmt"
	"os"

	"autosql/pkg/config"
	"autosql/pkg/migrate"
	"autosql/pkg/migrate/revision"
	"autosql/pkg/secret"
)

func runMigrateStatus(ctx context.Context, args []string, o output, redactor *secret.Redactor) error {
	fs := newFlags("migrate status", o.streams.Err)
	urlRef := fs.String("url", "", "database URL secret reference")
	dir := fs.String("migration-dir", "", "verified migration directory")
	schema := fs.String("revision-schema", "", "revision schema")
	configPath := fs.String("config", "", "autosql configuration file")
	environment := fs.String("env", "", "named environment")
	jsonFlag := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 || (*configPath == "" && (*urlRef == "" || *dir == "")) {
		return usageError(errors.New("--config or both --url and --migration-dir are required"))
	}
	o.json = *jsonFlag
	resolver := secret.NewResolver()
	resolver.Redactor = redactor
	url := ""
	if *configPath != "" {
		loaded, err := config.Load(*configPath, os.LookupEnv, config.Overrides{Environment: *environment, Target: *urlRef, MigrationDir: *dir, RevisionSchema: *schema})
		if err != nil {
			return &Error{Kind: "config", Message: "load migration status configuration failed", Code: ExitConfig, Cause: err}
		}
		runtimeConfig, err := loaded.Preflight(ctx, resolver)
		if err != nil {
			return &Error{Kind: "validation", Message: "migration status preflight failed", Code: ExitValidation, Cause: err}
		}
		url, *dir, *schema = runtimeConfig.Target, runtimeConfig.MigrationDir, runtimeConfig.RevisionSchema
	} else {
		ref := secret.Reference(*urlRef)
		if err := ref.Validate(); err != nil {
			return &Error{Kind: "secret", Message: "invalid revision database reference", Code: ExitSecret}
		}
		var err error
		url, err = resolver.Resolve(ctx, ref)
		if err != nil {
			return &Error{Kind: "secret", Message: "resolve revision database failed", Code: ExitSecret, Cause: err}
		}
	}
	if *schema == "" {
		*schema = "autosql_revision"
	}
	manifest, err := migrate.Verify(*dir)
	if err != nil {
		return &Error{Kind: "validation", Message: "migration directory verification failed", Code: ExitValidation, Cause: err}
	}
	store, err := revision.Open(revision.Config{URL: url, Schema: *schema})
	if err != nil {
		return &Error{Kind: "config", Message: "revision store configuration invalid", Code: ExitConfig, Cause: err}
	}
	status, err := store.Status(ctx, manifest)
	if err != nil {
		return &Error{Kind: "migration", Message: "read migration status failed", Code: ExitMigration, Cause: err}
	}
	human := fmt.Sprintf("manifest %s: %d migrations", status.ManifestDigest, len(status.Entries))
	if status.Dirty || status.Drift {
		human += ", attention required"
	}
	return o.success(status, human)
}
