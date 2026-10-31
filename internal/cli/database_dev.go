package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"autosql/pkg/config"
	"autosql/pkg/dbtest"
	"autosql/pkg/secret"
)

func runDatabaseDev(ctx context.Context, args []string, o output) error {
	if len(args) == 0 {
		return usageError(errors.New("database dev requires create or cleanup"))
	}
	switch args[0] {
	case "create":
		return runDatabaseDevCreate(ctx, args[1:], o)
	case "cleanup":
		return runDatabaseDevCleanup(ctx, args[1:], o)
	default:
		return usageError(errors.New("database dev requires create or cleanup"))
	}
}

type devCreateArgs struct {
	project, environment, leaseDirectory string
	major                                int
}

func parseDevCreateArgs(args []string, o output) (devCreateArgs, error) {
	var result devCreateArgs
	flags := newFlags("database dev create", o.streams.Err)
	flags.StringVar(&result.project, "config", "", "version-2 project JSON or HCL path")
	flags.StringVar(&result.environment, "env", "", "named development environment")
	flags.StringVar(&result.leaseDirectory, "lease-dir", "", "private runtime lease directory (default: user cache/autosql/development)")
	flags.IntVar(&result.major, "postgres-version", 0, "required PostgreSQL major; must match the configured factory")
	flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return result, usageError(err)
	}
	if flags.NArg() != 0 || result.project == "" || result.major < 14 || result.major > 18 {
		return result, usageError(errors.New("database dev create requires --config and --postgres-version 14..18"))
	}
	return result, nil
}

func runDatabaseDevCreate(ctx context.Context, args []string, o output) error {
	options, err := parseDevCreateArgs(args, o)
	if err != nil {
		return err
	}
	factory, err := projectDevelopmentFactory(options, o.redactor)
	if err != nil {
		return err
	}
	target, err := factory.Create(ctx, "interactive")
	if err != nil {
		return devFactoryError(err)
	}
	info := target.Info()
	data := struct {
		dbtest.LeaseInfo
		CredentialReference secret.Reference `json:"credential_ref"`
	}{info, target.CredentialReference()}
	human := fmt.Sprintf("development database ready (PostgreSQL %d)\nlease: %s\ncredential reference: %s\ncleanup: %s", info.PostgresMajor, info.Path, target.CredentialReference(), devCleanupCommand(info.Path))
	if err := o.success(data, human); err != nil {
		cleanupErr := target.Close(context.WithoutCancel(ctx))
		result := &Error{Kind: "internal", Message: "write development database result failed", Code: ExitInternal, Cause: err}
		attachDevRecovery(result, info.Path, cleanupErr)
		return result
	}
	return target.Detach()
}

type ownedDevelopmentFactory interface {
	Create(context.Context, string) (*dbtest.DisposableTarget, error)
}

func projectDevelopmentFactory(args devCreateArgs, redactor *secret.Redactor) (ownedDevelopmentFactory, error) {
	c, err := loadProjectConfig(args.project, args.environment, config.Overrides{})
	if err != nil {
		return nil, err
	}
	effective := c.Effective()
	if c.Version != config.CurrentVersion || effective.Values.Classification != "development" || effective.DevelopmentFactory == nil {
		return nil, &Error{Kind: "config", Message: "database dev requires a version-2 development environment and named factory", Code: ExitConfig}
	}
	definition := *effective.DevelopmentFactory
	if definition.PostgresVersion != args.major {
		return nil, &Error{Kind: "config", Message: "database dev requires a factory with the requested PostgreSQL major", Code: ExitConfig}
	}
	directory, err := devLeaseDirectory(args.leaseDirectory)
	if err != nil {
		return nil, devFactoryError(err)
	}
	return configuredDevelopmentFactory(c, effective, args.project, directory, redactor)
}

