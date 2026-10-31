package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosql/pkg/config"
)

func projectTestFixture(t *testing.T, raw string) string {
	t.Helper()
	project := devProjectFixture(t, "env://ABSENT_TEST_ADMIN", "postgres://127.0.0.1:5432/postgres", 17)
	data, err := os.ReadFile(project)
	if err != nil {
		t.Fatal(err)
	}
	var c config.Config
	if err := json.Unmarshal(data, &c); err != nil {
		t.Fatal(err)
	}
	e := c.Environments["dev"]
	e.Tests = config.TestSettings{Files: []string{"tests/*.test.hcl"}, Parallel: 2}
	e.Reporting = config.ReportingDefaults{JSON: true, JUnit: "reports/tests.xml"}
	c.Environments["dev"] = e
	data, _ = json.Marshal(c)
	if err := os.WriteFile(project, data, 0600); err != nil {
		t.Fatal(err)
	}
	writeTestInput(t, project, "tests/a.test.hcl", raw)
	writeTestInput(t, project, "not-read.sql", "CREATE SCHEMA test; CREATE TABLE test.items(id bigint PRIMARY KEY);")
	return project
}

func writeTestInput(t *testing.T, project, name, raw string) {
	t.Helper()
	name = filepath.Join(filepath.Dir(project), name)
	if err := os.MkdirAll(filepath.Dir(name), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(raw), 0600); err != nil {
		t.Fatal(err)
	}
}

func runTestCLI(t *testing.T, args ...string) (int, testReport, *EnvelopeError) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := Run(context.Background(), args, Streams{Out: &out, Err: &stderr})
	var envelope struct {
		OK    bool           `json:"ok"`
		Data  testReport     `json:"data"`
		Error *EnvelopeError `json:"error"`
	}
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatalf("decode %q: %v; stderr=%s", out.String(), err, stderr.String())
	}
	if envelope.OK != (code == 0) {
		t.Fatal("exit/envelope mismatch")
	}
	if strings.Contains(out.String(), "postgres://") {
		t.Fatal("report exposed connection URL")
	}
	return code, envelope.Data, envelope.Error
}

func TestProjectTestValidationBeforeFactory(t *testing.T) {
	t.Setenv("ABSENT_TEST_ADMIN", "")
	for _, test := range []struct {
		name, source, filter string
		code                 ExitCode
	}{
		{"syntax", "version = 1\nschema_test \"bad\" { sql = \"sentinel\" }", "", ExitValidation},
		{"empty", "version = 1", "", ExitValidation},
		{"skip", "version = 1\nschema_test \"skip\" { skip = \"reason\" }", "", ExitValidation},
		{"filter", "version = 1\nschema_test \"run\" {}", "tests/*::missing", ExitValidation},
		{"bad filter", "version = 1\nschema_test \"run\" {}", "[", ExitValidation},
	} {
		t.Run(test.name, func(t *testing.T) {
			project := projectTestFixture(t, test.source)
			code, report, failure := runTestCLI(t, "schema", "test", "--config", project, "--filter", test.filter)
			if code != int(test.code) || report.Executed != 0 || failure == nil {
				t.Fatalf("code=%d report=%+v failure=%+v", code, report, failure)
			}
			if test.name == "syntax" && (failure.Location == nil || failure.Location.StartLine != 2) {
				t.Fatal("syntax location lost")
			}
			raw, err := os.ReadFile(filepath.Join(filepath.Dir(project), "reports/tests.xml"))
			if err != nil {
				t.Fatal(err)
			}
			var suite junitSuite
			if err := xml.Unmarshal(raw, &suite); err != nil || suite.Errors != 1 {
				t.Fatalf("failed suite is green: %s %v", raw, err)
			}
		})
	}
}

func TestProjectTestParsesAllFilesBeforeFilter(t *testing.T) {
	project := projectTestFixture(t, "version = 1\nschema_test \"run\" {}")
	writeTestInput(t, project, "tests/b.test.hcl", "version = 1\nmigration_test \"broken\" {}")
	code, _, failure := runTestCLI(t, "schema", "test", "--config", project, "--filter", "tests/a.test.hcl::run")
	if code != 5 || failure.Location == nil || failure.Location.Filename != "tests/b.test.hcl" {
		t.Fatalf("filtered syntax escaped validation: %d %+v", code, failure)
	}
}

func TestProjectTestRefusesProductionAndInvalidParallel(t *testing.T) {
	project := projectTestFixture(t, "version = 1\nschema_test \"run\" {}")
	for _, args := range [][]string{{"--env", "prod"}, {"--parallel", "0"}, {"--parallel", "33"}} {
		base := []string{"schema", "test", "--config", project, "--json"}
		code, _, _ := runTestCLI(t, append(base, args...)...)
		if code != 2 && code != 3 {
			t.Fatalf("unsafe options accepted: %d", code)
		}
	}
}

func TestProjectTestRefusesReportOverInputs(t *testing.T) {
	project := projectTestFixture(t, "version = 1\nschema_test \"run\" {}")
	for _, name := range []string{project, "not-read.sql", "tests/a.test.hcl", "not-read-migrations/report.xml"} {
		code, report, _ := runTestCLI(t, "schema", "test", "--config", project, "--junit", name)
		if code != 3 || report.Executed != 0 || len(report.ReportErrors) != 1 {
			t.Fatalf("unsafe report accepted: %d %+v", code, report)
		}
	}
	raw, _ := os.ReadFile(filepath.Join(filepath.Dir(project), "not-read.sql"))
	if !strings.HasPrefix(string(raw), "CREATE SCHEMA") {
		t.Fatal("source overwritten")
	}
}

func TestProjectTestExplicitJSONAndJUnitOverrideDefaults(t *testing.T) {
	project := projectTestFixture(t, "version = 1\nschema_test \"run\" {}")
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"schema", "test", "--config", project, "--filter", "missing", "--json=false", "--junit="}, Streams{Out: &out, Err: &stderr})
	if code != 5 || !strings.Contains(out.String(), "0 selected") || !strings.Contains(stderr.String(), "no runnable") {
		t.Fatalf("overrides: %d %s %s", code, out.String(), stderr.String())
	}
	if _, err := os.Stat(filepath.Join(filepath.Dir(project), "reports")); !os.IsNotExist(err) {
		t.Fatal("disabled JUnit created output")
	}
}

func TestProjectTestHelpNeedsNoProject(t *testing.T) {
	for _, kind := range []string{"schema", "migrate"} {
		var out, stderr bytes.Buffer
		code := Run(context.Background(), []string{kind, "test", "--help"}, Streams{Out: &out, Err: &stderr})
		if code != 0 || !strings.Contains(out.String(), "--junit") || !strings.Contains(out.String(), "--parallel") || stderr.Len() != 0 {
			t.Fatalf("help %d %s %s", code, out.String(), stderr.String())
		}
	}
}
