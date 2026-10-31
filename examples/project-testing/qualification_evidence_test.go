package projecttesting_test

import (
	"context"
	"encoding/xml"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosql/pkg/config"
	"autosql/pkg/dbtest"
	"github.com/jackc/pgx/v5"
)

func validatePackagedCases(report packagedReport, required []string, major, failed int) error {
	r := report.Data
	if len(required) == 0 || r.Selected != len(required) || r.Executed != len(required) || r.Skipped != 0 || r.NotRun != 0 || r.Failed != failed || r.Passed != len(required)-failed || len(r.Cases) != len(required) {
		return fmt.Errorf("required cases did not all execute with their expected outcomes")
	}
	if r.InputDigest == "" || r.Project.ExecutionConfigDigest == "" || r.MigrationDigest == "" && r.SchemaDigest == "" {
		return fmt.Errorf("required input identities are missing")
	}
	for i, c := range r.Cases {
		if c.ID != required[i] || !c.Started || !c.Executed || c.PostgresMajor != major || c.Lease == nil || c.Lease.State != "cleaned" {
			return fmt.Errorf("required case identity, execution, version or cleanup is missing")
		}
		if c.Outcome != "passed" && c.Outcome != "failed" {
			return fmt.Errorf("required case has no completed outcome")
		}
	}
	return nil
}

func semanticWorkflowJSON(t *testing.T, report packagedReport) []byte {
	// Keep outcomes, exact source locations, assertion counts and version order.
	// Machine, target, elapsed-time and lease identities remain in raw evidence.
	result := report.Data.SuiteResult
	result.DurationMillis = 0
	result.Cases = append([]dbtest.CaseResult(nil), result.Cases...)
	for i := range result.Cases {
		result.Cases[i].Lease = nil
		result.Cases[i].PostgresMajor = 0
		result.Cases[i].DurationMillis = 0
	}
	return mustWorkflowJSON(t, result)
}

func verifyPackagedJUnit(t *testing.T, report packagedReport, raw []byte) {
	t.Helper()
	var suite struct {
		XMLName xml.Name `xml:"testsuite"`
		Tests   int      `xml:"tests,attr"`
		Skipped int      `xml:"skipped,attr"`
		Cases   []struct {
			Name     string `xml:"name,attr"`
			File     string `xml:"file,attr"`
			Failures []any  `xml:"failure"`
			Errors   []any  `xml:"error"`
		} `xml:"testcase"`
	}
	if err := xml.Unmarshal(raw, &suite); err != nil {
		t.Fatal(err)
	}
	if suite.Tests != report.Data.Selected || suite.Skipped != 0 || len(suite.Cases) != len(report.Data.Cases) {
		t.Fatal("JUnit case inventory differs from JSON")
	}
	for i, c := range report.Data.Cases {
		x := suite.Cases[i]
		failures, errors := 0, len(c.Cleanup)
		if c.Primary != nil {
			if c.Primary.Kind == "assertion" {
				failures++
			} else {
				errors++
			}
		}
		if x.Name != c.Name || x.File != c.Range.Filename || len(x.Failures) != failures || len(x.Errors) != errors {
			t.Fatal("JUnit semantic result differs from JSON")
		}
	}
}

func (q *workflowQualification) verifyCleanup(t *testing.T, result packagedReport) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := pgx.Connect(ctx, q.admin)
	if err != nil {
		t.Fatal("independent cleanup inspection cannot reach development administrator")
	}
	defer admin.Close(ctx)
	for _, c := range result.Data.Cases {
		if c.Lease == nil || c.Lease.State != "cleaned" {
			t.Fatal("owned lease was not cleaned")
		}
		container := false
		for _, resource := range c.Lease.Resources {
			container = container || resource.Kind == "container"
		}
		for _, resource := range c.Lease.Resources {
			if resource.State != "released" {
				t.Fatal("owned resource not released")
			}
			if resource.Kind == "container" {
				command := exec.CommandContext(ctx, "docker", "--host", q.endpoint, "container", "ls", "--all", "--filter", "name=^/"+resource.Name+"$", "--format", "{{.ID}}")
				raw, err := command.Output()
				if err != nil || len(raw) != 0 {
					t.Fatal("owned container remains or independent inspection failed")
				}
			} else if !container {
				var present bool
				err := admin.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM pg_database WHERE datname=$1) OR EXISTS(SELECT 1 FROM pg_roles WHERE rolname=$1)", resource.Name).Scan(&present)
				if err != nil || present {
					t.Fatal("owned administration resource remains or independent inspection failed")
				}
			}
		}
	}
}

