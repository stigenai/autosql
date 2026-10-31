package projecttesting_test

import (
	"bytes"
	"context"
	"encoding/json"
	"encoding/xml"
	"os"
	"path/filepath"
	"strconv"
	"testing"

	"autosql/internal/cli"
	"autosql/pkg/config"
	"autosql/pkg/dbtest"
)

func TestProjectExampleLiveTestCommands(t *testing.T) {
	endpoint, image := os.Getenv("AUTOSQL_DBTEST_DOCKER_ENDPOINT"), os.Getenv("AUTOSQL_DBTEST_POSTGRES_IMAGE")
	if endpoint == "" || image == "" {
		t.Skip("approved daemon and pinned image required")
	}
	major, err := strconv.Atoi(os.Getenv("AUTOSQL_DBTEST_POSTGRES_MAJOR"))
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	if err := os.CopyFS(root, os.DirFS(".")); err != nil {
		t.Fatal(err)
	}
	c, err := config.Load("container.hcl", func(string) (string, bool) { return "", false }, config.Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	f := c.DevFactories["local_container"]
	f.Endpoint, f.Allowlist, f.Image, f.PostgresVersion = endpoint, []string{endpoint}, image, major
	c.DevFactories["local_container"] = f
	raw, _ := json.Marshal(c)
	project := filepath.Join(root, "runtime.json")
	if err := os.WriteFile(project, raw, 0600); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		command, environment string
		code                 int
	}{{"schema", "local", 0}, {"migrate", "local", 0}, {"migrate", "regression", 5}} {
		var out, stderr bytes.Buffer
		report := test.command + "-" + test.environment + ".xml"
		code := cli.Run(context.Background(), []string{test.command, "test", "--config", project, "--env", test.environment, "--json", "--junit", report}, cli.Streams{Out: &out, Err: &stderr})
		var result struct {
			OK   bool               `json:"ok"`
			Data dbtest.SuiteResult `json:"data"`
		}
		if err := json.Unmarshal(out.Bytes(), &result); err != nil || code != test.code || result.Data.Executed != 1 {
			t.Fatalf("example %s/%s: %d %s %s %v", test.command, test.environment, code, out.String(), stderr.String(), err)
		}
		if result.Data.Cases[0].Lease == nil || result.Data.Cases[0].Lease.State != "cleaned" {
			t.Fatal("example target not cleaned")
		}
		raw, err := os.ReadFile(filepath.Join(root, report))
		if err != nil {
			t.Fatal(err)
		}
		var junit struct {
			XMLName  xml.Name `xml:"testsuite"`
			Tests    int      `xml:"tests,attr"`
			Failures int      `xml:"failures,attr"`
			Errors   int      `xml:"errors,attr"`
		}
		if err := xml.Unmarshal(raw, &junit); err != nil || junit.Tests != 1 {
			t.Fatalf("JUnit: %s %v", raw, err)
		}
		if test.code == 5 && (junit.Failures != 1 || junit.Errors != 1 || result.Data.Cases[0].Primary == nil || len(result.Data.Cases[0].Cleanup) != 1) {
			t.Fatal("intentional primary/cleanup failure lost")
		}
	}
}