func configuredDevelopmentFactory(c *config.Config, effective config.EffectiveEnvironment, project, directory string, redactor *secret.Redactor) (ownedDevelopmentFactory, error) {
	definition := *effective.DevelopmentFactory
	if definition.Mode == "container" {
		return dbtest.ContainerFactory{Name: effective.Values.DevFactory, Classification: effective.Values.Classification, Definition: definition, LeaseDirectory: directory}, nil
	}
	absolute, err := filepath.Abs(project)
	if err != nil {
		return nil, devFactoryError(dbtest.ErrFactoryConfig)
	}
	resolver := secret.NewResolver()
	resolver.BaseDir = filepath.Dir(absolute)
	resolver.Redactor = redactor
	return dbtest.AdminFactory{Name: effective.Values.DevFactory, Classification: effective.Values.Classification, Definition: definition, ProtectedReferences: projectProtectedReferences(c), Resolver: resolver, LeaseDirectory: directory}, nil
}

func projectProtectedReferences(c *config.Config) []secret.Reference {
	var refs []secret.Reference
	for _, environment := range c.Environments {
		if environment.Classification != "production" {
			continue
		}
		refs = append(refs, environment.Target)
		if environment.DevDatabase != "" {
			refs = append(refs, environment.DevDatabase)
		}
	}
	return refs
}

func devLeaseDirectory(path string) (string, error) {
	if path == "" {
		cache, err := os.UserCacheDir()
		if err != nil {
			return "", dbtest.ErrLease
		}
		path = filepath.Join(cache, "autosql", "development")
	}
	return dbtest.PrepareLeaseDirectory(path)
}

func runDatabaseDevCleanup(ctx context.Context, args []string, o output) error {
	flags := newFlags("database dev cleanup", o.streams.Err)
	path := flags.String("lease", "", "exact durable lease path")
	flags.Bool("json", false, "emit JSON")
	if err := flags.Parse(args); err != nil {
		return usageError(err)
	}
	if *path == "" || flags.NArg() != 0 {
		return usageError(errors.New("database dev cleanup requires --lease"))
	}
	resolver := secret.NewResolver()
	resolver.Redactor = o.redactor
	if err := dbtest.CleanupLease(ctx, *path, resolver); err != nil {
		return devFactoryError(err)
	}
	info, err := dbtest.InspectLease(*path)
	if err != nil {
		return devFactoryError(err)
	}
	return o.success(info, "development resources cleaned; lease: "+info.Path)
}

func devFactoryError(err error) *Error {
	primary := err
	var factoryError *dbtest.FactoryError
	if errors.As(err, &factoryError) {
		primary = factoryError.Unwrap()
	}
	kind, code := devFactoryErrorCode(primary)
	result := &Error{Kind: kind, Message: "development database operation refused", Code: code, Cause: err}
	if factoryError != nil {
		result.Message = factoryError.Error()
		attachDevRecovery(result, factoryError.LeasePath, err)
	}
	return result
}

func devFactoryErrorCode(err error) (string, ExitCode) {
	switch {
	case errors.Is(err, context.Canceled):
		return "canceled", ExitCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", ExitTimeout
	case errors.Is(err, dbtest.ErrFactoryConfig):
		return "config", ExitConfig
	case errors.Is(err, dbtest.ErrFactorySecret):
		return "secret", ExitSecret
	case errors.Is(err, dbtest.ErrFactoryConnection):
		return "connection", ExitConnection
	case errors.Is(err, dbtest.ErrOwnership), errors.Is(err, dbtest.ErrLease):
		return "conflict", ExitConflict
	default:
		return "migration", ExitMigration
	}
}

func attachDevRecovery(result *Error, path string, cause error) {
	if path == "" {
		return
	}
	info, err := dbtest.InspectLease(path)
	if err == nil {
		result.Data = info
	}
	if cause != nil {
		result.RecoveryGuidance = "inspect the recorded lease; retry " + devCleanupCommand(path)
	}
}

func devCleanupCommand(path string) string {
	return "autosql database dev cleanup --lease '" + strings.ReplaceAll(path, "'", "'\\''") + "'"
}
