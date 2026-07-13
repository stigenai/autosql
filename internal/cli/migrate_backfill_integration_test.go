package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosql/pkg/zdm/backfill"
	"github.com/jackc/pgx/v5"
)

func TestBackfillCLILiveHumanJSONRedaction(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	t.Setenv("AUTOSQL_BF_URL", url)
	ctx := context.Background()
	c, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close(ctx)
	for _, s := range []string{"cli_bf_state", "cli_bf"} {
		_, _ = c.Exec(ctx, "drop schema if exists "+pgx.Identifier{s}.Sanitize()+" cascade")
	}
	defer func() {
		for _, s := range []string{"cli_bf_state", "cli_bf"} {
			_, _ = c.Exec(context.Background(), "drop schema if exists "+pgx.Identifier{s}.Sanitize()+" cascade")
		}
	}()
	if _, e = c.Exec(ctx, `create schema cli_bf;create table cli_bf.items(id bigint primary key,old_value text,new_value text);insert into cli_bf.items values(1,'A',null),(2,'B',null),(3,'C',null)`); e != nil {
		t.Fatal(e)
	}
	s, e := backfill.New(strings.Repeat("d", 64), "cli_job", "cli_bf", "items", "id", "old_value", "new_value", "lower(value)")
	if e != nil {
		t.Fatal(e)
	}
	b, e := s.MarshalJSONCanonical()
	if e != nil {
		t.Fatal(e)
	}
	path := filepath.Join(t.TempDir(), "backfill.json")
	if e = os.WriteFile(path, b, 0600); e != nil {
		t.Fatal(e)
	}
	for _, tc := range []struct {
		action string
		json   bool
	}{{"run", false}, {"status", true}} {
		args := []string{"migrate", "backfill", tc.action, "--file", path, "--url", "env://AUTOSQL_BF_URL", "--state-schema", "cli_bf_state", "--target", "db", "--env", "test", "--batch-size", "2"}
		if tc.json {
			args = append(args, "--json")
		}
		var out, stderr bytes.Buffer
		code := Run(ctx, args, Streams{Out: &out, Err: &stderr})
		if code != 0 {
			t.Fatalf("%s: %s", tc.action, stderr.String())
		}
		combined := out.String() + stderr.String()
		if strings.Contains(combined, "postgres:postgres") || strings.Contains(combined, "127.0.0.1") || strings.Contains(combined, "'A'") {
			t.Fatalf("secret/row leaked: %s", combined)
		}
		if !strings.Contains(combined, "complete") {
			t.Fatalf("missing status: %s", combined)
		}
	}
}
