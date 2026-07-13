package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosql/pkg/zdm/shadowsync"
	"github.com/jackc/pgx/v5"
)

func TestShadowSyncCLILiveHumanJSONRedaction(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	t.Setenv("AUTOSQL_SYNC_URL", url)
	ctx := context.Background()
	c, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close(ctx)
	_, _ = c.Exec(ctx, `drop schema if exists cli_sync cascade`)
	defer c.Exec(context.Background(), `drop schema if exists cli_sync cascade`)
	if _, e = c.Exec(ctx, `create schema cli_sync;create table cli_sync.t(id bigint primary key,old_name text,new_name text)`); e != nil {
		t.Fatal(e)
	}
	s, e := shadowsync.New(strings.Repeat("e", 64), "cli_sync", []shadowsync.Table{{Name: "t", Pairs: []shadowsync.Pair{{ID: "p01", OldColumn: "old_name", NewColumn: "new_name", Forward: "value", Reverse: "value"}}}})
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.MarshalJSONCanonical()
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "sync.json")
	if e = os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		action string
		json   bool
	}{{"apply", false}, {"status", true}, {"remove", true}} {
		args := []string{"migrate", "shadow-sync", tc.action, "--file", path, "--url", "env://AUTOSQL_SYNC_URL", "--target", "db", "--env", "test"}
		if tc.json {
			args = append(args, "--json")
		}
		var out, stderr bytes.Buffer
		code := Run(ctx, args, Streams{Out: &out, Err: &stderr})
		if code != 0 {
			t.Fatalf("%s: %s", tc.action, stderr.String())
		}
		combined := out.String() + stderr.String()
		if strings.Contains(combined, "postgres:postgres") || strings.Contains(combined, "127.0.0.1") {
			t.Fatalf("secret leaked: %s", combined)
		}
		if !strings.Contains(combined, "rollback") && tc.action == "apply" {
			t.Fatalf("missing status: %s", combined)
		}
	}
}
