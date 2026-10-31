package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"syscall"
	"testing"

	"autosql/pkg/config"
	"autosql/pkg/schema"
)

func writeProjectCLIFile(t *testing.T, root, name, body string) string {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func projectCLIConfig(t *testing.T, root string, version int, kind, path string) string {
	t.Helper()
	e := config.Environment{Target: "file:///missing-target-secret", SchemaSources: []config.SchemaSource{{Kind: kind, Path: path}}, MigrationDir: "missing-migrations"}
	c := config.Config{Version: version, Environment: "local", Environments: map[string]config.Environment{"local": e, "ci": e, "prod": e}}
	body, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	return writeProjectCLIFile(t, root, "autosql.json", string(body))
}

func runProjectCLI(t *testing.T, ctx context.Context, args ...string) (int, []byte) {
	t.Helper()
	var out, stderr bytes.Buffer
	code := Run(ctx, append(args, "--json"), Streams{Out: &out, Err: &stderr})
	if stderr.Len() != 0 {
		t.Fatalf("unexpected stderr: %s", stderr.String())
	}
	if !json.Valid(out.Bytes()) {
		t.Fatalf("invalid JSON output: %s", out.String())
	}
	return code, out.Bytes()
}

type projectSchemaOutput struct {
	Data struct {
		Schema      schema.Document         `json:"schema"`
		Environment string                  `json:"environment"`
		Identity    *config.ProjectIdentity `json:"identity"`
		Inputs      []config.InputFile      `json:"source_inputs"`
	} `json:"data"`
}

func decodeProjectSchema(t *testing.T, code int, raw []byte) projectSchemaOutput {
	t.Helper()
	var result projectSchemaOutput
	if code != 0 {
		t.Fatalf("schema load code=%d: %s", code, raw)
	}
	if err := json.Unmarshal(raw, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestProjectSchemaLoadCapturesModulesWithoutResolvingTargets(t *testing.T) {
	root := t.TempDir()
	path := projectCLIConfig(t, root, 2, "hcl", "schema.hcl")
	writeProjectCLIFile(t, root, "schema.hcl", `module "accounts" { source="accounts" }`)
	writeProjectCLIFile(t, root, "accounts/schema.hcl", `schema "demo" {}`)
	writeProjectCLIFile(t, root, "accounts/table.hcl", `table "accounts" { schema="demo" }`)
	for _, environment := range []string{"local", "ci", "prod"} {
		code, raw := runProjectCLI(t, context.Background(), "schema", "load", "--config", path, "--env", environment)
		result := decodeProjectSchema(t, code, raw)
		if result.Data.Identity == nil || result.Data.Environment != environment || len(result.Data.Schema.Graph.Resources) != 2 {
			t.Fatalf("wrong project result: %s", raw)
		}
		var names []string
		for _, record := range result.Data.Inputs {
			names = append(names, record.Path)
		}
		if !reflect.DeepEqual(names, []string{"accounts/schema.hcl", "accounts/table.hcl", "schema.hcl"}) || bytes.Contains(raw, []byte(root)) {
			t.Fatalf("unbound module or nonportable location: %s", raw)
		}
	}
	code, raw := runProjectCLI(t, context.Background(), "schema", "load", "--config", path)
	first := decodeProjectSchema(t, code, raw)
	writeProjectCLIFile(t, root, "accounts/table.hcl", `table "members" { schema="demo" }`)
	code, raw = runProjectCLI(t, context.Background(), "schema", "load", "--config", path)
	second := decodeProjectSchema(t, code, raw)
	if *first.Data.Identity != *second.Data.Identity || reflect.DeepEqual(first.Data.Inputs, second.Data.Inputs) {
		t.Fatal("configuration identity and source content identity were conflated")
	}
}

func TestProjectSchemaLoadFlagsReplaceAndBindConfiguredInputs(t *testing.T) {
	root := t.TempDir()
	path := projectCLIConfig(t, root, 2, "sql", "missing.sql")
	writeProjectCLIFile(t, root, "schema.any", `schema "from_flag" {}`)
	t.Setenv("AUTOSQL_SCHEMA_SOURCES", "also-missing.sql")
	args := []string{"schema", "load", "--config", path, "--source", "hcl:schema.any"}
	code, raw := runProjectCLI(t, context.Background(), args...)
	first := decodeProjectSchema(t, code, raw)
	if len(first.Data.Schema.Graph.Resources) != 1 || len(first.Data.Inputs) != 1 || first.Data.Inputs[0].Path != "schema.any" {
		t.Fatalf("configured sources were not replaced: %s", raw)
	}
	code, raw = runProjectCLI(t, context.Background(), append(args, "--timeout", "5s")...)
	second := decodeProjectSchema(t, code, raw)
	if first.Data.Identity.ExecutionConfigDigest == second.Data.Identity.ExecutionConfigDigest {
		t.Fatal("execution timeout override did not change identity")
	}
}

func TestProjectSchemaLoadLegacyAndDirectOutput(t *testing.T) {
	root := t.TempDir()
	path := projectCLIConfig(t, root, 1, "sql", "schema.sql")
	source := writeProjectCLIFile(t, root, "schema.sql", "CREATE TABLE public.accounts(id integer);")
	code, raw := runProjectCLI(t, context.Background(), "schema", "load", "--config", path)
	result := decodeProjectSchema(t, code, raw)
	if result.Data.Identity != nil || result.Data.Inputs != nil || len(result.Data.Schema.Graph.Resources) == 0 {
		t.Fatalf("legacy load gained a new binding claim: %s", raw)
	}
	code, raw = runProjectCLI(t, context.Background(), "schema", "load", "--source", "sql:"+source)
	var direct struct{ Data schema.Document }
	if err := json.Unmarshal(raw, &direct); err != nil || code != 0 || len(direct.Data.Graph.Resources) == 0 {
		t.Fatalf("direct result shape changed: %d %v %s", code, err, raw)
	}
}

func TestProjectSchemaLoadRefusesUnsafeSourcesWithSafeLocations(t *testing.T) {
	for _, fixture := range []string{"invalid", "symlink", "fifo", "import"} {
		t.Run(fixture, func(t *testing.T) {
			root := t.TempDir()
			path := projectCLIConfig(t, root, 2, "hcl", "schema.hcl")
			source := filepath.Join(root, "schema.hcl")
			switch fixture {
			case "invalid":
				writeProjectCLIFile(t, root, "schema.hcl", `schema "seeded-source-secret" {`)
			case "symlink":
				outside := writeProjectCLIFile(t, t.TempDir(), "private.hcl", `schema "seeded-source-secret" {}`)
				if err := os.Symlink(outside, source); err != nil {
					t.Fatal(err)
				}
			case "fifo":
				if err := syscall.Mkfifo(source, 0600); err != nil {
					t.Fatal(err)
				}
			case "import":
				writeProjectCLIFile(t, root, "schema.hcl", `import "escape" { source="../seeded-source-secret.hcl" }`)
			}
			code, raw := runProjectCLI(t, context.Background(), "schema", "load", "--config", path)
			var envelope Envelope
			if err := json.Unmarshal(raw, &envelope); err != nil {
				t.Fatal(err)
			}
			if code != int(ExitValidation) || envelope.Error == nil || envelope.Error.Location == nil || envelope.Error.Location.Filename != path || envelope.Error.Field != "/environments/local/schema_sources/0" || bytes.Contains(raw, []byte("seeded-source-secret")) {
				t.Fatalf("unsafe or unlocated failure: %d %s", code, raw)
			}
		})
	}
}

func TestProjectSchemaLoadInvalidSelectionAndCancellation(t *testing.T) {
	root := t.TempDir()
	path := projectCLIConfig(t, root, 2, "sql", "schema.sql")
	writeProjectCLIFile(t, root, "schema.sql", "CREATE SCHEMA demo;")
	for _, fixture := range []struct {
		args []string
		code ExitCode
	}{
		{[]string{"--config", path, "--env", "missing"}, ExitConfig},
		{[]string{"--config", path, "--source", "provider:secret"}, ExitUsage},
		{[]string{"--config", path, "--source", "hcl:../private.hcl"}, ExitConfig},
		{[]string{"--source", "sql:schema.sql", "--env", "prod"}, ExitUsage},
	} {
		code, raw := runProjectCLI(t, context.Background(), append([]string{"schema", "load"}, fixture.args...)...)
		if code != int(fixture.code) {
			t.Fatalf("wrong refusal: %d %s", code, raw)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	code, raw := runProjectCLI(t, ctx, "schema", "load", "--config", path)
	if code != 130 || !strings.Contains(string(raw), "cancel") {
		t.Fatalf("cancellation was masked: %d %s", code, raw)
	}
}

func TestProjectSchemaLoadRetainsCompositionConflictCode(t *testing.T) {
	root := t.TempDir()
	path := projectCLIConfig(t, root, 2, "sql", "a.sql")
	writeProjectCLIFile(t, root, "a.sql", "CREATE TABLE demo.accounts(id integer);")
	writeProjectCLIFile(t, root, "b.sql", "CREATE TABLE demo.accounts(id text);")
	code, raw := runProjectCLI(t, context.Background(), "schema", "load", "--config", path, "--source", "sql:a.sql", "--source", "sql:b.sql")
	if code != int(ExitConflict) || !bytes.Contains(raw, []byte(`"field":`)) {
		t.Fatalf("source conflict lost its code or location: %d %s", code, raw)
	}
}
