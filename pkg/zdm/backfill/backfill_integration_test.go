package backfill

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"autosql/pkg/zdm/shadowsync"
	"github.com/jackc/pgx/v5"
)

func TestLiveBoundedResumeControlsAndConcurrentWrites(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
	defer cancel()
	c, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close(ctx)
	for _, s := range []string{"bf_state", "bf_app"} {
		_, _ = c.Exec(ctx, "drop schema if exists "+q(s)+" cascade")
	}
	defer func() {
		for _, s := range []string{"bf_state", "bf_app"} {
			_, _ = c.Exec(context.Background(), "drop schema if exists "+q(s)+" cascade")
		}
	}()
	if _, e = c.Exec(ctx, `create schema bf_app;create table bf_app.items(id bigint primary key,old_value text,new_value text,new_cancel text)`); e != nil {
		t.Fatal(e)
	}
	for i := 1; i <= 25; i++ {
		if _, e = c.Exec(ctx, `insert into bf_app.items(id,old_value) values($1,$2)`, i, fmt.Sprintf("Name%02d", i)); e != nil {
			t.Fatal(e)
		}
	}
	// The synchronization trigger must not reverse-transform the source during backfill.
	ss, e := shadowsync.New(strings.Repeat("a", 64), "bf_app", []shadowsync.Table{{Name: "items", Pairs: []shadowsync.Pair{{ID: "p01", OldColumn: "old_value", NewColumn: "new_value", Forward: "lower(value)", Reverse: "upper(value)", Lossy: true}}}})
	if e != nil {
		t.Fatal(e)
	}
	sp := shadowsync.Policy{AllowLossy: true}
	sc := shadowsync.Config{URL: url, Target: "db", Environment: "test", LockTimeoutMS: 500}
	if _, e = shadowsync.Apply(ctx, sc, ss, sp); e != nil {
		t.Fatal(e)
	}
	s := spec(t, "new_value", "job_one")
	cfg := Config{URL: url, Schema: "bf_state", Target: "db", Environment: "test", BatchSize: 5, MaxRetries: 2, LockTimeoutMS: 200, StatementTimeoutMS: 1000, MaxRowsPerSecond: 1000, Backoff: time.Millisecond}
	first, e := RunBatch(ctx, cfg, s)
	if e != nil {
		t.Fatal(e)
	}
	if first.Processed != 5 || first.Remaining != 20 || first.LastBatchRows != 5 {
		t.Fatalf("unbounded first batch: %+v", first)
	}
	paused, e := Control(ctx, cfg, s, "pause")
	if e != nil || paused.State != "paused" {
		t.Fatalf("pause: %v %+v", e, paused)
	}
	again, e := RunBatch(ctx, cfg, s)
	if e != nil || again.Processed != 5 {
		t.Fatalf("paused work progressed: %v %+v", e, again)
	}
	if _, e = Control(ctx, cfg, s, "resume"); e != nil {
		t.Fatal(e)
	}
	// Application write to a not-yet-filled row wins and becomes ineligible.
	if _, e = c.Exec(ctx, `update bf_app.items set old_value='Concurrent' where id=25`); e != nil {
		t.Fatal(e)
	}
	done, e := Run(ctx, cfg, s)
	if e != nil {
		t.Fatal(e)
	}
	if done.State != "complete" || done.Remaining != 0 || done.Processed != 24 || done.ThroughputRowsPerSecond <= 0 {
		t.Fatalf("resume status: %+v", done)
	}
	var bad int
	if e = c.QueryRow(ctx, `select count(*) from bf_app.items where new_value is distinct from lower(old_value)`).Scan(&bad); e != nil || bad != 0 {
		t.Fatalf("corrupt backfill: %v %d", e, bad)
	}
	var source string
	if e = c.QueryRow(ctx, `select old_value from bf_app.items where id=1`).Scan(&source); e != nil || source != "Name01" {
		t.Fatalf("backfill rewrote source: %v %q", e, source)
	}
	// Durable status is read-only and contains aggregate telemetry, never row values.
	st, e := StatusOf(ctx, cfg, s)
	if e != nil {
		t.Fatal(e)
	}
	raw, _ := json.Marshal(st)
	if strings.Contains(string(raw), "Name") || st.Retries != 0 {
		t.Fatalf("status leak/retries: %s", raw)
	}
	// A second job can be cancelled durably after one bounded batch.
	cancelSpec := spec(t, "new_cancel", "job_cancel")
	one, e := RunBatch(ctx, cfg, cancelSpec)
	if e != nil || one.LastBatchRows != 5 {
		t.Fatalf("cancel setup: %v %+v", e, one)
	}
	cancelled, e := Control(ctx, cfg, cancelSpec, "cancel")
	if e != nil || cancelled.State != "cancelled" {
		t.Fatalf("cancel: %v %+v", e, cancelled)
	}
	same, e := RunBatch(ctx, cfg, cancelSpec)
	if e != nil || same.Processed != 5 {
		t.Fatalf("cancelled job progressed: %v %+v", e, same)
	}
}

