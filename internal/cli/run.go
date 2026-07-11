package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"runtime/debug"
	"strings"
	"time"

	"autosql/pkg/config"
	"autosql/pkg/postgres"
	"autosql/pkg/secret"
	"autosql/pkg/source"
)

var Version = "dev"

func Run(ctx context.Context, args []string, streams Streams) int {
	if streams.In == nil {
		streams.In = strings.NewReader("")
	}
	if streams.Out == nil {
		streams.Out = io.Discard
	}
	if streams.Err == nil {
		streams.Err = io.Discard
	}
	redactor := secret.NewRedactor()
	command := commandName(args)
	jsonMode := contains(args, "--json")
	o := output{streams: streams, json: jsonMode, command: command, redactor: redactor}
	var err error
	switch {
	case len(args) == 0:
		err = &Error{Kind: "usage", Message: usage(), Code: ExitUsage}
	case args[0] == "version":
		err = runVersion(args[1:], o)
	case len(args) >= 2 && args[0] == "config" && args[1] == "validate":
		err = runConfigValidate(ctx, args[2:], o, redactor)
	case len(args) >= 2 && args[0] == "schema" && args[1] == "load":
		err = runSchemaLoad(ctx, args[2:], o)
	case len(args) >= 2 && args[0] == "schema" && args[1] == "inspect":
		err = runSchemaInspect(ctx, args[2:], o, redactor)
	case args[0] == "help" || args[0] == "--help" || args[0] == "-h":
		if e := o.success(map[string]string{"usage": usage()}, usage()); e != nil {
			err = e
		}
	default:
		err = &Error{Kind: "usage", Message: fmt.Sprintf("unknown command %q\n%s", args[0], usage()), Code: ExitUsage}
	}
	if err == nil {
		return int(ExitOK)
	}
	e := classify(err, nil)
	o.failure(e)
	return int(e.Code)
}

var inspectPostgres = postgres.InspectURL

