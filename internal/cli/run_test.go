package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosql/pkg/postgres"
	"autosql/pkg/schema"
)

func TestVersionJSONContract(t *testing.T) {
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"version", "--json"}, Streams{Out: &out, Err: &stderr})
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	var e Envelope
	if err := json.Unmarshal(out.Bytes(), &e); err != nil {
		t.Fatal(err)
	}
	if !e.OK || e.SchemaVersion != OutputSchemaVersion || e.Command != "version" {
		t.Fatalf("bad envelope: %#v", e)
	}
}

func TestRawArgumentsAreNeverEchoed(t *testing.T) {
	var out bytes.Buffer
	secretValue := "postgres://admin:should-not-echo@prod/db"
	code := Run(context.Background(), []string{"config", "validate", "--target", secretValue, "--config", "missing", "--json"}, Streams{Out: &out, Err: &bytes.Buffer{}})
	if code == 0 {
		t.Fatal("expected error")
	}
	if strings.Contains(out.String(), secretValue) {
		t.Fatalf("argument leaked: %s", out.String())
	}
}

func TestMachineErrorUsesStdout(t *testing.T) {
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"config", "validate", "--config", "missing", "--json"}, Streams{Out: &out, Err: &stderr})
	if code != int(ExitConfig) || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	var e Envelope
	if json.Unmarshal(out.Bytes(), &e) != nil || e.OK || e.Error.ExitCode != int(ExitConfig) {
		t.Fatalf("bad envelope: %s", out.String())
	}
}

func TestHumanErrorUsesStderr(t *testing.T) {
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"wat"}, Streams{Out: &out, Err: &stderr})
	if code != int(ExitUsage) || out.Len() != 0 || !strings.Contains(stderr.String(), "usage") {
		t.Fatalf("out=%q err=%q", out.String(), stderr.String())
	}
}

func TestPreflightRedactsResolvedSecretOnLaterFailure(t *testing.T) {
	d := t.TempDir()
	cfg := filepath.Join(d, "autosql.json")
	content := `{"version":1,"environment":"prod","environments":{"prod":{"target":"env://AUTOSQL_TEST_SECRET","schema_sources":[{"kind":"sql","path":"missing.sql"}],"migration_dir":"missing"}}}`
	if err := os.WriteFile(cfg, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_TEST_SECRET", "postgres://admin:super-secret@prod/db")
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"config", "validate", "--config", cfg, "--preflight", "--json"}, Streams{Out: &out, Err: &stderr})
	if code != int(ExitValidation) {
		t.Fatalf("code=%d output=%s", code, out.String())
	}
	if strings.Contains(out.String(), "super-secret") {
		t.Fatalf("secret leaked: %s", out.String())
	}
}

func TestCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var out bytes.Buffer
	code := Run(ctx, []string{"config", "validate", "--json"}, Streams{Out: &out, Err: &bytes.Buffer{}})
	if code != int(ExitCanceled) {
		t.Fatalf("code=%d output=%s", code, out.String())
	}
}

func TestSchemaLoadJSON(t *testing.T) {
	path := filepath.Join(t.TempDir(), "schema.sql")
	if err := os.WriteFile(path, []byte("CREATE TABLE app.users (id bigint PRIMARY KEY);"), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"schema", "load", "--source", "sql:" + path, "--json"}, Streams{Out: &out, Err: &stderr})
	if code != int(ExitOK) || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q out=%q", code, stderr.String(), out.String())
	}
	var envelope Envelope
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if !envelope.OK || envelope.Command != "schema load" {
		t.Fatalf("bad envelope: %#v", envelope)
	}
}

func TestSchemaLoadConflictHasStableExitCode(t *testing.T) {
	dir := t.TempDir()
	first, second := filepath.Join(dir, "first.sql"), filepath.Join(dir, "second.sql")
	if err := os.WriteFile(first, []byte("CREATE TABLE app.users (id bigint);"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(second, []byte("CREATE TABLE app.users (id text);"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := Run(context.Background(), []string{"schema", "load", "--source", "sql:" + first, "--source", "sql:" + second, "--json"}, Streams{Out: &out, Err: &bytes.Buffer{}})
	if code != int(ExitConflict) {
		t.Fatalf("code=%d out=%s", code, out.String())
	}
}

func TestSchemaInspectResolvesSecretAndEmitsEnvelope(t *testing.T) {
	previous := inspectPostgres
	t.Cleanup(func() { inspectPostgres = previous })
	inspectPostgres = func(_ context.Context, url string, options postgres.Options) (schema.Document, error) {
		if url != "postgres://user:secret@db/app" || len(options.Schemas) != 1 || options.Schemas[0] != "app" {
			t.Fatalf("unexpected inspect request: url=%q options=%+v", url, options)
		}
		return schema.Document{Version: schema.SchemaVersion}, nil
	}
	t.Setenv("AUTOSQL_INSPECT_URL", "postgres://user:secret@db/app")
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"schema", "inspect", "--url", "env://AUTOSQL_INSPECT_URL", "--schema", "app", "--json"}, Streams{Out: &out, Err: &stderr})
	if code != int(ExitOK) || stderr.Len() != 0 {
		t.Fatalf("code=%d stderr=%q", code, stderr.String())
	}
	if strings.Contains(out.String(), "secret") {
		t.Fatalf("secret leaked: %s", out.String())
	}
	var envelope Envelope
	if err := json.Unmarshal(out.Bytes(), &envelope); err != nil || !envelope.OK || envelope.Command != "schema inspect" {
		t.Fatalf("bad envelope: %s (%v)", out.String(), err)
	}
}
func TestSchemaInspectOutputFormats(t *testing.T) {
	previous := inspectPostgres
	t.Cleanup(func() { inspectPostgres = previous })
	n := schema.Name{Name: "app"}
	inspectPostgres = func(context.Context, string, postgres.Options) (schema.Document, error) {
		return schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{{ID: schema.StableID(schema.KindSchema, n), Kind: schema.KindSchema, Name: n, Spec: json.RawMessage(`{}`)}}}}, nil
	}
	t.Setenv("AUTOSQL_INSPECT_URL", "postgres://user:secret@db/app")
	for _, fixture := range []struct{ format, want string }{{"native", `"version"`}, {"json", `  "graph"`}, {"sql", `CREATE SCHEMA "app"`}} {
		t.Run(fixture.format, func(t *testing.T) {
			var out bytes.Buffer
			code := Run(context.Background(), []string{"schema", "inspect", "--url", "env://AUTOSQL_INSPECT_URL", "--format", fixture.format}, Streams{Out: &out, Err: &bytes.Buffer{}})
			if code != 0 || !strings.Contains(out.String(), fixture.want) {
				t.Fatalf("code=%d out=%s", code, out.String())
			}
		})
	}
}
