package cli

import (
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosql/pkg/config"
	"autosql/pkg/dbtest"
	"autosql/pkg/secret"
)

func TestTestReportsRetainPrimaryCleanupAndRedact(t *testing.T) {
	report := testReport{Kind: "schema", BinaryVersion: "test", SuiteResult: dbtest.SuiteResult{Selected: 3, Executed: 2, Failed: 1, Passed: 1, Skipped: 1, Cases: []dbtest.CaseResult{
		{Name: "bad postgres://reader:hidden@localhost/db", Range: config.SourceRange{Filename: "tests/a.test.hcl", StartLine: 4}, Outcome: "failed", Primary: &dbtest.CaseFailure{Kind: "assertion", Code: 5, Message: "expected value differs"}, Cleanup: []dbtest.CaseFailure{{Kind: "timeout", Code: 124, Stage: "cleanup", Message: "cleanup timed out"}}},
		{Name: "pass", Outcome: "passed"},
		{Name: "skip", Outcome: "skipped", SkipReason: "token=hidden"},
	}}}
	safe, err := safeTestReport(report, secret.NewRedactor("known-secret"))
	if err != nil {
		t.Fatal(err)
	}
	raw, err := json.Marshal(safe)
	if err != nil || strings.Contains(string(raw), "postgres://") || strings.Contains(string(raw), "hidden") {
		t.Fatalf("JSON leaked: %s %v", raw, err)
	}
	xmlRaw, err := renderTestJUnit(report, nil, nil)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(xmlRaw), "hidden") || strings.Contains(string(xmlRaw), "postgres://") {
		t.Fatal("JUnit leaked credentials or connection string")
	}
	var suite junitSuite
	if err := xml.Unmarshal(xmlRaw, &suite); err != nil {
		t.Fatal(err)
	}
	if suite.Tests != 3 || suite.Failures != 1 || suite.Errors != 1 || suite.Skipped != 1 || len(suite.Cases[0].Failures) != 1 || len(suite.Cases[0].Errors) != 1 {
		t.Fatalf("outcomes lost: %+v", suite)
	}
}

func TestJUnitSelectionFailureIsNotSuccessfulSkippedSuite(t *testing.T) {
	report := testReport{Kind: "migration", SuiteResult: dbtest.SuiteResult{Selected: 1, Skipped: 1, Cases: []dbtest.CaseResult{{Name: "skip", Outcome: "skipped", SkipReason: "reason"}}}}
	raw, err := renderTestJUnit(report, &dbtest.CaseFailure{Kind: "validation", Stage: "selection", Code: 5, Message: "no runnable cases"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var suite junitSuite
	if err := xml.Unmarshal(raw, &suite); err != nil || suite.Errors != 1 || suite.Tests != 2 {
		t.Fatalf("suite failure missing: %s %v", raw, err)
	}
}

func TestJUnitPublicationAtomicAndRefusesLinks(t *testing.T) {
	dir := t.TempDir()
	destination := filepath.Join(dir, "nested", "report.xml")
	if err := writeTestReportAtomic(destination, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := writeTestReportAtomic(destination, []byte("second")); err != nil {
		t.Fatal(err)
	}
	got, err := os.ReadFile(destination)
	if err != nil || string(got) != "second" {
		t.Fatalf("publication=%q %v", got, err)
	}
	for _, link := range []string{"symbolic", "hard"} {
		name := filepath.Join(dir, link)
		if link == "symbolic" {
			err = os.Symlink(destination, name)
		} else {
			err = os.Link(destination, name)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := writeTestReportAtomic(name, []byte("bad")); err == nil {
			t.Fatal("link destination accepted")
		}
	}
	if err := writeTestReportAtomic(dir, []byte("bad")); err == nil {
		t.Fatal("directory destination accepted")
	}
	if matches, _ := filepath.Glob(filepath.Join(dir, ".autosql-junit-*")); len(matches) != 0 {
		t.Fatalf("temporary reports leaked: %v", matches)
	}
}
