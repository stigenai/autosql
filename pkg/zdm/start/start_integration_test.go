package start

import (
	"context"
	"errors"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"autosql/pkg/zdm/backfill"
	"autosql/pkg/zdm/shadowsync"
	"autosql/pkg/zdm/virtualschema"
	"github.com/jackc/pgx/v5"
)

func TestLiveConcretePipelinePublishesUsableVersion(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	for _, n := range []string{"pipe_state", "pipe_backfill", "pipe_v1", "pipe_v2", "pipe_app"} {
		_, _ = c.Exec(ctx, "drop schema if exists "+q(n)+" cascade")
	}
	defer func() {
		for _, n := range []string{"pipe_state", "pipe_backfill", "pipe_v1", "pipe_v2", "pipe_app"} {
			_, _ = c.Exec(context.Background(), "drop schema if exists "+q(n)+" cascade")
		}
	}()
	if _, err = c.Exec(ctx, `create schema pipe_app;create table pipe_app.accounts(id bigint primary key,name text);insert into pipe_app.accounts values(1,'ALICE'),(2,'BOB')`); err != nil {
		t.Fatal(err)
	}
	artifact := strings.Repeat("c", 64)
	s, _ := New("release_pipe", artifact, "pipe_v1", "pipe_v2")
	shadow, err := shadowsync.New(artifact, "pipe_app", []shadowsync.Table{{Name: "accounts", Pairs: []shadowsync.Pair{{ID: "name_lower", OldColumn: "name", NewColumn: "name_v2", Forward: "lower(value)", Reverse: "upper(value)", Lossy: true}}}})
	if err != nil {
		t.Fatal(err)
	}
	bf, err := backfill.New(artifact, "names", "pipe_app", "accounts", "id", "name", "name_v2", "lower(value)")
	if err != nil {
		t.Fatal(err)
	}
	oldView := virtualschema.TableView{Name: "accounts", PhysicalTable: "accounts", Columns: []virtualschema.ColumnView{{Name: "id", PhysicalColumn: "id"}, {Name: "name", PhysicalColumn: "name"}}}
	newView := virtualschema.TableView{Name: "accounts", PhysicalTable: "accounts", Columns: []virtualschema.ColumnView{{Name: "id", PhysicalColumn: "id"}, {Name: "name", PhysicalColumn: "name_v2"}}}
	vs, err := virtualschema.New(artifact, "pipe_app", virtualschema.SchemaVersion{Name: "pipe_v1", Tables: []virtualschema.TableView{oldView}}, virtualschema.SchemaVersion{Name: "pipe_v2", Tables: []virtualschema.TableView{newView}})
	if err != nil {
		t.Fatal(err)
	}
	p := Pipeline{Spec: s, Expand: func(ctx context.Context) error {
		_, e := c.Exec(ctx, "alter table pipe_app.accounts add column if not exists name_v2 text")
		return e
	}, VirtualConfig: virtualschema.Config{URL: url, Target: "primary", Environment: "pipe", LockTimeoutMS: 500}, Virtual: vs, ShadowConfig: shadowsync.Config{URL: url, Target: "primary", Environment: "pipe", LockTimeoutMS: 500}, Shadow: shadow, ShadowPolicy: shadowsync.Policy{AllowLossy: true}, BackfillConfig: backfill.Config{URL: url, Schema: "pipe_backfill", Target: "primary", Environment: "pipe", BatchSize: 1, MaxRetries: 2, LockTimeoutMS: 500, StatementTimeoutMS: 2000, Backoff: time.Millisecond}, Backfills: []backfill.Spec{bf}}
	st, err := RunPipeline(ctx, Config{URL: url, Schema: "pipe_state", Target: "primary", Environment: "pipe", LockTimeoutMS: 500}, p)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "complete" {
		t.Fatalf("not complete: %+v", st)
	}
	var names []string
	rows, err := c.Query(ctx, "select name from pipe_v2.accounts order by id")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n string
		if err = rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		names = append(names, n)
	}
	rows.Close()
	if len(names) != 2 || names[0] != "alice" || names[1] != "bob" {
		t.Fatalf("new version unusable: %v", names)
	}
	if _, err = c.Exec(ctx, "update pipe_v1.accounts set name='CAROL' where id=1"); err != nil {
		t.Fatal(err)
	}
	var got string
	if err = c.QueryRow(ctx, "select name from pipe_v2.accounts where id=1").Scan(&got); err != nil || got != "carol" {
		t.Fatalf("compatibility failed: %q %v", got, err)
	}
}