func TestLiveRetriesAndRedactedError(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	ctx := context.Background()
	c, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close(ctx)
	for _, s := range []string{"bf_retry_state", "bf_retry"} {
		_, _ = c.Exec(ctx, "drop schema if exists "+q(s)+" cascade")
	}
	defer func() {
		for _, s := range []string{"bf_retry_state", "bf_retry"} {
			_, _ = c.Exec(context.Background(), "drop schema if exists "+q(s)+" cascade")
		}
	}()
	if _, e = c.Exec(ctx, `create schema bf_retry;create table bf_retry.items(id bigint primary key,old_value text,new_value integer);insert into bf_retry.items values(1,'secret-row-value',null)`); e != nil {
		t.Fatal(e)
	}
	s, e := New(strings.Repeat("b", 64), "job_retry", "bf_retry", "items", "id", "old_value", "new_value", "value::integer")
	if e != nil {
		t.Fatal(e)
	}
	cfg := Config{URL: url, Schema: "bf_retry_state", Target: "db", Environment: "test", BatchSize: 1, MaxRetries: 1, LockTimeoutMS: 100, StatementTimeoutMS: 500, Backoff: time.Millisecond}
	st, e := RunBatch(ctx, cfg, s)
	if e == nil {
		t.Fatal("bad transform succeeded")
	}
	if strings.Contains(e.Error(), "secret-row-value") || strings.Contains(st.LastError, "secret-row-value") || st.State != "failed" {
		t.Fatalf("error leaked row or wrong state: %v %+v", e, st)
	}
}

func TestLiveTransientRetryAndRecovery(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	ctx := context.Background()
	admin, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer admin.Close(ctx)
	for _, s := range []string{"bf_lock_state", "bf_lock"} {
		_, _ = admin.Exec(ctx, "drop schema if exists "+q(s)+" cascade")
	}
	defer func() {
		for _, s := range []string{"bf_lock_state", "bf_lock"} {
			_, _ = admin.Exec(context.Background(), "drop schema if exists "+q(s)+" cascade")
		}
	}()
	if _, e = admin.Exec(ctx, `create schema bf_lock;create table bf_lock.items(id bigint primary key,old_value text,new_value text);insert into bf_lock.items values(1,'A',null)`); e != nil {
		t.Fatal(e)
	}
	locker, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer locker.Close(ctx)
	tx, e := locker.Begin(ctx)
	if e != nil {
		t.Fatal(e)
	}
	if _, e = tx.Exec(ctx, `lock table bf_lock.items in access exclusive mode`); e != nil {
		t.Fatal(e)
	}
	s, e := New(strings.Repeat("c", 64), "job_lock", "bf_lock", "items", "id", "old_value", "new_value", "lower(value)")
	if e != nil {
		t.Fatal(e)
	}
	cfg := Config{URL: url, Schema: "bf_lock_state", Target: "db", Environment: "test", BatchSize: 1, MaxRetries: 1, LockTimeoutMS: 25, StatementTimeoutMS: 500, Backoff: time.Millisecond}
	st, e := RunBatch(ctx, cfg, s)
	if e == nil || st.State != "failed" || st.Retries != 1 {
		t.Fatalf("transient retry evidence: %v %+v", e, st)
	}
	if e = tx.Rollback(ctx); e != nil {
		t.Fatal(e)
	}
	if _, e = Control(ctx, cfg, s, "resume"); e != nil {
		t.Fatal(e)
	}
	done, e := Run(ctx, cfg, s)
	if e != nil || done.State != "complete" || done.Processed != 1 {
		t.Fatalf("recovery: %v %+v", e, done)
	}
}
