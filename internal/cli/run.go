package cli

import (
	"bytes"
	"context"
	"encoding/json"
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
	return RunWithServices(ctx, args, streams, Services{})
}
func RunWithServices(ctx context.Context, args []string, streams Streams, services Services) int {
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
	if services.ReadPlan == nil {
		services.ReadPlan = DefaultReadPlan{Redactor: redactor}
	}
	tty := false
	if streams.IsTTY != nil {
		tty = streams.IsTTY()
	} else if f, ok := streams.In.(*os.File); ok {
		if info, e := f.Stat(); e == nil {
			tty = info.Mode()&os.ModeCharDevice != 0
		}
	}
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
	case len(args) >= 2 && args[0] == "migrate" && args[1] == "generate":
		trusted := false
		for _, arg := range args[2:] {
			trusted = trusted || arg == "--generation-config" || strings.HasPrefix(arg, "--generation-config=")
		}
		if !trusted {
			err = &Error{Kind: "config", Message: "migrate generate requires --generation-config", Code: ExitConfig}
		} else {
			err = runMigrateGenerate(ctx, args[2:], o, services.ReadPlan)
		}
	case len(args) >= 2 && args[0] == "migrate" && args[1] == "checkpoint":
		err = runMigrateCheckpoint(ctx, args[2:], o)
	case len(args) >= 2 && args[0] == "migrate" && args[1] == "metadata-init":
		err = runMetadataInit(ctx, args[2:], o, redactor)
	case len(args) >= 2 && args[0] == "migrate" && args[1] == "metadata-status":
		err = runMetadataStatus(ctx, args[2:], o, redactor)
	case len(args) >= 2 && args[0] == "migrate" && args[1] == "metadata-baseline":
		err = runMetadataBaseline(ctx, args[2:], o, redactor)
	case len(args) >= 2 && args[0] == "migrate" && args[1] == "validate-online":
		err = runMigrateValidateOnline(args[2:], o)
	case len(args) >= 2 && args[0] == "migrate" && args[1] == "expand-plan":
		err = runMigrateExpandPlan(ctx, args[2:], o, redactor)
	case len(args) >= 2 && args[0] == "migrate" && args[1] == "virtual-schema-apply":
		err = runMigrateVirtualSchema(ctx, args[2:], o, redactor, true)
	case len(args) >= 2 && args[0] == "migrate" && args[1] == "virtual-schema-status":
		err = runMigrateVirtualSchema(ctx, args[2:], o, redactor, false)
	case len(args) >= 3 && args[0] == "migrate" && args[1] == "shadow-sync":
		err = runMigrateShadowSync(ctx, args[2], args[3:], o, redactor)
	case len(args) >= 3 && args[0] == "migrate" && args[1] == "backfill":
		err = runMigrateBackfill(ctx, args[2], args[3:], o, redactor)
	case len(args) >= 2 && args[0] == "migrate" && args[1] == "start-status":
		err = runMigrateStartStatus(ctx, args[2:], o, redactor)
	case len(args) >= 2 && args[0] == "schema" && args[1] == "load":
		err = runSchemaLoad(ctx, args[2:], o)
	case len(args) >= 2 && args[0] == "schema" && args[1] == "inspect":
		err = runSchemaInspect(ctx, args[2:], o, redactor)
	case len(args) >= 2 && args[0] == "schema" && args[1] == "diff":
		err = runSchemaDiff(ctx, args[2:], o, services.ReadPlan)
	case len(args) >= 2 && args[0] == "database" && args[1] == "prepare":
		err = runDatabasePrepare(ctx, args[2:], o, redactor)
	case len(args) >= 2 && args[0] == "database" && args[1] == "bootstrap":
		err = runDatabaseBootstrap(ctx, args[2:], o, redactor)
	case len(args) >= 2 && args[0] == "migrate" && args[1] == "status":
		err = runMigrateStatus(ctx, args[2:], o, redactor)
	case len(args) >= 2 && args[0] == "migrate" && args[1] == "apply":
		err = runMigrateApply(ctx, args[2:], o, services, false, redactor)
	case len(args) >= 2 && args[0] == "migrate" && args[1] == "down":
		err = runMigrateDown(ctx, args[2:], o, services.Down)
	case len(args) >= 2 && args[0] == "migrate" && args[1] == "baseline":
		err = runMigrateApply(ctx, args[2:], o, services, true, redactor)
	case len(args) >= 2 && args[0] == "migrate" && args[1] == "diagnose":
		err = runMigrateDiagnose(ctx, args[2:], o, services, redactor)
	case len(args) >= 3 && args[0] == "migrate" && args[1] == "repair" && (args[2] == "mark" || args[2] == "remove" || args[2] == "reconcile"):
		err = runMigrateRepair(ctx, args[2], args[3:], o, services, redactor)
	case len(args) >= 2 && args[0] == "plan" && args[1] == "edit":
		err = runPlanEdit(ctx, args[2:], o, services.PlanEdit)
	case len(args) >= 2 && args[0] == "plan" && args[1] == "review":
		err = runPlanReview(ctx, args[2:], o)
	case len(args) >= 2 && args[0] == "plan" && args[1] == "revalidate":
		err = runPlanRevalidate(ctx, args[2:], o, services.PlanEdit)
	case len(args) >= 2 && args[0] == "plan" && args[1] == "publish":
		err = runPlanPublish(ctx, args[2:], o, services.PlanEdit)
	case args[0] == "plan":
		err = runPlan(ctx, args[1:], o, services.ReadPlan)
	case args[0] == "apply":
		err = runApply(ctx, args[1:], o, services, tty)
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
	format := fs.String("format", "native", "native, sql, or json")
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
	if *format != "native" && *format != "json" && *format != "sql" && *format != "hcl" {
		return usageError(fmt.Errorf("invalid --format"))
	}
	human, err := doc.MarshalCanonical()
	if err != nil {
		return &Error{Kind: "validation", Message: err.Error(), Code: ExitValidation, Cause: err}
	}
	text := strings.TrimSuffix(string(human), "\n")
	if *format == "sql" {
		text, err = schemaSQL(doc)
		if err != nil {
			return &Error{Kind: "validation", Message: "SQL rendering failed", Code: ExitValidation, Cause: err}
		}
	}
	if *format == "hcl" {
		hcl, herr := source.FormatHCL(doc)
		if herr != nil {
			return &Error{Kind: "validation", Message: "HCL rendering failed", Code: ExitValidation, Cause: herr}
		}
		text = strings.TrimSuffix(string(hcl), "\n")
	}
	if *format == "json" {
		var pretty bytes.Buffer
		if e := json.Indent(&pretty, human, "", "  "); e == nil {
			text = pretty.String()
		}
	}
	return o.success(doc, text)
}