func TestLiveInterruptedRetryAndNoDuplicateWork(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	_, _ = c.Exec(ctx, "drop schema if exists start_state cascade;drop schema if exists start_app cascade")
	defer c.Exec(context.Background(), "drop schema if exists start_state cascade;drop schema if exists start_app cascade")
	if _, err = c.Exec(ctx, "create schema start_app;create table start_app.events(name text primary key)"); err != nil {
		t.Fatal(err)
	}
	s, _ := New("release_two", strings.Repeat("a", 64), "v1", "v2")
	cfg := Config{URL: url, Schema: "start_state", Target: "primary", Environment: "test", LockTimeoutMS: 500}
	var calls [5]int32
	action := func(i int, name string) func(context.Context) error {
		return func(ctx context.Context) error {
			atomic.AddInt32(&calls[i], 1)
			_, e := c.Exec(ctx, "insert into start_app.events(name) values($1) on conflict do nothing", name)
			return e
		}
	}
	a := Actions{Validate: action(0, "validate"), Expand: action(1, "expand"), Compatibility: action(2, "compatibility"), Backfill: action(3, "backfill"), Publish: action(4, "publish")}
	stop := errors.New("crash")
	st, err := StartWithHooks(ctx, cfg, s, a, Hooks{AfterPhase: func(p string) error {
		if p == "expand" {
			return stop
		}
		return nil
	}})
	if !errors.Is(err, stop) || st.State != "interrupted" || st.Phase != "expand" || st.Progress != 50 {
		t.Fatalf("interruption not classified: err=%v status=%+v", err, st)
	}
	st, err = Start(ctx, cfg, s, a)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "complete" || st.Progress != 100 || st.PreviousVersion != "v1" || st.NewVersion != "v2" {
		t.Fatalf("bad completion: %+v", st)
	}
	// validate and expand were checkpointed before interruption and are not invoked again.
	if calls != [5]int32{1, 1, 1, 1, 1} {
		t.Fatalf("duplicated phase work: %v", calls)
	}
	if _, err = Start(ctx, cfg, s, a); err != nil {
		t.Fatal(err)
	}
	if calls != [5]int32{1, 1, 1, 1, 1} {
		t.Fatalf("completed retry ran work: %v", calls)
	}
}

func TestLiveConcurrentStartsSerialized(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close(ctx)
	_, _ = c.Exec(ctx, "drop schema if exists start_lock cascade")
	defer c.Exec(context.Background(), "drop schema if exists start_lock cascade")
	s, _ := New("release_lock", strings.Repeat("b", 64), "v1", "v2")
	cfg := Config{URL: url, Schema: "start_lock", Target: "primary", Environment: "test", LockTimeoutMS: 500}
	entered := make(chan struct{})
	release := make(chan struct{})
	done := make(chan error, 1)
	noop := func(context.Context) error { return nil }
	a := Actions{Validate: func(context.Context) error { close(entered); <-release; return nil }, Expand: noop, Compatibility: noop, Backfill: noop, Publish: noop}
	go func() { _, e := Start(ctx, cfg, s, a); done <- e }()
	<-entered
	_, err = Start(ctx, cfg, s, Actions{Validate: noop, Expand: noop, Compatibility: noop, Backfill: noop, Publish: noop})
	if !errors.Is(err, ErrBusy) {
		t.Fatalf("concurrent start not refused: %v", err)
	}
	close(release)
	if err = <-done; err != nil {
		t.Fatal(err)
	}
}
