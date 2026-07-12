package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestMetadataCLILiveHumanJSONSuccessRefusalAndRedaction(t *testing.T) {
	dsn := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	t.Setenv("AUTOSQL_ZDM_CLI_DSN", dsn)
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e9)
	meta := "zdm_cli_" + suffix
	app := meta + "_app"
	qi := func(parts ...string) string { return pgx.Identifier(parts).Sanitize() }
	if _, err = c.Exec(ctx, `create schema `+qi(app)+`; create table `+qi(app, "users")+`(id bigint primary key,email text not null); insert into `+qi(app, "users")+` values(1,'alice@example.test')`); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.Exec(context.Background(), `drop schema if exists `+qi(meta)+` cascade; drop schema if exists `+qi(app)+` cascade`)
		c.Close(context.Background())
	})
	run := func(args ...string) (int, string, string) {
		var out, stderr bytes.Buffer
		code := Run(ctx, args, Streams{Out: &out, Err: &stderr})
		combined := out.String() + stderr.String()
		if strings.Contains(combined, dsn) || strings.Contains(combined, "postgres:postgres@") {
			t.Fatalf("credential/DSN leaked: %s", combined)
		}
		return code, out.String(), stderr.String()
	}
	if code, out, stderr := run("migrate", "metadata-init", "--url", "env://AUTOSQL_ZDM_CLI_DSN", "--metadata-schema", meta); code != 0 || stderr != "" || !strings.Contains(out, "initialized at version 3") {
		t.Fatalf("init code=%d out=%q err=%q", code, out, stderr)
	}
	if code, out, stderr := run("migrate", "metadata-init", "--url", "env://AUTOSQL_ZDM_CLI_DSN", "--metadata-schema", meta, "--json"); code != 0 || stderr != "" {
		t.Fatalf("json init code=%d out=%q err=%q", code, out, stderr)
	} else {
		var e Envelope
		if json.Unmarshal([]byte(out), &e) != nil || !e.OK || e.Command != "migrate metadata-init" {
			t.Fatalf("bad init envelope: %s", out)
		}
	}
	if code, out, stderr := run("migrate", "metadata-status", "--url", "env://AUTOSQL_ZDM_CLI_DSN", "--metadata-schema", meta); code != 0 || stderr != "" || !strings.Contains(out, "recovery clean") {
		t.Fatalf("status code=%d out=%q err=%q", code, out, stderr)
	}
	base := []string{"migrate", "metadata-baseline", "--url", "env://AUTOSQL_ZDM_CLI_DSN", "--metadata-schema", meta, "--id", "adopt", "--target", "primary", "--env", "production", "--operator", "operator_a", "--schema", app}
	if code, out, stderr := run(base...); code != 0 || stderr != "" || !strings.Contains(out, "baseline adopt recorded") {
		t.Fatalf("baseline code=%d out=%q err=%q", code, out, stderr)
	}
	jsonRetry := append(append([]string{}, base...), "--json")
	if code, out, stderr := run(jsonRetry...); code != 0 || stderr != "" {
		t.Fatalf("baseline retry code=%d out=%q err=%q", code, out, stderr)
	} else {
		var e Envelope
		if json.Unmarshal([]byte(out), &e) != nil || !e.OK || e.Command != "migrate metadata-baseline" {
			t.Fatalf("bad baseline envelope: %s", out)
		}
	}
	refused := append([]string{}, base...)
	for i := range refused {
		if refused[i] == "operator_a" {
			refused[i] = "operator_b"
		}
	}
	refused = append(refused, "--json")
	if code, out, stderr := run(refused...); code != int(ExitMigration) || stderr != "" {
		t.Fatalf("refusal code=%d out=%q err=%q", code, out, stderr)
	} else {
		var e Envelope
		if json.Unmarshal([]byte(out), &e) != nil || e.OK || e.Error == nil || !strings.Contains(e.Error.Message, "conflict") {
			t.Fatalf("bad refusal envelope: %s", out)
		}
	}
	if code, out, stderr := run("migrate", "metadata-status", "--url", "env://AUTOSQL_ZDM_CLI_DSN", "--metadata-schema", meta, "--json"); code != 0 || stderr != "" {
		t.Fatalf("json status code=%d out=%q err=%q", code, out, stderr)
	} else {
		var e Envelope
		if json.Unmarshal([]byte(out), &e) != nil || !e.OK || e.Command != "migrate metadata-status" {
			t.Fatalf("bad status envelope: %s", out)
		}
	}
}