func runSchemaInspect(parent context.Context, args []string, o output, redactor *secret.Redactor) error {
	fs := newFlags("schema inspect", o.streams.Err)
	urlRef := fs.String("url", "", "database URL secret reference (env:// or file://)")
	timeout := fs.Duration("timeout", 30*time.Second, "maximum command duration")
	advanced := fs.Bool("advanced", false, "inspect roles and grants")
	jsonFlag := fs.Bool("json", false, "emit JSON envelope")
	var schemas, include, exclude stringList
	fs.Var(&schemas, "schema", "schema to inspect (repeatable)")
	fs.Var(&include, "include", "include pattern (repeatable)")
	fs.Var(&exclude, "exclude", "exclude pattern (repeatable)")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 {
		return usageError(fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if *urlRef == "" || *timeout <= 0 {
		return usageError(fmt.Errorf("--url and a positive --timeout are required"))
	}
	o.json = *jsonFlag
	ref := secret.Reference(*urlRef)
	if err := ref.Validate(); err != nil {
		return &Error{Kind: "secret", Message: "--url must be a valid env:// or file:// secret reference", Code: ExitSecret, Cause: err}
	}
	resolver := secret.NewResolver()
	resolver.Redactor = redactor
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	url, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return &Error{Kind: "secret", Message: redactor.String(err.Error()), Code: ExitSecret, Cause: err}
	}
	doc, err := inspectPostgres(ctx, url, postgres.Options{Schemas: schemas.value(), Include: include.value(), Exclude: exclude.value(), Advanced: *advanced})
	if err != nil {
		if errors.Is(err, postgres.ErrPermission) {
			return &Error{Kind: "permission", Message: redactor.String(err.Error()), Code: ExitValidation, Cause: err}
		}
		return &Error{Kind: "connection", Message: redactor.String(err.Error()), Code: ExitConnection, Cause: err}
	}
	human, err := doc.MarshalCanonical()
	if err != nil {
		return &Error{Kind: "validation", Message: err.Error(), Code: ExitValidation, Cause: err}
	}
	return o.success(doc, strings.TrimSuffix(string(human), "\n"))
}

func runSchemaLoad(parent context.Context, args []string, o output) error {
	fs := newFlags("schema load", o.streams.Err)
	timeout := fs.Duration("timeout", 30*time.Second, "maximum command duration")
	jsonFlag := fs.Bool("json", false, "emit JSON envelope")
	var sources stringList
	fs.Var(&sources, "source", "schema source as sql:path or json:path (repeatable)")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 {
		return usageError(fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if *timeout <= 0 || len(sources.values) == 0 {
		return usageError(fmt.Errorf("at least one --source and a positive --timeout are required"))
	}
	o.json = *jsonFlag
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	inputs := make([]source.Input, 0, len(sources.values))
	for _, value := range sources.values {
		if err := ctx.Err(); err != nil {
			return err
		}
		formatName, path, ok := strings.Cut(value, ":")
		if !ok || path == "" {
			return usageError(fmt.Errorf("invalid --source %q; expected sql:path or json:path", value))
		}
		var format source.Format
		switch formatName {
		case "sql":
			format = source.FormatSQL
		case "json":
			format = source.FormatNative
		default:
			return usageError(fmt.Errorf("invalid source format %q", formatName))
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return &Error{Kind: "config", Message: fmt.Sprintf("read schema source %s: %v", path, err), Code: ExitConfig, Cause: err}
		}
		inputs = append(inputs, source.Input{URI: path, Format: format, Data: data})
	}
	doc, err := source.LoadContext(ctx, inputs...)
	if err != nil {
		var conflict *source.ConflictError
		if errors.As(err, &conflict) {
			return &Error{Kind: "conflict", Message: conflict.Error(), Code: ExitConflict, Cause: err}
		}
		return &Error{Kind: "validation", Message: err.Error(), Code: ExitValidation, Cause: err}
	}
	human, err := doc.MarshalCanonical()
	if err != nil {
		return &Error{Kind: "validation", Message: err.Error(), Code: ExitValidation, Cause: err}
	}
	return o.success(doc, strings.TrimSuffix(string(human), "\n"))
}

func runVersion(args []string, o output) error {
	fs := newFlags("version", o.streams.Err)
	jsonFlag := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 {
		return usageError(fmt.Errorf("version accepts no arguments"))
	}
	o.json = *jsonFlag
	data := map[string]string{"version": Version}
	if info, ok := debug.ReadBuildInfo(); ok {
		data["go_version"] = info.GoVersion
	}
	return o.success(data, Version)
}

func runConfigValidate(parent context.Context, args []string, o output, redactor *secret.Redactor) error {
	fs := newFlags("config validate", o.streams.Err)
	path := fs.String("config", "autosql.json", "configuration file")
	environment := fs.String("env", "", "named environment")
	target := fs.String("target", "", "target secret reference")
	dev := fs.String("dev-database", "", "development database secret reference")
	migrations := fs.String("migration-dir", "", "migration directory")
	timeout := fs.Duration("timeout", 30*time.Second, "maximum command duration")
	preflight := fs.Bool("preflight", false, "resolve secrets and verify local paths")
	jsonFlag := fs.Bool("json", false, "emit JSON")
	var sources, include, exclude stringList
	fs.Var(&sources, "schema-source", "schema source path (repeatable)")
	fs.Var(&include, "include", "include filter (repeatable)")
	fs.Var(&exclude, "exclude", "exclude filter (repeatable)")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 {
		return usageError(fmt.Errorf("unexpected arguments: %s", strings.Join(fs.Args(), " ")))
	}
	if *timeout <= 0 {
		return usageError(fmt.Errorf("timeout must be positive"))
	}
	o.json = *jsonFlag
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	c, err := config.Load(*path, os.LookupEnv, config.Overrides{Environment: *environment, Target: *target, DevDatabase: *dev, MigrationDir: *migrations, SchemaSources: sources.value(), Include: include.value(), Exclude: exclude.value()})
	if err != nil {
		return &Error{Kind: "config", Message: err.Error(), Code: ExitConfig, Cause: err}
	}
	data := map[string]any{"valid": true, "environment": c.Environment, "preflight": *preflight}
	if *preflight {
		resolver := secret.NewResolver()
		resolver.Redactor = redactor
		runtimeConfig, err := c.Preflight(ctx, resolver)
		if err != nil {
			kind, code := "validation", ExitValidation
			if strings.Contains(err.Error(), "resolve ") {
				kind, code = "secret", ExitSecret
			}
			return &Error{Kind: kind, Message: redactor.String(err.Error()), Code: code, Cause: err}
		}
		data["schema_sources"] = len(runtimeConfig.SchemaSources)
		data["migration_dir"] = runtimeConfig.MigrationDir
	}
	return o.success(data, fmt.Sprintf("configuration is valid (environment %s)", c.Environment))
}

func newFlags(name string, _ io.Writer) *flag.FlagSet {
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	fs.Usage = func() {}
	return fs
}
func usageError(err error) *Error {
	return &Error{Kind: "usage", Message: err.Error(), Code: ExitUsage, Cause: err}
}
func usage() string {
	return "usage: autosql <command>\n\ncommands:\n  version [--json]\n  config validate [--config path] [--env name] [--preflight] [--json]\n  schema load --source sql:path|json:path [--source ...] [--json]\n  schema inspect --url env://NAME|file://path [--schema name] [--include pattern] [--exclude pattern] [--advanced] [--json]"
}
func contains(args []string, want string) bool {
	for _, a := range args {
		if a == want {
			return true
		}
	}
	return false
}

func commandName(args []string) string {
	if len(args) == 0 {
		return ""
	}
	if len(args) >= 2 && args[0] == "config" && args[1] == "validate" {
		return "config validate"
	}
	if len(args) >= 2 && args[0] == "schema" && args[1] == "load" {
		return "schema load"
	}
	if len(args) >= 2 && args[0] == "schema" && args[1] == "inspect" {
		return "schema inspect"
	}
	return args[0]
}

type stringList struct {
	values []string
	set    bool
}

func (s *stringList) String() string     { return strings.Join(s.values, ",") }
func (s *stringList) Set(v string) error { s.set = true; s.values = append(s.values, v); return nil }
func (s *stringList) value() []string {
	if !s.set {
		return nil
	}
	return append([]string(nil), s.values...)
}
