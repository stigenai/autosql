package dbtest

import (
	"context"
	"errors"
	"path"
	"slices"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

// OwnedFactory deliberately excludes legacy schema-only factories and arbitrary
// Database adapters. File-based tests execute only on an owned restricted target.
type OwnedFactory interface {
	Create(context.Context, string) (*DisposableTarget, error)
}

type SuiteRunner struct {
	Factory                 OwnedFactory
	Schema                  SchemaProgram
	MigrationDirectory      string
	Parallel                int
	Timeout, CleanupTimeout time.Duration
	// VerifyInputs checks captured inputs before provisioning and each version.
	// Callers must support concurrent read-only calls when Parallel > 1.
	VerifyInputs func(context.Context) error
	runCase      func(context.Context, TestCase) CaseResult
}

type SuiteResult struct {
	Selected       int          `json:"selected"`
	Executed       int          `json:"executed"`
	Passed         int          `json:"passed"`
	Failed         int          `json:"failed"`
	Skipped        int          `json:"skipped"`
	NotRun         int          `json:"not_run"`
	Cases          []CaseResult `json:"cases"`
	DurationMillis int64        `json:"duration_ms"`
}

// SelectTests sorts files, preserves declaration order, and validates selection
// before a runner can acquire a database. Filters use path.Match semantics.
func SelectTests(files []TestFile, kind, filter string) ([]TestCase, error) {
	if kind != "schema" && kind != "migration" {
		return nil, selectionFailure("test kind must be schema or migration")
	}
	if _, err := path.Match(filter, ""); err != nil {
		return nil, selectionFailure("invalid test filter glob")
	}
	files = slices.Clone(files)
	sort.Slice(files, func(i, j int) bool { return files[i].filename < files[j].filename })
	result, err := selectedTestCases(files, kind, filter)
	if err != nil {
		return nil, err
	}
	if err := validateRunnableCases(result); err != nil {
		return result, err
	}
	return result, nil
}

func selectedTestCases(files []TestFile, kind, filter string) ([]TestCase, error) {
	var result []TestCase
	seen := map[string]bool{}
	for _, file := range files {
		for _, c := range file.cases {
			id := caseIdentity(c)
			if seen[id] {
				return nil, selectionFailure("duplicate fully qualified case name")
			}
			seen[id] = true
			match, _ := path.Match(filter, id)
			if c.kind == kind && (filter == "" || match) {
				result = append(result, c)
			}
		}
	}
	return result, nil
}

func validateRunnableCases(cases []TestCase) error {
	runnable := 0
	seen := map[string]bool{}
	for _, c := range cases {
		if c.name == "" || (c.kind != "schema" && c.kind != "migration") {
			return selectionFailure("test cases must come from the versioned parser")
		}
		if seen[caseIdentity(c)] {
			return selectionFailure("duplicate fully qualified case name")
		}
		seen[caseIdentity(c)] = true
		if c.skip == "" {
			runnable++
		}
	}
	if runnable == 0 {
		return selectionFailure("selection contains no runnable test cases")
	}
	return nil
}

func (r SuiteRunner) validate(cases []TestCase) error {
	if r.Parallel < 1 || r.Parallel > 32 || r.Timeout < 0 || r.CleanupTimeout < 0 {
		return selectionFailure("parallelism must be 1..32 and timeouts must be nonnegative")
	}
	if err := validateRunnableCases(cases); err != nil {
		return err
	}
	if r.runCase != nil {
		return nil
	}
	if r.Factory == nil {
		return selectionFailure("an owned development factory is required")
	}
	return r.validatePrograms(cases)
}

func (r SuiteRunner) validatePrograms(cases []TestCase) error {
	for _, c := range cases {
		if c.kind == "schema" && !r.Schema.ready {
			return selectionFailure("schema tests require a prepared schema program")
		}
		if c.kind == "migration" && c.migrationDigest == "" {
			return selectionFailure("migration tests require a verified snapshot binding")
		}
	}
	return nil
}

func (r SuiteRunner) Run(ctx context.Context, cases []TestCase) (SuiteResult, error) {
	result := SuiteResult{Selected: len(cases), Cases: make([]CaseResult, len(cases))}
	for i, c := range cases {
		result.Cases[i] = initialCaseResult(c)
	}
	if err := r.validate(cases); err != nil {
		countSuiteOutcomes(&result)
		return result, err
	}
	started := time.Now()
	r.schedule(ctx, cases, result.Cases)
	result.DurationMillis = time.Since(started).Milliseconds()
	countSuiteOutcomes(&result)
	if err := ctx.Err(); err != nil {
		failure := testFailure("suite", Operation{}, err)
		return result, &SuiteError{Failure: failure, cause: err}
	}
	for _, c := range result.Cases {
		if failure := caseOutcomeFailure(c); failure != nil {
			return result, &SuiteError{Failure: *failure, cause: failure.cause}
		}
	}
	return result, nil
}

func (r SuiteRunner) schedule(ctx context.Context, cases []TestCase, results []CaseResult) {
	var next atomic.Int64
	var workers sync.WaitGroup
	for range min(r.Parallel, len(cases)) {
		workers.Go(func() {
			for ctx.Err() == nil {
				i := int(next.Add(1) - 1)
				if i >= len(cases) {
					return
				}
				if cases[i].skip != "" {
					continue
				}
				if ctx.Err() != nil {
					return
				}
				results[i] = r.executeCase(ctx, cases[i])
			}
		})
	}
	workers.Wait()
}

func countSuiteOutcomes(result *SuiteResult) {
	for _, c := range result.Cases {
		if c.Executed {
			result.Executed++
		}
		switch c.Outcome {
		case "passed":
			result.Passed++
		case "failed":
			result.Failed++
		case "skipped":
			result.Skipped++
		default:
			result.NotRun++
		}
	}
}

func caseOutcomeFailure(c CaseResult) *CaseFailure {
	if c.Primary != nil {
		return c.Primary
	}
	if len(c.Cleanup) > 0 {
		result := c.Cleanup[0]
		result.Kind, result.Code, result.cause = "cleanup", 7, nil
		return &result
	}
	return nil
}

type SuiteError struct {
	Failure CaseFailure
	cause   error
}

func (e *SuiteError) Error() string { return e.Failure.Message }
func (e *SuiteError) Unwrap() error { return e.cause }
func selectionFailure(message string) error {
	return &SuiteError{Failure: CaseFailure{Kind: "validation", Code: 5, Stage: "selection", Message: message}}
}

var errTestInputs = errors.New("test inputs changed or could not be verified")
