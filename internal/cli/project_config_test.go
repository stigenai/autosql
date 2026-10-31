package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosql/pkg/config"
)

func TestProjectConfigValidateReportsEffectiveValuesWithoutResolving(t *testing.T) {
	path := filepath.Join(t.TempDir(), "project.hcl")
	body := `version=2
environment="prod"
environment "prod" {
 classification="production"
 target="file:///not-present/production-secret"
 schema_sources=[{kind="hcl",path="missing.hcl"}]
 migration_dir="migrations"
 include=["configured.*"]
}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_TARGET", "postgres://user:seeded-validation-secret@unreachable.example/production")
	t.Setenv("AUTOSQL_INCLUDE", "environment.*")
	t.Setenv("AUTOSQL_ENV", "missing-environment")
	var out, stderr bytes.Buffer
	args := []string{"config", "validate", "--config", path, "--env", "prod", "--schema-source", "cli.sql", "--include", "flag.*", "--json"}
	if code := Run(context.Background(), args, Streams{Out: &out, Err: &stderr}); code != 0 || stderr.Len() != 0 {
		t.Fatalf("validate code=%d out=%s err=%s", code, out.String(), stderr.String())
	}
	var result struct {
		OK   bool `json:"ok"`
		Data struct {
			Effective config.EffectiveEnvironment `json:"effective"`
			Identity  config.ProjectIdentity      `json:"identity"`
		} `json:"data"`
	}
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	e := result.Data.Effective
	if !strings.HasPrefix(result.Data.Identity.ConfigDigest, "sha256:") || !strings.HasPrefix(result.Data.Identity.ExecutionConfigDigest, "sha256:") {
		t.Fatalf("validation omitted configuration identity: %s", out.String())
	}
	if !result.OK || e.Name != "prod" || e.Values.Target != "env://AUTOSQL_TARGET" || len(e.Values.SchemaSources) != 1 || e.Values.SchemaSources[0].Path != "cli.sql" || len(e.Values.Include) != 1 || e.Values.Include[0] != "flag.*" {
		t.Fatalf("wrong effective selection/overrides: %s", out.String())
	}
	if e.Provenance["/environment"].Kind != "flag" || e.Provenance["/target"].Kind != "environment" || e.Provenance["/include"].Kind != "flag" || e.Provenance["/classification"].Range.Filename != path {
		t.Fatalf("wrong provenance: %s", out.String())
	}
	if strings.Contains(out.String(), "seeded-validation-secret") {
		t.Fatal("normal validation exposed a resolved credential")
	}
}

func TestProjectConfigErrorsHaveMachineLocations(t *testing.T) {
	for _, format := range []string{"json", "hcl"} {
		path := filepath.Join(t.TempDir(), "project."+format)
		body := "{\n  \"version\":2,\n  \"unexpected\":\"seeded-error-secret\"\n}"
		if format == "hcl" {
			body = "version=2\nunexpected=\"seeded-error-secret\"\n"
		}
		if err := os.WriteFile(path, []byte(body), 0600); err != nil {
			t.Fatal(err)
		}
		var out, stderr bytes.Buffer
		code := Run(context.Background(), []string{"config", "validate", "--config", path, "--json"}, Streams{Out: &out, Err: &stderr})
		var result Envelope
		if err := json.Unmarshal(out.Bytes(), &result); err != nil {
			t.Fatal(err)
		}
		if code != int(ExitConfig) || stderr.Len() != 0 || result.Error == nil || result.Error.Location == nil || result.Error.Location.Filename != path || result.Error.Location.StartLine < 2 || result.Error.Field != "/unexpected" {
			t.Fatalf("unlocated failure: code=%d out=%s stderr=%s", code, out.String(), stderr.String())
		}
		if strings.Contains(out.String(), "seeded-error-secret") {
			t.Fatal("invalid field value leaked")
		}
	}
}

func TestProjectConfigV1RetainsResultShape(t *testing.T) {
	path := filepath.Join(t.TempDir(), "autosql.json")
	body := `{"version":1,"environment":"dev","environments":{"dev":{"target":"env://UNRESOLVED","schema_sources":[{"kind":"sql","path":"schema.sql"}],"migration_dir":"migrations"}}}`
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if code := Run(context.Background(), []string{"config", "validate", "--config", path, "--json"}, Streams{Out: &out, Err: &bytes.Buffer{}}); code != 0 {
		t.Fatalf("legacy validation failed: %s", out.String())
	}
	var result struct{ Data map[string]any }
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if len(result.Data) != 3 || result.Data["environment"] != "dev" || result.Data["valid"] != true || result.Data["preflight"] != false {
		t.Fatalf("legacy result shape changed: %s", out.String())
	}
}