func runSchemaLoad(parent context.Context, args []string, o output) error {
	fs := newFlags("schema load", o.streams.Err)
	timeout := fs.Duration("timeout", 30*time.Second, "maximum command duration")
	jsonFlag := fs.Bool("json", false, "emit JSON envelope")
	var sources stringList
	fs.Var(&sources, "source", "schema source as sql:path, json:path, or hcl:path (repeatable)")
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
			return usageError(fmt.Errorf("invalid --source %q; expected sql:path, json:path, or hcl:path", value))
		}
		var format source.Format
		switch formatName {
		case "sql":
			format = source.FormatSQL
		case "json":
			format = source.FormatNative
		case "hcl":
			format = source.FormatHCLSource
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
func usageBase() string {
	return "usage: autosql <command>\n\ncommands:\n  version [--json]\n  config validate [--config path] [--env name] [--preflight] [--json]\n  database prepare --target-hcl path --maintenance-url env://NAME|file://path [--json]\n  migrate generate --dir path --from source --to source --version semver --label name [--rename-hints value] [--json]\n  migrate checkpoint create|verify --dir path [--json]\n  migrate validate-online --file path --format json|yaml [--json]\n  migrate expand-plan --file path --url env://NAME --public-key env://NAME --plan-signing-key env://NAME --plan-key-id id --target id --env name --expected-fingerprint digest --schema name [--json]\n  migrate virtual-schema-apply|virtual-schema-status --file path --url env://NAME --target id --env name [--json]\n  migrate shadow-sync apply|status|remove --file path --url env://NAME --target id --env name [--allow-lossy] [--allow-non-reversible] [--json]\n  migrate backfill run|status|pause|resume|cancel --file path --url env://NAME --target id --env name [--batch-size n] [--json]\n  migrate metadata-init|metadata-status --url env://NAME [--metadata-schema name] [--json]\n  migrate metadata-baseline --url env://NAME --id id --target id --env name --operator id --schema name [--json]\n  migrate status [--config path --env name | --url env://NAME --migration-dir path] [--revision-schema name] [--json]\n  migrate apply|baseline [--config path | --url env://NAME --migration-dir path] [--from version] [--to version] [--count n] [--dry-run] [--json]\n  migrate down --to version [--dry-run] [--json]\n  migrate diagnose --url env://NAME --migration-dir path [--json]\n  migrate repair mark|remove|reconcile --url env://NAME --proposal file --operator-public-key env://NAME --audit path [--json]\n  schema load --source sql:path|json:path|hcl:path [--source ...] [--json]\n  schema inspect --url env://NAME|file://path [--format native|sql|json|hcl]\n  schema diff --from source --to source [--max-changes n] [--json]\n  plan --from source --to source [--max-changes n] [--json]\n  plan edit --artifact file --sql file --editor id --reason text --output file\n  plan review --artifact file [--json]\n  plan revalidate --draft file --output file [--json]\n  plan publish --attested file --output file [--json]\n  apply --from source --to source [--dry-run|--approve-digest digest|--artifact path] [--no-edits] [--json]"
}
func usage() string {
	base := strings.Replace(usageBase(), "\n  migrate generate", "\n  database bootstrap --file path --maintenance-url env://NAME|file://path [--json]\n  migrate generate", 1)
	return base + "\n  migrate start-status --file path --url env://NAME --target id --env name [--json]"
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
	if len(args) >= 2 && args[0] == "migrate" {
		return "migrate " + args[1]
	}
	if len(args) >= 2 && args[0] == "schema" && args[1] == "load" {
		return "schema load"
	}
	if len(args) >= 2 && args[0] == "schema" && args[1] == "inspect" {
		return "schema inspect"
	}
	if len(args) >= 2 && args[0] == "schema" && args[1] == "diff" {
		return "schema diff"
	}
	if len(args) >= 2 && args[0] == "database" && args[1] == "prepare" {
		return "database prepare"
	}
	if len(args) >= 2 && args[0] == "database" && args[1] == "bootstrap" {
		return "database bootstrap"
	}
	if len(args) >= 2 && args[0] == "plan" && (args[1] == "edit" || args[1] == "review") {
		return "plan " + args[1]
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
