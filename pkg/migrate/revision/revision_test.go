package revision

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	neturl "net/url"
	"os"
	"strings"
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
func testManifestN(t *testing.T, n int) migrate.Manifest {
	t.Helper()
	files := make([]migrate.File, 0, n)
	for i := 1; i <= n; i++ {
		files = append(files, migrate.File{Name: fmt.Sprintf("V%d.0.0__migration_%d.sql", i, i), SQL: []byte(fmt.Sprintf("select %d;", i))})
	}
	d := t.TempDir()
	m, e := migrate.Update(d, migrate.UpdateRequest{Files: files})
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

	// A v1 fixture upgrades through every later numbered migration without changing its rows.
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
	var duration int64
	if e = conn.QueryRow(ctx, "select duration_ns from "+q(v1, "revisions")+" where version='0.9.0'").Scan(&duration); e != nil || duration != 0 {
		t.Fatalf("v1 duration=%d err=%v", duration, e)
	}

	// A structurally valid v2 fixture receives only migration 3; failure rolls it back exactly.
	v2 := testSchema(t)
	defer conn.Exec(ctx, "drop schema if exists "+q(v2)+" cascade")
	v2s, _ := Open(Config{URL: url, Schema: v2})
	if e = v2s.Init(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = conn.Exec(ctx, "alter table "+q(v2, "revisions")+" drop column duration_ns, drop column manifest_generation; update "+q(v2, "meta")+" set schema_version=2"); e != nil {
		t.Fatal(e)
	}
	injectedV3 := errors.New("v3 placement failure")
	if e = v2s.InitWithHooks(ctx, InitHooks{AfterMigration: func(step int) error {
		if step == 3 {
			return injectedV3
		}
		return nil
	}}); !errors.Is(e, injectedV3) {
		t.Fatalf("v2 rollback=%v", e)
	}
	var columnExists bool
	if e = conn.QueryRow(ctx, `select exists(select 1 from information_schema.columns where table_schema=$1 and table_name='revisions' and column_name='duration_ns')`, v2).Scan(&columnExists); e != nil || columnExists {
		t.Fatalf("v2 rollback column=%v err=%v", columnExists, e)
	}
	if e = v2s.Init(ctx); e != nil {
		t.Fatal(e)
	}

	future := testSchema(t)
	defer conn.Exec(ctx, "drop schema if exists "+q(future)+" cascade")
	futureStore, _ := Open(Config{URL: url, Schema: future})
	if e = futureStore.Init(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = conn.Exec(ctx, "update "+q(future, "meta")+" set schema_version=$1", SchemaVersion+1); e != nil {
		t.Fatal(e)
	}
	if e = futureStore.Init(ctx); !errors.Is(e, ErrConfig) {
		t.Fatalf("future version accepted: %v", e)
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
	r := Revision{Version: entry.Version, Description: entry.Name, Kind: "migration", FileName: entry.File, FileDigest: entry.SQLDigest, ManifestDigest: m.Digest, ManifestGeneration: m.Generation, PlanDigest: entry.Directives.PlanDigest, ChecksDigest: entry.Directives.CheckDigest, BundleDigest: entry.Directives.BundleDigest, State: "partial", StatementOrdinal: 1, Attempt: 1, Operator: "operator", StartedAt: now, UpdatedAt: now}
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

func TestStatusAllStatesDriftAxesAndReadOnlyExecutorReconciliation(t *testing.T) {
	url := os.Getenv("AUTOSQL_POSTGRES_TEST_DSN")
	if url == "" {
		t.Skip("AUTOSQL_POSTGRES_TEST_DSN unset")
	}
	ctx := context.Background()
	schema := testSchema(t)
	history := "history"
	s, _ := Open(Config{URL: url, Schema: schema, ExecutorHistorySchema: schema, ExecutorHistoryTable: history})
	if e := s.Init(ctx); e != nil {
		t.Fatal(e)
	}
	conn, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(ctx)
	defer conn.Exec(ctx, "drop schema if exists "+q(schema)+" cascade")
	if _, e = conn.Exec(ctx, "create table "+q(schema, history)+"(artifact_digest text,state text)"); e != nil {
		t.Fatal(e)
	}
	m := testManifestN(t, 6)
	now := time.Now().UTC()
	states := []string{"applied", "failed", "partial", "baseline", "checkpoint"}
	for i, state := range states {
		x := m.Entries[i]
		kind := "migration"
		if state == "baseline" || state == "checkpoint" {
			kind = state
		}
		r := Revision{Version: x.Version, Description: x.Name, Kind: kind, FileName: x.File, FileDigest: x.SQLDigest, ManifestDigest: "historical-manifest", ManifestGeneration: m.Generation, ArtifactDigest: fmt.Sprintf("artifact-%d", i), PlanDigest: x.Directives.PlanDigest, ChecksDigest: x.Directives.CheckDigest, BundleDigest: x.Directives.BundleDigest, State: state, Attempt: 1, Operator: "operator", StartedAt: now, UpdatedAt: now, Duration: time.Duration(i+1) * time.Second}
		if i == len(states)-1 {
			r.ManifestDigest = m.Digest
		}
		if e = s.Insert(ctx, r); e != nil {
			t.Fatal(e)
		}
	}
	if _, e = conn.Exec(ctx, "insert into "+q(schema, history)+" values('artifact-0','intended')"); e != nil {
		t.Fatal(e)
	}
	first, e := s.Status(ctx, m)
	if e != nil {
		t.Fatal(e)
	}
	for _, want := range []string{"applied", "failed", "partial", "baseline", "checkpoint", "pending"} {
		if first.Counts[want] != 1 {
			t.Fatalf("missing %s: %+v", want, first)
		}
	}
	if !first.Dirty || first.Entries[0].Guidance == "" || first.Entries[0].DurationNS != int64(time.Second) {
		t.Fatalf("executor/duration=%+v", first.Entries[0])
	}
	beforeRaw, _ := json.Marshal(first)
	second, e := s.Status(ctx, m)
	afterRaw, _ := json.Marshal(second)
	if e != nil || !bytes.Equal(beforeRaw, afterRaw) {
		t.Fatalf("repeated status changed: %v\n%s\n%s", e, beforeRaw, afterRaw)
	}

	// Every manifest-bound field independently produces drift.
	base := m.Entries[5]
	drifts := []func(*Revision){func(r *Revision) { r.FileName = "other.sql" }, func(r *Revision) { r.FileDigest = "sha256:other" }, func(r *Revision) { r.ManifestDigest = "sha256:other" }, func(r *Revision) { r.ManifestGeneration = "other" }, func(r *Revision) { r.PlanDigest = "sha256:other" }, func(r *Revision) { r.ChecksDigest = "sha256:other" }, func(r *Revision) { r.BundleDigest = "sha256:other" }}
	for i, mutate := range drifts {
		ds := testSchema(t)
		store, _ := Open(Config{URL: url, Schema: ds})
		if e = store.Init(ctx); e != nil {
			t.Fatal(e)
		}
		r := Revision{Version: base.Version, Description: base.Name, Kind: "migration", FileName: base.File, FileDigest: base.SQLDigest, ManifestDigest: m.Digest, ManifestGeneration: m.Generation, PlanDigest: base.Directives.PlanDigest, ChecksDigest: base.Directives.CheckDigest, BundleDigest: base.Directives.BundleDigest, State: "applied", Attempt: 1, Operator: "operator", StartedAt: now, UpdatedAt: now}
		mutate(&r)
		if e = store.Insert(ctx, r); e != nil {
			t.Fatal(e)
		}
		status, se := store.Status(ctx, m)
		_, _ = conn.Exec(ctx, "drop schema if exists "+q(ds)+" cascade")
		if se != nil || !status.Drift || status.Counts["drift"] != 1 {
			t.Fatalf("drift %d=%+v err=%v", i, status, se)
		}
	}

	// A relation that attempts mutation proves every executor read runs in the read-only transaction.
	mutSchema := testSchema(t)
	mutStore, _ := Open(Config{URL: url, Schema: mutSchema, ExecutorHistorySchema: mutSchema, ExecutorHistoryTable: "evil_history"})
	if e = mutStore.Init(ctx); e != nil {
		t.Fatal(e)
	}
	defer conn.Exec(ctx, "drop schema if exists "+q(mutSchema)+" cascade")
	entry := testManifest(t).Entries[0]
	manifest := testManifest(t)
	r := Revision{Version: entry.Version, Description: entry.Name, Kind: "migration", FileName: entry.File, FileDigest: entry.SQLDigest, ManifestDigest: manifest.Digest, ManifestGeneration: manifest.Generation, ArtifactDigest: "artifact-side-effect", State: "applied", Attempt: 1, Operator: "operator", StartedAt: now, UpdatedAt: now}
	if e = mutStore.Insert(ctx, r); e != nil {
		t.Fatal(e)
	}
	if _, e = conn.Exec(ctx, "create table "+q(mutSchema, "sentinel")+"(n int); create function "+q(mutSchema, "mutate")+"() returns text language plpgsql as $$begin insert into "+q(mutSchema, "sentinel")+" values(1); return 'intended'; end$$; create view "+q(mutSchema, "evil_history")+" as select 'artifact-side-effect'::text artifact_digest,"+q(mutSchema, "mutate")+"() state"); e != nil {
		t.Fatal(e)
	}
	if _, e = mutStore.Status(ctx, manifest); e == nil {
		t.Fatal("side-effecting executor relation succeeded in read-only status")
	}
	var sentinel int
	if e = conn.QueryRow(ctx, "select count(*) from "+q(mutSchema, "sentinel")).Scan(&sentinel); e != nil || sentinel != 0 {
		t.Fatalf("sentinel=%d err=%v", sentinel, e)
	}
	if _, e = conn.Exec(ctx, "drop view "+q(mutSchema, "evil_history")+"; create view "+q(mutSchema, "evil_history")+" as select 'artifact-side-effect'::text artifact_digest"); e != nil {
		t.Fatal(e)
	}
	if _, e = mutStore.Status(ctx, manifest); e == nil {
		t.Fatal("malformed executor history was ignored")
	}
	if _, e = conn.Exec(ctx, "drop view "+q(mutSchema, "evil_history")+"; create view "+q(mutSchema, "evil_history")+" as select 'artifact-side-effect'::text artifact_digest,'confirmed'::text state"); e != nil {
		t.Fatal(e)
	}
	role := "rev_reader_" + strings.TrimPrefix(testSchema(t), "autosql_rev_")
	defer conn.Exec(ctx, "drop owned by "+q(role)+"; drop role if exists "+q(role))
	if _, e = conn.Exec(ctx, "create role "+q(role)+" login password 'reader-pass'; grant usage on schema "+q(mutSchema)+" to "+q(role)+"; grant select on "+q(mutSchema, "revisions")+" to "+q(role)); e != nil {
		t.Fatal(e)
	}
	parsed, e := neturl.Parse(url)
	if e != nil {
		t.Fatal(e)
	}
	parsed.User = neturl.UserPassword(role, "reader-pass")
	restricted, _ := Open(Config{URL: parsed.String(), Schema: mutSchema, ExecutorHistorySchema: mutSchema, ExecutorHistoryTable: "evil_history"})
	if _, e = restricted.Status(ctx, manifest); e == nil {
		t.Fatal("executor history permission error was ignored")
	}
}
