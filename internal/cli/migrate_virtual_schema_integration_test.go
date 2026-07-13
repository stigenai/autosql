package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosql/pkg/zdm/virtualschema"
	"github.com/jackc/pgx/v5"
)

func TestVirtualSchemaCLILiveHumanJSONAndRedaction(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	t.Setenv("AUTOSQL_VSCHEMA_URL", url)
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	for _, s := range []string{"cli_v1", "cli_v2", "cli_physical"} {
		_, _ = c.Exec(ctx, "drop schema if exists "+pgx.Identifier{s}.Sanitize()+" cascade")
	}
	defer func() {
		for _, s := range []string{"cli_v1", "cli_v2", "cli_physical"} {
			_, _ = c.Exec(context.Background(), "drop schema if exists "+pgx.Identifier{s}.Sanitize()+" cascade")
		}
	}()
	if _, err = c.Exec(ctx, `create schema cli_physical; create table cli_physical.widgets(id bigint generated always as identity primary key,name text not null default 'new')`); err != nil {
		t.Fatal(err)
	}
	view := virtualschema.TableView{Name: "widgets", PhysicalTable: "widgets", Columns: []virtualschema.ColumnView{{Name: "id", PhysicalColumn: "id"}, {Name: "name", PhysicalColumn: "name"}}}
	spec, err := virtualschema.New(strings.Repeat("c", 64), "cli_physical", virtualschema.SchemaVersion{Name: "cli_v1", Tables: []virtualschema.TableView{view}}, virtualschema.SchemaVersion{Name: "cli_v2", Tables: []virtualschema.TableView{view}})
	if err != nil {
		t.Fatal(err)
	}
	b, err := spec.MarshalJSONCanonical()
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "virtual.json")
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		cmd  string
		json bool
	}{{"virtual-schema-apply", false}, {"virtual-schema-status", true}} {
		args := []string{"migrate", tc.cmd, "--file", path, "--url", "env://AUTOSQL_VSCHEMA_URL", "--target", "db", "--env", "test"}
		if tc.json {
			args = append(args, "--json")
		}
		var out, stderr bytes.Buffer
		code := Run(ctx, args, Streams{Out: &out, Err: &stderr})
		if code != 0 {
			t.Fatalf("%s failed: %s", tc.cmd, stderr.String())
		}
		combined := out.String() + stderr.String()
		if strings.Contains(combined, "postgres:postgres") || strings.Contains(combined, "127.0.0.1") {
			t.Fatalf("secret leaked: %s", combined)
		}
		if !strings.Contains(combined, "cli_v1") || !strings.Contains(combined, "cli_v2") {
			t.Fatalf("missing discovery: %s", combined)
		}
	}
}
