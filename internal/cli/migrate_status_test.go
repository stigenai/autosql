package cli

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"os"
	"testing"
	"time"

	"autosql/pkg/migrate"
	"autosql/pkg/migrate/revision"
	"github.com/jackc/pgx/v5"
)

func TestMigrateStatusJSONLive(t *testing.T) {
	url := os.Getenv("AUTOSQL_POSTGRES_TEST_DSN")
	if url == "" {
		t.Skip("AUTOSQL_POSTGRES_TEST_DSN unset")
	}
	b := make([]byte, 6)
	if _, e := rand.Read(b); e != nil {
		t.Fatal(e)
	}
	schemaName := "autosql_status_" + hex.EncodeToString(b)
	store, e := revision.Open(revision.Config{URL: url, Schema: schemaName})
	if e != nil {
		t.Fatal(e)
	}
	if e = store.Init(context.Background()); e != nil {
		t.Fatal(e)
	}
	conn, e := pgx.Connect(context.Background(), url)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(context.Background())
	defer conn.Exec(context.Background(), "drop schema if exists "+pgx.Identifier{schemaName}.Sanitize()+" cascade")
	dir := t.TempDir()
	manifest, e := migrate.Update(dir, migrate.UpdateRequest{Files: []migrate.File{{Name: "V1.0.0__create.sql", SQL: []byte("create table app(id bigint);")}}})
	if e != nil {
		t.Fatal(e)
	}
	entry := manifest.Entries[0]
	now := time.Now().UTC()
	if e = store.Insert(context.Background(), revision.Revision{Version: entry.Version, Description: entry.Name, Kind: "migration", FileName: entry.File, FileDigest: entry.SQLDigest, ManifestDigest: manifest.Digest, PlanDigest: entry.Directives.PlanDigest, ChecksDigest: entry.Directives.CheckDigest, BundleDigest: entry.Directives.BundleDigest, State: "applied", StatementOrdinal: 1, Attempt: 1, Operator: "release", StartedAt: now, UpdatedAt: now, CompletedAt: &now}); e != nil {
		t.Fatal(e)
	}
	t.Setenv("AUTOSQL_REVISION_STATUS_URL", url)
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"migrate", "status", "--url", "env://AUTOSQL_REVISION_STATUS_URL", "--migration-dir", dir, "--revision-schema", schemaName, "--json"}, Streams{Out: &out, Err: &stderr})
	if code != 0 || stderr.Len() != 0 {
		t.Fatalf("code=%d out=%s err=%s", code, out.String(), stderr.String())
	}
	var env Envelope
	if e = json.Unmarshal(out.Bytes(), &env); e != nil || !env.OK || env.Command != "migrate" {
		t.Fatalf("envelope=%+v err=%v", env, e)
	}
}
