package cli

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"path/filepath"
	"time"

	"autosql/pkg/config"
	"autosql/pkg/dbtest"
)

type projectTestArgs struct {
	project, environment, filter, junit string
	parallel                            optionalInt
	json, jsonSet, junitSet             bool
}

func parseProjectTestArgs(kind string, args []string, o *output) (projectTestArgs, error) {
	var result projectTestArgs
	flags := newFlags(kind+" test", o.streams.Err)
	flags.StringVar(&result.project, "config", "", "version-2 project JSON or HCL path (required)")
	flags.StringVar(&result.environment, "env", "", "named development environment")
	flags.StringVar(&result.filter, "filter", "", "file::case glob; matches slash-separated project-relative names")
	flags.Var(&result.parallel, "parallel", "maximum simultaneous cases, 1..32 (default: project tests.parallel)")
	flags.BoolVar(&result.json, "json", false, "emit JSON (default: project reporting.json)")
	flags.StringVar(&result.junit, "junit", "", "JUnit destination relative to project; empty disables the project default")
	if err := flags.Parse(args); err != nil {
		return result, usageError(err)
	}
	flags.Visit(func(f *flag.Flag) {
		result.jsonSet = result.jsonSet || f.Name == "json"
		result.junitSet = result.junitSet || f.Name == "junit"
	})
	if result.jsonSet {
		o.json = result.json
	}
	if result.project == "" || flags.NArg() != 0 {
		return result, usageError(errors.New("test requires --config and no positional arguments"))
	}
	if result.parallel.set && (result.parallel.value < 1 || result.parallel.value > 32) {
		return result, usageError(errors.New("test parallelism must be 1..32"))
	}
	return result, nil
}

func runProjectTests(ctx context.Context, kind string, args []string, o *output) error {
	options, err := parseProjectTestArgs(kind, args, o)
	if errors.Is(err, flag.ErrHelp) {
		return o.success(map[string]string{"usage": projectTestUsage(kind)}, projectTestUsage(kind))
	}
	if err != nil {
		return err
	}
	c, err := loadProjectConfig(options.project, options.environment, config.Overrides{})
	if err != nil {
		return err
	}
	effective := c.Effective()
	applyTestDefaults(&options, effective, o)
	if c.Version != config.CurrentVersion || effective.Values.Classification != "development" || effective.DevelopmentFactory == nil {
		return &Error{Kind: "config", Code: ExitConfig, Message: "test requires a version-2 development environment and named factory"}
	}
	report := testReport{Kind: kind, Environment: c.Environment, BinaryVersion: Version, Filter: options.filter, Parallel: options.parallel.value, Timeout: effective.Values.Tests.Timeout, FactoryDeadline: effective.DevelopmentFactory.CaseTimeout}
	prepared, err := prepareProjectTests(ctx, c, options, &report)
	if err == nil {
		defer prepared.files.Close()
		if err = validateTestReportPath(c, options, report.Inputs); err == nil {
			report.SuiteResult, err = executeProjectTests(ctx, c, options, effective, prepared, o)
		}
	}
	return finishProjectTests(c, options, report, err, o)
}

func applyTestDefaults(options *projectTestArgs, effective config.EffectiveEnvironment, o *output) {
	if !options.parallel.set {
		options.parallel.value = effective.Values.Tests.Parallel
	}
	if !options.jsonSet {
		o.json = effective.Values.Reporting.JSON
	}
	if !options.junitSet {
		options.junit = effective.Values.Reporting.JUnit
	}
	if options.junit != "" && !filepath.IsAbs(options.junit) {
		options.junit = filepath.Join(filepath.Dir(options.project), options.junit)
	}
}

