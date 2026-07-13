package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosql/pkg/zdm/start"
	"github.com/jackc/pgx/v5"
)

func TestStartStatusCLILive(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	t.Setenv("AUTOSQL_START_URL", url)
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	_, _ = c.Exec(ctx, "drop schema if exists cli_start_state cascade")
	defer c.Exec(context.Background(), "drop schema if exists cli_start_state cascade")
	s, err := start.New("cli_release", strings.Repeat("d", 64), "v1", "v2")
	if err != nil {
		t.Fatal(err)
	}
	noop := func(context.Context) error { return nil }
	if _, err = start.Start(ctx, start.Config{URL: url, Schema: "cli_start_state", Target: "db", Environment: "test", LockTimeoutMS: 500}, s, start.Actions{Validate: noop, Expand: noop, Compatibility: noop, Backfill: noop, Publish: noop}); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(s)
	path := filepath.Join(t.TempDir(), "start.json")
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := Run(ctx, []string{"migrate", "start-status", "--file", path, "--url", "env://AUTOSQL_START_URL", "--state-schema", "cli_start_state", "--target", "db", "--env", "test", "--json"}, Streams{Out: &out, Err: &stderr})
	if code != 0 {
		t.Fatalf("status: %s", stderr.String())
	}
	combined := out.String() + stderr.String()
	if !strings.Contains(combined, `"state":"complete"`) || !strings.Contains(combined, `"progress_percent":100`) {
		t.Fatalf("missing status: %s", combined)
	}
	if strings.Contains(combined, "postgres:postgres") || strings.Contains(combined, "127.0.0.1") {
		t.Fatalf("secret leaked: %s", combined)
	}
}
