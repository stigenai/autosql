package cli

import (
	"context"
	"encoding/json"
	"encoding/xml"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"autosql/pkg/config"
	"autosql/pkg/dbtest"
	"autosql/pkg/migrate"
)

func liveProjectTestFixture(t *testing.T, mode, source string) string {
	t.Helper()
	major, err := strconv.Atoi(os.Getenv("AUTOSQL_DBTEST_POSTGRES_MAJOR"))
	if err != nil {
		t.Skip("AUTOSQL_DBTEST_POSTGRES_MAJOR unset; requires approved disposable PostgreSQL")
	}
	project := projectTestFixture(t, source)
	raw, err := os.ReadFile(project)
	if err != nil {
		t.Fatal(err)
	}
	var c config.Config
	if err := json.Unmarshal(raw, &c); err != nil {
		t.Fatal(err)
	}
	factory := config.DevelopmentFactory{Mode: mode, PostgresVersion: major, CaseTimeout: "30s", CleanupTimeout: "10s"}
	if mode == "container" {
		factory.Endpoint, factory.Image = os.Getenv("AUTOSQL_DBTEST_DOCKER_ENDPOINT"), os.Getenv("AUTOSQL_DBTEST_POSTGRES_IMAGE")
		if factory.Endpoint == "" || factory.Image == "" {
			t.Skip("approved daemon and pinned image unset")
		}
		factory.Allowlist = []string{factory.Endpoint}
	} else {
		value := os.Getenv("AUTOSQL_DBTEST_ADMIN_DSN")
		if value == "" {
			t.Skip("disposable administrator unset")
		}
		u, err := url.Parse(value)
		if err != nil {
			t.Fatal("invalid disposable administrator")
		}
		u.User, u.RawQuery = nil, ""
		factory.Admin, factory.AdminClassification = "env://AUTOSQL_DBTEST_ADMIN_DSN", "development"
		factory.Allowlist = []string{u.String()}
	}
	c.DevFactories["local"] = factory
	raw, _ = json.Marshal(c)
	if err := os.WriteFile(project, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return project
}

func assertCLITestCleaned(t *testing.T, report testReport) {
	t.Helper()
	for _, c := range report.Cases {
		if !c.Started {
			continue
		}
		if c.Lease == nil || c.Lease.State != "cleaned" {
			t.Fatalf("uncleaned case: %+v", c)
		}
		info, err := dbtest.InspectLease(c.Lease.Path)
		if err != nil || info.State != "cleaned" {
			t.Fatalf("cleanup evidence: %v", err)
		}
	}
}

func TestProjectTestLiveSchemaAndFailureReports(t *testing.T) {
	for _, mode := range []string{"existing_admin", "container"} {
		t.Run(mode, func(t *testing.T) {
			project := liveProjectTestFixture(t, mode, `version = 1
schema_test "passing" {
  fixture { sql = "INSERT INTO test.items VALUES (1)" }
  expect_error "duplicate" {
    sql = "INSERT INTO test.items VALUES (1)"
    sqlstate = "23505"
  }
  assert "count" {
    sql = "SELECT count(*) FROM test.items"
    equals = 1
  }
}
schema_test "failing" {
  assert "isolated" {
    sql = "SELECT count(*) FROM test.items"
    equals = 1
  }
  cleanup { sql = "SELECT 1/0" }
}
schema_test "skipped" { skip = "reason" }
`)
			code, report, failure := runTestCLI(t, "schema", "test", "--config", project, "--parallel", "2")
			if code != 5 || report.Executed != 2 || report.Passed != 1 || report.Failed != 1 || report.Skipped != 1 || failure == nil {
				t.Fatalf("result=%+v failure=%+v code=%d", report, failure, code)
			}
			if report.InputDigest == "" || report.Project.ExecutionConfigDigest == "" || report.SchemaDigest == "" || report.BinaryVersion != Version {
				t.Fatal("input identity missing")
			}
			failed := report.Cases[1]
			if failed.Primary == nil || failed.Primary.Range.StartLine != 14 || len(failed.Cleanup) != 1 {
				t.Fatalf("failure detail lost: %+v", failed)
			}
			assertCLITestCleaned(t, report)
			raw, err := os.ReadFile(filepath.Join(filepath.Dir(project), "reports/tests.xml"))
			if err != nil {
				t.Fatal(err)
			}
			var junit junitSuite
			if err := xml.Unmarshal(raw, &junit); err != nil || junit.Tests != 3 || junit.Failures != 1 || junit.Errors != 1 || junit.Skipped != 1 {
				t.Fatalf("JUnit outcomes: %s %v", raw, err)
			}
			code, selected, _ := runTestCLI(t, "schema", "test", "--config", project, "--filter", "tests/*.test.hcl::passing", "--junit", "reports/filtered.xml")
			if code != 0 || selected.Selected != 1 || selected.Passed != 1 || selected.InputDigest == report.InputDigest {
				t.Fatalf("filter failed: %d %+v", code, selected)
			}
			assertCLITestCleaned(t, selected)
		})
	}
}

func TestProjectTestLiveUpgradeAndReportWriteFailure(t *testing.T) {
	for _, mode := range []string{"existing_admin", "container"} {
		t.Run(mode, func(t *testing.T) {
			project := liveProjectTestFixture(t, mode, `version = 1
migration_test "upgrade" {
  version "1" {
    seed { sql = "INSERT INTO items VALUES (1)" }
  }
  version "2" {
    assert "seed retained" {
      sql = "SELECT label FROM items WHERE id=1"
      equals = "kept"
    }
  }
}
`)
			directory := filepath.Join(filepath.Dir(project), "not-read-migrations")
			if err := os.Mkdir(directory, 0700); err != nil {
				t.Fatal(err)
			}
			if _, err := migrate.Update(directory, migrate.UpdateRequest{Files: []migrate.File{{Name: "V1__initial.sql", SQL: []byte("CREATE TABLE items(id bigint PRIMARY KEY);")}, {Name: "V2__upgrade.sql", SQL: []byte("ALTER TABLE items ADD COLUMN label text NOT NULL DEFAULT 'kept';")}}}); err != nil {
				t.Fatal(err)
			}
			code, report, failure := runTestCLI(t, "migrate", "test", "--config", project)
			if code != 0 || report.Passed != 1 || report.MigrationDigest == "" || len(report.Cases[0].Versions) != 2 {
				t.Fatalf("upgrade failed: %d %+v %+v", code, report, failure)
			}
			assertCLITestCleaned(t, report)
			code, report, _ = runTestCLI(t, "migrate", "test", "--config", project, "--junit", "reports")
			if code != 1 || report.Passed != 1 || len(report.ReportErrors) != 1 {
				t.Fatalf("report failure was green: %d %+v", code, report)
			}
			assertCLITestCleaned(t, report)
			writeTestInput(t, project, "tests/a.test.hcl", "version = 1\nmigration_test \"fail\" {\nversion \"2\" {\nassert \"bad\" {\nsql=\"SELECT 0\"\nequals=1\n}\n}\n}")
			code, report, _ = runTestCLI(t, "migrate", "test", "--config", project, "--junit", "reports")
			if code != 5 || report.Failed != 1 || len(report.ReportErrors) != 1 {
				t.Fatalf("report failure replaced assertion: %d %+v", code, report)
			}
			assertCLITestCleaned(t, report)
		})
	}
}

func TestProjectTestInputGuardChecksConfiguration(t *testing.T) {
	project := projectTestFixture(t, "version = 1\nschema_test \"run\" {}")
	c, err := loadProjectConfig(project, "", config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	report := testReport{Kind: "schema"}
	prepared, err := prepareProjectTests(context.Background(), c, projectTestArgs{project: project}, &report)
	if err != nil {
		t.Fatal(err)
	}
	defer prepared.files.Close()
	file, err := os.OpenFile(project, os.O_APPEND|os.O_WRONLY, 0)
	if err != nil {
		t.Fatal(err)
	}
	_, err = file.WriteString("\n")
	file.Close()
	if err != nil {
		t.Fatal(err)
	}
	if err := prepared.verify(context.Background()); err == nil {
		t.Fatal("configuration change accepted")
	}
}
