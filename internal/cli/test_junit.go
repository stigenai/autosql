package cli

import (
	"encoding/xml"
	"fmt"

	"autosql/pkg/dbtest"
	"autosql/pkg/secret"
)

type junitSuite struct {
	XMLName    xml.Name        `xml:"testsuite"`
	Name       string          `xml:"name,attr"`
	Tests      int             `xml:"tests,attr"`
	Failures   int             `xml:"failures,attr"`
	Errors     int             `xml:"errors,attr"`
	Skipped    int             `xml:"skipped,attr"`
	Time       string          `xml:"time,attr"`
	Properties []junitProperty `xml:"properties>property"`
	Cases      []junitCase     `xml:"testcase"`
}

type junitProperty struct {
	Name  string `xml:"name,attr"`
	Value string `xml:"value,attr"`
}

type junitCase struct {
	Name      string         `xml:"name,attr"`
	ClassName string         `xml:"classname,attr"`
	File      string         `xml:"file,attr,omitempty"`
	Line      int            `xml:"line,attr,omitempty"`
	Time      string         `xml:"time,attr"`
	Failures  []junitProblem `xml:"failure"`
	Errors    []junitProblem `xml:"error"`
	Skipped   *junitProblem  `xml:"skipped,omitempty"`
}

type junitProblem struct {
	Type    string `xml:"type,attr,omitempty"`
	Message string `xml:"message,attr"`
	Detail  string `xml:",chardata"`
}

func renderTestJUnit(report testReport, failure *dbtest.CaseFailure, redactor *secret.Redactor) ([]byte, error) {
	report, err := safeTestReport(report, redactor)
	if err != nil {
		return nil, err
	}
	suite := junitSuite{Name: "autosql." + report.Kind, Time: junitSeconds(report.DurationMillis), Properties: testJUnitProperties(report)}
	for _, c := range report.Cases {
		suite.Cases = append(suite.Cases, testJUnitCase(c))
	}
	// Selection and command failures must be visible even if every selected case
	// was skipped. Case failures already have their own complete diagnostics.
	if failure != nil && (report.Failed == 0 || failure.Stage == "suite") {
		problem := junitProblem{Type: failure.Kind, Message: sanitizeTestText(redactor, failure.Message), Detail: sanitizeTestText(redactor, testFailureText(*failure))}
		suite.Cases = append(suite.Cases, junitCase{Name: "suite", ClassName: suite.Name, Time: "0.000", Errors: []junitProblem{problem}})
	}
	countJUnit(&suite)
	raw, err := xml.MarshalIndent(suite, "", "  ")
	return append([]byte(xml.Header), append(raw, '\n')...), err
}

func testJUnitProperties(report testReport) []junitProperty {
	return []junitProperty{
		{Name: "binary_version", Value: report.BinaryVersion},
		{Name: "environment", Value: report.Environment},
		{Name: "config_digest", Value: report.Project.ConfigDigest},
		{Name: "execution_config_digest", Value: report.Project.ExecutionConfigDigest},
		{Name: "input_digest", Value: report.InputDigest},
		{Name: "migration_digest", Value: report.MigrationDigest},
		{Name: "schema_digest", Value: report.SchemaDigest},
	}
}

func testJUnitCase(c dbtest.CaseResult) junitCase {
	result := junitCase{Name: c.Name, ClassName: c.Range.Filename, File: c.Range.Filename, Line: c.Range.StartLine, Time: junitSeconds(c.DurationMillis)}
	if c.Primary != nil {
		problem := junitFailure(*c.Primary)
		if c.Primary.Kind == "assertion" {
			result.Failures = append(result.Failures, problem)
		} else {
			result.Errors = append(result.Errors, problem)
		}
	}
	for _, failure := range c.Cleanup {
		result.Errors = append(result.Errors, junitFailure(failure))
	}
	if c.Outcome == "skipped" || c.Outcome == "not_run" {
		result.Skipped = &junitProblem{Type: c.Outcome, Message: c.SkipReason}
	}
	return result
}

func junitFailure(failure dbtest.CaseFailure) junitProblem {
	return junitProblem{Type: failure.Kind, Message: failure.Message, Detail: testFailureText(failure)}
}

func countJUnit(suite *junitSuite) {
	suite.Tests = len(suite.Cases)
	for _, c := range suite.Cases {
		if len(c.Failures) > 0 {
			suite.Failures++
		}
		if len(c.Errors) > 0 {
			suite.Errors++
		}
		if c.Skipped != nil {
			suite.Skipped++
		}
	}
}

func junitSeconds(milliseconds int64) string { return fmt.Sprintf("%.3f", float64(milliseconds)/1000) }
