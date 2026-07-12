package cli

import (
	"context"
	"errors"
	"os"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/config"
	"autosql/pkg/executor"
	"autosql/pkg/migrate"
	migrateapply "autosql/pkg/migrate/apply"
	"autosql/pkg/migrate/revision"
	"autosql/pkg/secret"
)

func runMigrateApply(ctx context.Context, args []string, o output, services Services, baseline bool, redactor *secret.Redactor) error {
	fs := newFlags("migrate apply", o.streams.Err)
	configPath := fs.String("config", "", "configuration")
	environment := fs.String("env", "", "environment")
	urlRef := fs.String("url", "", "database secret reference")
	dir := fs.String("migration-dir", "", "migration directory")
	schema := fs.String("revision-schema", "", "revision schema")
	to := fs.String("to", "", "inclusive target version")
	from := fs.String("from", "", "first pending version")
	count := fs.Int("count", -1, "maximum files")
	dry := fs.Bool("dry-run", false, "select without mutation")
	transaction := fs.String("transaction", "file", "file or all")
	operator := fs.String("operator", "", "trusted operator identity")
	mode := fs.String("approval-mode", "artifact", "artifact (signed release only)")
	timeout := fs.Duration("timeout", 5*time.Minute, "operation timeout")
	jsonFlag := fs.Bool("json", false, "JSON")
	if e := fs.Parse(args); e != nil {
		return usageError(e)
	}
	if fs.NArg() != 0 || *operator == "" || *count < -1 || (*transaction != "file" && *transaction != "all") || *mode != "artifact" || *timeout <= 0 {
		return usageError(errors.New("invalid migrate apply arguments"))
	}
	if *configPath == "" && (*urlRef == "" || *dir == "") {
		return usageError(errors.New("--config or --url plus --migration-dir required"))
	}
	o.json = *jsonFlag
	resolver := secret.NewResolver()
	resolver.Redactor = redactor
	url := ""
	if *configPath != "" {
		loaded, e := config.Load(*configPath, os.LookupEnv, config.Overrides{Environment: *environment, Target: *urlRef, MigrationDir: *dir, RevisionSchema: *schema})
		if e != nil {
			return &Error{Kind: "config", Message: "load migration configuration failed", Code: ExitConfig, Cause: e}
		}
		rt, e := loaded.Preflight(ctx, resolver)
		if e != nil {
			return &Error{Kind: "validation", Message: "migration preflight failed", Code: ExitValidation, Cause: e}
		}
		url, *dir, *schema = rt.Target, rt.MigrationDir, rt.RevisionSchema
	} else {
		ref := secret.Reference(*urlRef)
		if e := ref.Validate(); e != nil {
			return &Error{Kind: "secret", Message: "invalid migration database reference", Code: ExitSecret}
		}
		var e error
		url, e = resolver.Resolve(ctx, ref)
		if e != nil {
			return &Error{Kind: "secret", Message: "resolve migration database failed", Code: ExitSecret, Cause: e}
		}
	}
	if *schema == "" {
		*schema = "autosql_revision"
	}
	snapshot, e := migrate.LoadSnapshot(*dir)
	if e != nil {
		return &Error{Kind: "validation", Message: "verify migration snapshot failed", Code: ExitValidation, Cause: e}
	}
	store, e := revision.Open(revision.Config{URL: url, Schema: *schema})
	if e != nil {
		return &Error{Kind: "config", Message: "revision configuration invalid", Code: ExitConfig, Cause: e}
	}
	if !*dry {
		if e = store.Init(ctx); e != nil {
			return &Error{Kind: "migration", Message: "initialize revision store failed", Code: ExitMigration, Cause: e}
		}
	}
	var max *int
	if *count >= 0 {
		max = count
	}
	verifier, ok := services.Apply.(interface {
		VerifyArtifact(artifact.Artifact) (artifact.VerifiedArtifact, error)
		ApplyVersioned(context.Context, artifact.VerifiedArtifact, executor.Session, executor.Tx) (executor.ExternalExecution, error)
		DrainLifecycle(context.Context, executor.LifecycleEvent) error
	})
	if !ok {
		return &Error{Kind: "config", Message: "trusted migration artifact verifier is not configured", Code: ExitConfig}
	}
	engine := migrateapply.Engine{Store: store, Verify: verifier.VerifyArtifact, Apply: verifier.ApplyVersioned, Drain: verifier.DrainLifecycle}
	callCtx, cancel := context.WithTimeout(ctx, *timeout)
	defer cancel()
	result, e := engine.Run(callCtx, migrateapply.Request{Directory: *dir, Snapshot: snapshot, From: *from, To: *to, Count: max, DryRun: *dry, Baseline: baseline, Transaction: *transaction, Operator: *operator, TargetIdentity: "revision/" + *schema})
	if e != nil {
		return &Error{Kind: "migration", Message: "versioned migration operation failed", Code: ExitMigration, Cause: e, Status: result.Status, RecoveryGuidance: func() string {
			if result.Failure != nil {
				return result.Failure.Recovery
			}
			return ""
		}()}
	}
	return o.success(result, result.Status+" through "+result.FinalVersion)
}
