package cli

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"autosql/pkg/config"
	"autosql/pkg/dbtest"
	"autosql/pkg/secret"
)

type testReport struct {
	dbtest.SuiteResult
	Kind            string                 `json:"kind"`
	Filter          string                 `json:"filter,omitempty"`
	Parallel        int                    `json:"parallel"`
	Timeout         string                 `json:"case_timeout"`
	FactoryDeadline string                 `json:"factory_case_deadline"`
	Environment     string                 `json:"environment"`
	BinaryVersion   string                 `json:"binary_version"`
	Project         config.ProjectIdentity `json:"project"`
	InputDigest     string                 `json:"input_digest"`
	Inputs          []config.InputFile     `json:"inputs"`
	MigrationDigest string                 `json:"migration_digest,omitempty"`
	SchemaDigest    string                 `json:"schema_digest,omitempty"`
	ReportErrors    []string               `json:"report_errors,omitempty"`
}

var testConnectionURL = regexp.MustCompile(`(?i)postgres(?:ql)?://[^\s"'<>]+`)

func sanitizeTestText(redactor *secret.Redactor, value string) string {
	value = testConnectionURL.ReplaceAllString(value, "[REDACTED CONNECTION]")
	return sanitizeOutput(redactor, value)
}

func sanitizeTestJSON(redactor *secret.Redactor, value any) any {
	switch v := value.(type) {
	case string:
		return sanitizeTestText(redactor, v)
	case []any:
		for i := range v {
			v[i] = sanitizeTestJSON(redactor, v[i])
		}
	case map[string]any:
		for key := range v {
			v[key] = sanitizeTestJSON(redactor, v[key])
		}
	}
	return value
}

func safeTestReport(report testReport, redactor *secret.Redactor) (testReport, error) {
	raw, err := json.Marshal(report)
	if err != nil {
		return testReport{}, err
	}
	var value any
	if err := json.Unmarshal(raw, &value); err != nil {
		return testReport{}, err
	}
	raw, err = json.Marshal(sanitizeTestJSON(redactor, value))
	if err != nil {
		return testReport{}, err
	}
	var safe testReport
	err = json.Unmarshal(raw, &safe)
	return safe, err
}

func humanTestReport(report testReport) string {
	var out strings.Builder
	fmt.Fprintf(&out, "%s tests: %d selected, %d executed, %d passed, %d failed, %d skipped, %d not run", report.Kind, report.Selected, report.Executed, report.Passed, report.Failed, report.Skipped, report.NotRun)
	for _, c := range report.Cases {
		fmt.Fprintf(&out, "\n%s %s", c.Outcome, c.ID)
		if c.SkipReason != "" {
			fmt.Fprintf(&out, ": %s", c.SkipReason)
		}
		if c.Primary != nil {
			fmt.Fprintf(&out, "\n  %s", testFailureText(*c.Primary))
		}
		for _, failure := range c.Cleanup {
			fmt.Fprintf(&out, "\n  cleanup: %s", testFailureText(failure))
		}
		if c.Lease != nil && c.Lease.State != "cleaned" {
			fmt.Fprintf(&out, "\n  recovery: %s", devCleanupCommand(c.Lease.Path))
		}
	}
	for _, failure := range report.ReportErrors {
		fmt.Fprintf(&out, "\nreport: %s", failure)
	}
	return out.String()
}

func testFailureText(failure dbtest.CaseFailure) string {
	return fmt.Sprintf("%s:%d:%d: %s: %s", failure.Range.Filename, failure.Range.StartLine, failure.Range.StartColumn, failure.Stage, failure.Message)
}
