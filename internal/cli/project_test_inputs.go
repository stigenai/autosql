package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"sort"
	"strings"

	"autosql/pkg/config"
	"autosql/pkg/dbtest"
	"autosql/pkg/migrate"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
)

type preparedTests struct {
	files  *config.InputFiles
	cases  []dbtest.TestCase
	schema dbtest.SchemaProgram
	verify func(context.Context) error
}

func prepareProjectTests(ctx context.Context, c *config.Config, options projectTestArgs, report *testReport) (preparedTests, error) {
	result := preparedTests{}
	identity, err := c.Identity()
	if err != nil {
		return result, err
	}
	report.Project = identity
	result.files, err = c.OpenInputFiles(ctx)
	if err != nil {
		return result, err
	}
	prepared := false
	defer func() {
		report.Inputs = result.files.Records()
		if !prepared {
			result.files.Close()
		}
	}()
	if err := c.CaptureConfiguration(result.files); err != nil {
		return result, err
	}
	testFiles, err := discoverProjectTests(result.files, c.Effective().Values.Tests.Files)
	if err != nil {
		return result, err
	}
	result.cases, err = dbtest.SelectTests(testFiles, report.Kind, options.filter)
	report.SuiteResult = unrunTestCases(result.cases)
	if err != nil {
		return result, err
	}
	err = prepareTestProgram(ctx, c, &result, report)
	if err != nil {
		return result, err
	}
	report.Inputs = result.files.Records()
	report.InputDigest, err = projectTestDigest(*report, options)
	prepared = err == nil
	return result, err
}

func discoverProjectTests(files *config.InputFiles, patterns []string) ([]dbtest.TestFile, error) {
	selected := map[string]bool{}
	for _, pattern := range patterns {
		matches, err := files.Glob(pattern)
		if err != nil {
			return nil, err
		}
		for _, name := range matches {
			selected[name] = true
		}
	}
	names := make([]string, 0, len(selected))
	for name := range selected {
		names = append(names, name)
	}
	sort.Strings(names)
	var result []dbtest.TestFile
	for _, name := range names {
		file, err := readProjectTest(files, name)
		if err != nil {
			return nil, err
		}
		result = append(result, file)
	}
	return result, nil
}

func readProjectTest(files *config.InputFiles, name string) (dbtest.TestFile, error) {
	if !strings.HasSuffix(name, ".test.hcl") {
		return dbtest.TestFile{}, &dbtest.LanguageError{Range: config.SourceRange{Filename: name, StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 1}, Message: "test files must end in .test.hcl"}
	}
	raw, err := files.ReadFile(name)
	if err != nil {
		return dbtest.TestFile{}, err
	}
	return dbtest.ParseTestFile(name, raw)
}

func prepareTestProgram(ctx context.Context, c *config.Config, result *preparedTests, report *testReport) error {
	if report.Kind == "migration" {
		return prepareTestMigrations(c, result, report)
	}
	sources, err := loadCapturedProjectSources(ctx, c, result.files)
	if err != nil {
		return err
	}
	doc, err := postgres.New().Normalize(ctx, sources.Document)
	if err != nil {
		return projectSourceError(c, 0, err)
	}
	effective := c.Effective()
	doc, err = schema.Select(doc, effective.Values.Include, effective.Values.Exclude)
	if err != nil {
		return projectSourceError(c, 0, err)
	}
	result.schema, err = dbtest.PrepareSchema(ctx, doc, effective.DevelopmentFactory.PostgresVersion)
	report.SchemaDigest = result.schema.Digest()
	result.verify = result.files.VerifyCurrent
	return err
}

func inspectTestMigrations(files *config.InputFiles, name string) (migrate.Snapshot, error) {
	directory, err := files.OpenDirectory(name)
	if err != nil {
		return migrate.Snapshot{}, err
	}
	defer directory.Close()
	return migrate.InspectSnapshot(directory)
}

func prepareTestMigrations(c *config.Config, result *preparedTests, report *testReport) error {
	directory := c.Effective().Values.MigrationDir
	snapshot, err := inspectTestMigrations(result.files, directory)
	if err != nil {
		return &Error{Kind: "validation", Code: ExitValidation, Message: "test migration snapshot is missing or invalid"}
	}
	report.MigrationDigest = snapshot.Manifest.Digest
	for i, test := range result.cases {
		result.cases[i], err = dbtest.BindMigrations(test, snapshot)
		if err != nil {
			return err
		}
	}
	files := result.files
	digest := snapshot.Manifest.Digest
	result.verify = func(ctx context.Context) error {
		if err := files.VerifyCurrent(ctx); err != nil {
			return err
		}
		current, err := inspectTestMigrations(files, directory)
		if err != nil || current.Manifest.Digest != digest {
			return errors.New("test migration snapshot changed")
		}
		return ctx.Err()
	}
	return nil
}

func unrunTestCases(cases []dbtest.TestCase) dbtest.SuiteResult {
	result := dbtest.SuiteResult{Selected: len(cases), Cases: make([]dbtest.CaseResult, 0, len(cases))}
	for _, c := range cases {
		at := c.Range()
		outcome := dbtest.CaseResult{ID: at.Filename + "::" + c.Name(), Name: c.Name(), Kind: c.Kind(), Range: at, Outcome: "not_run", SkipReason: c.SkipReason(), Versions: []string{}, Cleanup: []dbtest.CaseFailure{}}
		if c.SkipReason() != "" {
			outcome.Outcome = "skipped"
			result.Skipped++
		} else {
			result.NotRun++
		}
		result.Cases = append(result.Cases, outcome)
	}
	return result
}

func projectTestDigest(report testReport, options projectTestArgs) (string, error) {
	ids := make([]string, 0, len(report.Cases))
	for _, c := range report.Cases {
		ids = append(ids, c.ID)
	}
	value := struct {
		Kind, Configuration, Migration, Schema string
		Inputs                                 []config.InputFile
		Cases                                  []string
		Parallel                               int
	}{report.Kind, report.Project.ExecutionConfigDigest, report.MigrationDigest, report.SchemaDigest, report.Inputs, ids, options.parallel.value}
	raw, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte("autosql.test-inputs/v1\x00"), raw...))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}