func executeProjectTests(ctx context.Context, c *config.Config, options projectTestArgs, effective config.EffectiveEnvironment, prepared preparedTests, o *output) (dbtest.SuiteResult, error) {
	directory, err := devLeaseDirectory("")
	if err != nil {
		return unrunTestCases(prepared.cases), devFactoryError(err)
	}
	factory, err := configuredDevelopmentFactory(c, effective, options.project, directory, o.redactor)
	if err != nil {
		return unrunTestCases(prepared.cases), err
	}
	timeout, _ := time.ParseDuration(effective.Values.Tests.Timeout)
	cleanup, _ := time.ParseDuration(effective.DevelopmentFactory.CleanupTimeout)
	runner := dbtest.SuiteRunner{Factory: factory, Schema: prepared.schema, MigrationDirectory: effective.Values.MigrationDir, Parallel: options.parallel.value, Timeout: timeout, CleanupTimeout: cleanup, VerifyInputs: prepared.verify}
	return runner.Run(ctx, prepared.cases)
}

func projectTestFailure(err error) *Error {
	if err == nil {
		return nil
	}
	var suite *dbtest.SuiteError
	if errors.As(err, &suite) {
		return &Error{Kind: testEnvelopeKind(suite.Failure.Kind), Code: ExitCode(suite.Failure.Code), Message: suite.Failure.Message, Cause: suite}
	}
	var language *dbtest.LanguageError
	if errors.As(err, &language) {
		return &Error{Kind: "validation", Code: ExitValidation, Message: language.Error(), Cause: language}
	}
	return classify(err, &Error{Kind: "validation", Code: ExitValidation, Message: "test inputs could not be prepared"})
}

func testEnvelopeKind(kind string) string {
	switch kind {
	case "assertion":
		return "validation"
	case "cleanup":
		return "migration"
	default:
		return kind
	}
}

func projectTestUsage(kind string) string {
	command := "schema"
	if kind == "migration" {
		command = "migrate"
	}
	return "usage: autosql " + command + " test --config PATH [--env NAME] [--filter 'file::case'] [--parallel 1..32] [--json[=false]] [--junit PATH]\n\nRuns version-1 *.test.hcl files from the selected version-2 development project.\nAll test files are parsed before selection. Zero or all-skipped selections fail.\n--filter uses slash-aware globs over project-relative file::case names.\n--parallel, --json and --junit override project defaults. Relative JUnit paths\nresolve beside the project file; --junit= disables the configured report.\nOnly the named development factory is used. Every started case attempts cleanup."
}

func finishProjectTests(c *config.Config, options projectTestArgs, report testReport, runErr error, o *output) error {
	failure := projectTestFailure(runErr)
	if options.junit != "" {
		if err := publishProjectTestJUnit(c, options, report, failure, o); err != nil {
			report.ReportErrors = append(report.ReportErrors, errTestReportWrite.Error())
			if failure == nil {
				failure = &Error{Kind: "internal", Code: ExitInternal, Message: errTestReportWrite.Error()}
			}
		}
	}
	safe, err := safeTestReport(report, o.redactor)
	if err != nil {
		return &Error{Kind: "internal", Code: ExitInternal, Message: "test report could not be encoded"}
	}
	if failure == nil {
		return o.success(safe, humanTestReport(safe))
	}
	failure.Data = safe
	failure.Message = sanitizeTestText(o.redactor, failure.Message)
	if !o.json {
		if _, err := fmt.Fprintln(o.streams.Out, humanTestReport(safe)); err != nil {
			safe.ReportErrors = append(safe.ReportErrors, "human test report could not be written")
			failure.Data = safe
		}
	}
	return failure
}

func publishProjectTestJUnit(c *config.Config, options projectTestArgs, report testReport, failure *Error, o *output) error {
	if err := validateTestReportPath(c, options, report.Inputs); err != nil {
		return err
	}
	var problem *dbtest.CaseFailure
	if failure != nil {
		problem = &dbtest.CaseFailure{Kind: failure.Kind, Code: int(failure.Code), Stage: "suite", Message: failure.Message}
		var suite *dbtest.SuiteError
		if errors.As(failure, &suite) {
			problem = &suite.Failure
		}
	}
	raw, err := renderTestJUnit(report, problem, o.redactor)
	if err != nil {
		return err
	}
	return writeTestReportAtomic(options.junit, raw)
}
