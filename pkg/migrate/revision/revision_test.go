package revision

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"sync"
	"testing"
	"time"

	"autosql/pkg/migrate"
	"github.com/jackc/pgx/v5"
)

func testSchema(t *testing.T) string {
	t.Helper()
	b := make([]byte, 6)
	if _, e := rand.Read(b); e != nil {
		t.Fatal(e)
	}
	return "autosql_rev_" + hex.EncodeToString(b)
}
func testManifest(t *testing.T) migrate.Manifest {
	t.Helper()
	d := t.TempDir()
	m, e := migrate.Update(d, migrate.UpdateRequest{Files: []migrate.File{{Name: "V1.0.0__create.sql", SQL: []byte("create table app(id bigint);")}}})
	if e != nil {
		t.Fatal(e)
	}
	return m
}

func TestConfigRejectsIdentifierInjection(t *testing.T) {
	for _, bad := range []string{`x";drop schema public;--`, "Upper", "a.b", "" + string(make([]byte, 64))} {
		if _, e := Open(Config{URL: "postgres://x", Schema: bad}); !errors.Is(e, ErrConfig) {
			t.Fatalf("schema=%q err=%v", bad, e)
		}
	}
}

func TestInitConcurrentUpgradeRollbackAndReadOnlyStatus(t *testing.T) {
	url := os.Getenv("AUTOSQL_POSTGRES_TEST_DSN")
	if url == "" {
		t.Skip("AUTOSQL_POSTGRES_TEST_DSN unset")
	}
	ctx := context.Background()
	schema := testSchema(t)
	s, e := Open(Config{URL: url, Schema: schema})
	if e != nil {
		t.Fatal(e)
	}
	conn, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(ctx)
	defer conn.Exec(ctx, "drop schema if exists "+q(schema)+" cascade")
	var wg sync.WaitGroup
	errs := make(chan error, 12)
	for i := 0; i < 12; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.Init(ctx) }()
	}
	wg.Wait()
	close(errs)
	for e = range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	var version int
	if e = conn.QueryRow(ctx, "select schema_version from "+q(schema, "meta")+" where singleton").Scan(&version); e != nil || version != SchemaVersion {
		t.Fatalf("version=%d err=%v", version, e)
	}

	// A v1 fixture upgrades in place to v2 without changing its revision rows.
	v1 := testSchema(t)
	defer conn.Exec(ctx, "drop schema if exists "+q(v1)+" cascade")
	if _, e = conn.Exec(ctx, "create schema "+q(v1)+`; create table `+q(v1, "meta")+`(singleton boolean primary key default true check(singleton),schema_version integer not null,updated_at timestamptz not null); insert into `+q(v1, "meta")+` values(true,1,clock_timestamp()); create table `+q(v1, "revisions")+` (version text primary key, description text not null, kind text not null, file_name text not null,file_digest text not null,manifest_digest text not null,artifact_digest text not null,plan_digest text not null,checks_digest text not null,bundle_digest text not null,state text not null,statement_ordinal integer not null default 0,attempt integer not null,redacted_error text not null default '',operator_identity text not null,started_at timestamptz not null,updated_at timestamptz not null,completed_at timestamptz,from_version text not null default '',to_version text not null default '',supersedes text[] not null default '{}',reversal_of text not null default '',check (kind in ('migration','baseline','checkpoint','reversal')),check (state in ('pending','applied','failed','partial','baseline','checkpoint')),check(attempt>0),check(statement_ordinal>=0)); insert into `+q(v1, "revisions")+` values('0.9.0','legacy','migration','legacy.sql','sha256:f','sha256:m','','','','','applied',1,1,'','operator',clock_timestamp(),clock_timestamp(),clock_timestamp(),'','0.9.0','{}','')`); e != nil {
		t.Fatal(e)
	}
	v1s, _ := Open(Config{URL: url, Schema: v1})
	if e = v1s.Init(ctx); e != nil {
		t.Fatal(e)
	}
	var count int
	if e = conn.QueryRow(ctx, "select count(*) from "+q(v1, "revisions")).Scan(&count); e != nil || count != 1 {
		t.Fatalf("legacy count=%d err=%v", count, e)
	}

	rollback := testSchema(t)
	defer conn.Exec(ctx, "drop schema if exists "+q(rollback)+" cascade")
	rs, _ := Open(Config{URL: url, Schema: rollback})
	injected := errors.New("placement failure")
	if e = rs.InitWithHooks(ctx, InitHooks{AfterMigration: func(step int) error {
		if step == 1 {
			return injected
		}
		return nil
	}}); !errors.Is(e, injected) {
		t.Fatalf("rollback err=%v", e)
	}
	var exists bool
	if e = conn.QueryRow(ctx, `select exists(select 1 from pg_namespace where nspname=$1)`, rollback).Scan(&exists); e != nil || exists {
		t.Fatalf("rollback schema exists=%v err=%v", exists, e)
	}

	readonly := testSchema(t)
	ro, _ := Open(Config{URL: url, Schema: readonly})
	if _, e = ro.Status(ctx, testManifest(t)); e == nil {
		t.Fatal("status initialized missing schema")
	}
	if e = conn.QueryRow(ctx, `select exists(select 1 from pg_namespace where nspname=$1)`, readonly).Scan(&exists); e != nil || exists {
		t.Fatalf("status mutated schema exists=%v err=%v", exists, e)
	}
}

