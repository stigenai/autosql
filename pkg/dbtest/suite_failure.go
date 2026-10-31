package dbtest

import (
	"context"
	"errors"

	"autosql/pkg/config"
)

type CaseFailure struct {
	Kind    string             `json:"kind"`
	Code    int                `json:"exit_code"`
	Stage   string             `json:"stage"`
	Name    string             `json:"name,omitempty"`
	Range   config.SourceRange `json:"source"`
	Message string             `json:"message"`
	cause   error
}

type CaseResult struct {
	ID             string             `json:"id"`
	Name           string             `json:"name"`
	Kind           string             `json:"kind"`
	Range          config.SourceRange `json:"source"`
	Outcome        string             `json:"outcome"`
	SkipReason     string             `json:"skip_reason,omitempty"`
	Started        bool               `json:"started"`
	Executed       bool               `json:"executed"`
	PostgresMajor  int                `json:"postgres_major,omitempty"`
	Versions       []string           `json:"versions"`
	Assertions     int                `json:"assertions"`
	Primary        *CaseFailure       `json:"primary_failure,omitempty"`
	Cleanup        []CaseFailure      `json:"cleanup_failures"`
	Lease          *LeaseInfo         `json:"lease,omitempty"`
	DurationMillis int64              `json:"duration_ms"`
}

func (r *CaseResult) addCleanup(failures []CaseFailure, at config.SourceRange) {
	for _, failure := range failures {
		if failure.Range.Filename == "" {
			failure.Range = at
		}
		r.Cleanup = append(r.Cleanup, failure)
	}
}

func caseIdentity(c TestCase) string { return c.at.Filename + "::" + c.name }
func initialCaseResult(c TestCase) CaseResult {
	result := CaseResult{ID: caseIdentity(c), Name: c.name, Kind: c.kind, Range: c.at, Outcome: "not_run", Versions: []string{}, Cleanup: []CaseFailure{}}
	if c.skip != "" {
		result.Outcome, result.SkipReason = "skipped", c.skip
	}
	return result
}

func testFailure(stage string, op Operation, err error) CaseFailure {
	kind, code, message := testFailureCategory(err)
	return CaseFailure{Kind: kind, Code: code, Stage: stage, Name: op.Name, Range: op.Range, Message: message, cause: err}
}

func testFailureCategory(err error) (string, int, string) {
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return "timeout", 124, "test operation timed out"
	case errors.Is(err, context.Canceled):
		return "canceled", 130, "test operation canceled"
	case errors.Is(err, ErrFactorySecret):
		return "secret", 4, "development credential resolution failed"
	case errors.Is(err, ErrFactoryConfig):
		return "config", 3, "development factory configuration refused"
	case errors.Is(err, ErrOwnership), errors.Is(err, ErrLease):
		return "conflict", 8, "development resource ownership or lease refused"
	case errors.Is(err, errTestInputs):
		return "validation", 5, "test inputs changed or could not be verified"
	default:
		return testDatabaseFailureCategory(err)
	}
}

func testDatabaseFailureCategory(err error) (string, int, string) {
	if errors.Is(err, ErrFactoryConnection) {
		return "connection", 6, "development database connection failed"
	}
	if isAssertionError(err) {
		return "assertion", 5, "test assertion or expected SQLSTATE failed"
	}
	var database *DatabaseError
	if errors.As(err, &database) && len(database.State) == 5 {
		if database.State[:2] == "08" {
			return "connection", 6, "development database connection failed"
		}
		return "migration", 7, "database operation failed (SQLSTATE " + database.State + ")"
	}
	return "migration", 7, "test database operation failed"
}

func isAssertionError(err error) bool {
	return errors.Is(err, ErrScalarCardinality) || errors.Is(err, ErrScalarType) || errors.Is(err, ErrScalarMismatch) || errors.Is(err, ErrExpectedError)
}
