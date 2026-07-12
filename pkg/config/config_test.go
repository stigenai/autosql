package config

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosql/pkg/secret"
)

func fixture(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if err := os.WriteFile(filepath.Join(d, "schema.sql"), []byte("create table t(id int);"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(d, "migrations"), 0700); err != nil {
		t.Fatal(err)
	}
	p := filepath.Join(d, "autosql.json")
	s := `{"version":1,"environment":"dev","environments":{"dev":{"target":"env://FILE_TARGET","schema_sources":[{"kind":"sql","path":"schema.sql"}],"migration_dir":"migrations","timeout":"5s"}}}`
	if err := os.WriteFile(p, []byte(s), 0600); err != nil {
		t.Fatal(err)
	}
	return p
}

func TestPrecedenceAndPreflight(t *testing.T) {
	p := fixture(t)
	env := map[string]string{"AUTOSQL_TARGET": "environment-value", "AUTOSQL_MIGRATION_DIR": "env-migrations", "CLI_TARGET": "cli-value"}
	if err := os.Mkdir(filepath.Join(filepath.Dir(p), "cli-migrations"), 0700); err != nil {
		t.Fatal(err)
	}
	c, err := Load(p, func(k string) (string, bool) { v, ok := env[k]; return v, ok }, Overrides{Target: "env://CLI_TARGET", MigrationDir: "cli-migrations"})
	if err != nil {
		t.Fatal(err)
	}
	r := secret.NewResolver()
	r.Getenv = func(k string) (string, bool) { v, ok := env[k]; return v, ok }
	rt, err := c.Preflight(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Target != "cli-value" || filepath.Base(rt.MigrationDir) != "cli-migrations" || rt.RevisionSchema != "autosql_revision" {
		t.Fatalf("unexpected runtime: %#v", rt)
	}
}

func TestCLIEnvironmentSelectionStillReceivesEnvironmentOverrides(t *testing.T) {
	p := fixture(t)
	b := []byte(`{"version":1,"environment":"dev","environments":{"dev":{"target":"env://DEV","schema_sources":[{"kind":"sql","path":"schema.sql"}],"migration_dir":"migrations"},"prod":{"target":"env://PROD","schema_sources":[{"kind":"sql","path":"schema.sql"}],"migration_dir":"migrations"}}}`)
	if err := os.WriteFile(p, b, 0600); err != nil {
		t.Fatal(err)
	}
	env := map[string]string{"AUTOSQL_TARGET": "environment-target"}
	c, err := Load(p, func(k string) (string, bool) { v, ok := env[k]; return v, ok }, Overrides{Environment: "prod"})
	if err != nil {
		t.Fatal(err)
	}
	if c.Environment != "prod" || c.Environments["prod"].Target != "env://AUTOSQL_TARGET" {
		t.Fatalf("precedence failed: %#v", c)
	}
}

func TestUnknownFieldRejected(t *testing.T) {
	p := fixture(t)
	b, _ := os.ReadFile(p)
	b = append(b[:len(b)-1], []byte(`,"typo":true}`)...)
	os.WriteFile(p, b, 0600)
	if _, err := Load(p, nil, Overrides{}); err == nil {
		t.Fatal("expected error")
	}
}

func TestPreflightDoesNotSerializeSecrets(t *testing.T) {
	p := fixture(t)
	c, err := Load(p, nil, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	r := secret.NewResolver()
	r.Getenv = func(string) (string, bool) { return "postgres://user:password@prod/db", true }
	rt, err := c.Preflight(context.Background(), r)
	if err != nil {
		t.Fatal(err)
	}
	if rt.Target == "" {
		t.Fatal("target not resolved")
	}
	encoded, err := json.Marshal(rt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "password") || strings.Contains(string(encoded), "postgres://") {
		t.Fatalf("runtime serialization leaked target: %s", encoded)
	}
}