func TestStatusPreservesStatesAndDetectsDriftUnknownDirty(t *testing.T) {
	url := os.Getenv("AUTOSQL_POSTGRES_TEST_DSN")
	if url == "" {
		t.Skip("AUTOSQL_POSTGRES_TEST_DSN unset")
	}
	ctx := context.Background()
	schema := testSchema(t)
	s, _ := Open(Config{URL: url, Schema: schema})
	if e := s.Init(ctx); e != nil {
		t.Fatal(e)
	}
	conn, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(ctx)
	defer conn.Exec(ctx, "drop schema if exists "+q(schema)+" cascade")
	m := testManifest(t)
	entry := m.Entries[0]
	now := time.Now().UTC()
	r := Revision{Version: entry.Version, Description: entry.Name, Kind: "migration", FileName: entry.File, FileDigest: entry.SQLDigest, ManifestDigest: m.Digest, PlanDigest: entry.Directives.PlanDigest, ChecksDigest: entry.Directives.CheckDigest, BundleDigest: entry.Directives.BundleDigest, State: "partial", StatementOrdinal: 1, Attempt: 1, Operator: "operator", StartedAt: now, UpdatedAt: now}
	if e = s.Insert(ctx, r); e != nil {
		t.Fatal(e)
	}
	unknown := r
	unknown.Version = "9.9.9"
	unknown.FileName = "unknown.sql"
	unknown.State = "checkpoint"
	if e = s.Insert(ctx, unknown); e != nil {
		t.Fatal(e)
	}
	status, e := s.Status(ctx, m)
	if e != nil {
		t.Fatal(e)
	}
	if !status.Dirty || status.Counts["partial"] != 1 || status.Counts["unknown"] != 1 || len(status.Entries) != 2 {
		t.Fatalf("status=%+v", status)
	}
	// Stored state remains partial even if statement rows could look complete.
	if e = s.InsertStatement(ctx, StatementAttempt{Version: entry.Version, Ordinal: 1, Attempt: 1, State: "confirmed", Digest: entry.Statements[0].Digest, Operator: "operator", StartedAt: now, CompletedAt: &now}); e != nil {
		t.Fatal(e)
	}
	again, e := s.Status(ctx, m)
	if e != nil || again.Entries[0].RecordedState != "partial" || again.Entries[0].Classification != "partial" {
		t.Fatalf("reinterpreted incomplete row: %+v err=%v", again, e)
	}
}
