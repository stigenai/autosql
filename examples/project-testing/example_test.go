package projecttesting_test

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"autosql/internal/cli"
	"autosql/pkg/config"
	"autosql/pkg/dbtest"
	"autosql/pkg/migrate"
	"autosql/pkg/schema"
	"autosql/pkg/source"
)

func TestTestLanguageExamples(t *testing.T) {
	for _, name := range []string{"tests/schema.test.hcl", "tests/upgrade.test.hcl", "failures/regression.test.hcl"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		file, err := dbtest.ParseTestFile(name, data)
		if err != nil {
			t.Fatal(err)
		}
		if len(file.Cases()) != 1 || file.Cases()[0].Name() == "" {
			t.Fatal("example has no named case")
		}
		if file.Cases()[0].Kind() == "migration" {
			directory, err := os.Open("migrations")
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := migrate.InspectSnapshot(directory)
			directory.Close()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := dbtest.BindMigrations(file.Cases()[0], snapshot); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestProjectExamplesAgreeWithoutCredentials(t *testing.T) {
	for _, environment := range []string{"local", "ci/postgres", "production"} {
		var previous []byte
		for _, name := range []string{"autosql.hcl", "autosql.json"} {
			c, err := config.Load(name, func(string) (string, bool) { return "", false }, config.Overrides{Environment: environment})
			if err != nil {
				t.Fatal(err)
			}
			effective := c.Effective()
			// File locations intentionally differ; normalized values do not.
			effective.Provenance = nil
			raw, err := json.Marshal(effective)
			if err != nil {
				t.Fatal(err)
			}
			if previous != nil && !bytes.Equal(previous, raw) {
				t.Fatalf("JSON/HCL disagree for %s", environment)
			}
			previous = raw
			inputs, err := c.InspectLocal()
			if err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(inputs.SchemaSources[0].Path)
			if err != nil {
				t.Fatal(err)
			}
			doc, err := source.Load(source.Input{URI: inputs.SchemaSources[0].Path, Format: source.FormatHCLSource, Data: data})
			if err != nil {
				t.Fatalf("example schema: %v", err)
			}
			kinds := map[schema.Kind]string{}
			columns := map[string]bool{}
			for _, resource := range doc.Graph.Resources {
				kinds[resource.Kind] = resource.Name.Name
				if resource.Kind == schema.KindColumn {
					columns[resource.Name.Name] = true
				}
			}
			if len(doc.Graph.Resources) != 4 || kinds[schema.KindSchema] != "project_demo" || kinds[schema.KindTable] != "accounts" || !columns["id"] || !columns["label"] {
				t.Fatalf("example schema lost its table or column: %v", kinds)
			}
		}
	}
}

func TestProjectExampleCLIAndRefusals(t *testing.T) {
	for _, environment := range []string{"local", "ci/postgres", "production", "missing"} {
		var out, stderr bytes.Buffer
		code := cli.Run(context.Background(), []string{"config", "validate", "--config", "autosql.hcl", "--env", environment, "--json"}, cli.Streams{Out: &out, Err: &stderr})
		var result cli.Envelope
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if environment == "missing" {
			if code != int(cli.ExitConfig) || result.Error == nil || result.Error.Location == nil {
				t.Fatalf("unknown environment: code=%d out=%s", code, out.String())
			}
		} else if code != 0 || !result.OK || stderr.Len() != 0 {
			t.Fatalf("%s: code=%d out=%s err=%s", environment, code, out.String(), stderr.String())
		}
	}
	body, err := os.ReadFile("autosql.hcl")
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range []struct{ from, to string }{
		{`parallel = 2`, `paralell = 2`},
		{`env://AUTOSQL_EXAMPLE_LOCAL_TARGET`, `postgres://user:must-not-echo@localhost/app`},
	} {
		c, err := config.Parse("bad.hcl", []byte(strings.Replace(string(body), change.from, change.to, 1)))
		if err == nil {
			err = c.Validate()
		}
		if err == nil || strings.Contains(err.Error(), "must-not-echo") {
			t.Fatalf("unsafe refusal: %v", err)
		}
	}
}

func TestProjectExampleLocalSchemaAndPlanCommands(t *testing.T) {
	for _, environment := range []string{"local", "ci/postgres", "production"} {
		for _, command := range [][]string{{"schema", "load"}, {"plan", "--from", "sql:current.sql"}} {
			var previous map[string]any
			for _, name := range []string{"autosql.hcl", "autosql.json"} {
				var out, stderr bytes.Buffer
				args := append(append([]string{}, command...), "--config", name, "--env", environment, "--json")
				code := cli.Run(context.Background(), args, cli.Streams{Out: &out, Err: &stderr})
				var result struct{ Data map[string]any }
				if err := json.Unmarshal(out.Bytes(), &result); err != nil || code != 0 || stderr.Len() != 0 {
					t.Fatalf("%v: code=%d output=%s stderr=%s decode=%v", args, code, out.String(), stderr.String(), err)
				}
				if result.Data["identity"] == nil || result.Data["source_inputs"] == nil || result.Data["environment"] != environment {
					t.Fatalf("project evidence missing: %s", out.String())
				}
				if previous != nil {
					a, _ := json.Marshal(previous)
					b, _ := json.Marshal(result.Data)
					if !bytes.Equal(a, b) {
						t.Fatalf("JSON/HCL project commands disagree:\n%s\n%s", a, b)
					}
				}
				previous = result.Data
			}
		}
	}
}

func TestDevelopmentExampleRefusesProductionWithoutCredentials(t *testing.T) {
	for _, args := range [][]string{
		{"config", "validate", "--config", "container.hcl", "--env", "local", "--json"},
		{"database", "dev", "create", "--config", "container.hcl", "--env", "production", "--postgres-version", "17", "--json"},
		{"config", "validate", "--config", "development.hcl", "--env", "local", "--json"},
		{"database", "dev", "create", "--config", "development.hcl", "--env", "production", "--postgres-version", "17", "--json"},
	} {
		var out, stderr bytes.Buffer
		code := cli.Run(context.Background(), args, cli.Streams{Out: &out, Err: &stderr})
		expected := 0
		if args[0] == "database" {
			expected = int(cli.ExitConfig)
		}
		if code != expected || stderr.Len() != 0 {
			t.Fatalf("example result=%d %s %s", code, out.String(), stderr.String())
		}
		var result cli.Envelope
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
	}
}