func (q *workflowQualification) refusals(t *testing.T, project, mode string) {
	for _, test := range []struct {
		name  string
		code  int
		flags []string
	}{
		{"empty", 5, []string{"--env", "local", "--filter", "missing::*"}},
		{"production", 3, []string{"--env", "production"}},
	} {
		args := append([]string{"schema", "test", "--config", project, "--json", "--junit="}, test.flags...)
		raw := q.run(t, test.code, args...)
		var result packagedReport
		q.decode(t, raw, &result)
		if result.OK || result.Data.Executed != 0 {
			t.Fatal("refusal executed a case or reported success")
		}
		q.save(t, mode+"-"+test.name+".json", raw)
	}
	q.changedAndSkippedInputs(t, project, mode)
	q.redactionProbe(t, project, mode)
	blocked := filepath.Join(filepath.Dir(project), "reports", "blocked.xml")
	if err := os.MkdirAll(blocked, 0700); err != nil {
		t.Fatal(err)
	}
	raw := q.run(t, 1, "schema", "test", "--config", project, "--env", "local", "--json", "--junit", "reports/blocked.xml")
	var report packagedReport
	q.decode(t, raw, &report)
	if report.OK {
		t.Fatal("report-write failure qualified as success")
	}
	if err := validatePackagedCases(report, []string{requiredWorkflowCase("schema")}, q.major, 0); err != nil {
		t.Fatal(err)
	}
	q.verifyCleanup(t, report)
	q.executed += report.Data.Executed
	q.save(t, mode+"-report-failure.json", raw)
}

func (q *workflowQualification) changedAndSkippedInputs(t *testing.T, project, mode string) {
	root := filepath.Dir(project)
	testFile := filepath.Join(root, "tests", "schema.test.hcl")
	original := q.read(t, testFile)
	skipped := strings.Replace(string(original), "  timeout =", "  skip = \"qualification requires this case\"\n  timeout =", 1)
	if err := os.WriteFile(testFile, []byte(skipped), 0600); err != nil {
		t.Fatal(err)
	}
	raw := q.run(t, 5, "schema", "test", "--config", project, "--env", "local", "--json", "--junit=")
	var report packagedReport
	q.decode(t, raw, &report)
	if report.OK || report.Data.Executed != 0 || report.Data.Skipped != 1 {
		t.Fatal("all-skipped selection qualified")
	}
	q.save(t, mode+"-all-skipped.json", raw)
	if err := os.WriteFile(testFile, original, 0600); err != nil {
		t.Fatal(err)
	}
	migrations, err := filepath.Glob(filepath.Join(root, "migrations", ".autosql-generations", "*", "V2__*.sql"))
	if err != nil || len(migrations) != 1 {
		t.Fatal("fixture version 2 is missing or ambiguous")
	}
	original = q.read(t, migrations[0])
	if err := os.WriteFile(migrations[0], append(append([]byte(nil), original...), '\n'), 0600); err != nil {
		t.Fatal(err)
	}
	raw = q.run(t, 5, "migrate", "test", "--config", project, "--env", "local", "--json", "--junit=")
	q.decode(t, raw, &report)
	if report.OK || report.Data.Executed != 0 {
		t.Fatal("changed migration bytes reached fixture execution")
	}
	q.save(t, mode+"-changed-migration.json", raw)
	if err := os.WriteFile(migrations[0], original, 0600); err != nil {
		t.Fatal(err)
	}
}

func (q *workflowQualification) redactionProbe(t *testing.T, project, mode string) {
	probe := filepath.Join(filepath.Dir(project), "failures", "redaction.test.hcl")
	raw := []byte("version = 1\nmigration_test \"redaction probe\" {\n  version \"1\" {}\n  exec { sql = \"DO $$ BEGIN RAISE EXCEPTION 'postgres://user:autosql-qualification-canary@host.invalid/db'; END $$\" }\n}\n")
	if err := os.WriteFile(probe, raw, 0600); err != nil {
		t.Fatal(err)
	}
	defer os.Remove(probe)
	result := q.run(t, 7, "migrate", "test", "--config", project, "--env", "regression", "--filter", "failures/redaction.test.hcl::*", "--json", "--junit", "reports/redaction.xml")
	var report packagedReport
	q.decode(t, result, &report)
	if err := validatePackagedCases(report, []string{"failures/redaction.test.hcl::redaction probe"}, q.major, 1); err != nil {
		t.Fatal(err)
	}
	q.verifyCleanup(t, report)
	q.executed += report.Data.Executed
	q.save(t, mode+"-redaction.json", result)
	q.save(t, mode+"-redaction.xml", q.read(t, filepath.Join(filepath.Dir(project), "reports", "redaction.xml")))
}

func TestPackagedWorkflowEvidenceRejectsMissingOrSkippedCases(t *testing.T) {
	good := packagedReport{}
	good.Data.SuiteResult = dbtest.SuiteResult{Selected: 1, Executed: 1, Passed: 1, Cases: []dbtest.CaseResult{{ID: "required", Outcome: "passed", Started: true, Executed: true, PostgresMajor: 17, Lease: &dbtest.LeaseInfo{State: "cleaned"}}}}
	good.Data.InputDigest, good.Data.SchemaDigest = "input", "schema"
	good.Data.Project = config.ProjectIdentity{ExecutionConfigDigest: "config"}
	if err := validatePackagedCases(good, []string{"required"}, 17, 0); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*packagedReport){
		func(r *packagedReport) { r.Data.Cases = nil },
		func(r *packagedReport) { r.Data.Skipped = 1; r.Data.Executed = 0 },
		func(r *packagedReport) { r.Data.InputDigest = "" },
		func(r *packagedReport) { r.Data.NotRun = 1 },
	} {
		bad := good
		change(&bad)
		if err := validatePackagedCases(bad, []string{"required"}, 17, 0); err == nil {
			t.Fatal("incomplete qualification accepted")
		}
	}
}
