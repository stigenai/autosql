package contract

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"os"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

func TestLiveGatesRecoveryAndIdempotentCompletion(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	c, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close(ctx)
	_, _ = c.Exec(ctx, "drop schema if exists ct_state cascade;drop schema if exists ct_old cascade;drop schema if exists ct_app cascade")
	defer c.Exec(context.Background(), "drop schema if exists ct_state cascade;drop schema if exists ct_old cascade;drop schema if exists ct_app cascade")
	if _, e = c.Exec(ctx, "create schema ct_old;create schema ct_app;create table ct_app.items(id bigint primary key,old_name text,new_name text);create function ct_app.compat() returns trigger language plpgsql as 'begin return new; end';create trigger compat before update on ct_app.items for each row execute function ct_app.compat()"); e != nil {
		t.Fatal(e)
	}
	s, e := New("release_two", strings.Repeat("b", 64), "v1", "v2", []Step{{ID: "drop_old_schema", Summary: "withdraw old version", SQL: "drop schema ct_old", CheckSQL: "select to_regnamespace('ct_old') is null", Recovery: "drop ct_old if it still exists", Transactional: true}, {ID: "remove_trigger", Summary: "remove compatibility trigger", SQL: "drop trigger compat on ct_app.items", CheckSQL: "select not exists(select 1 from pg_trigger where tgname='compat' and not tgisinternal)", Recovery: "inspect and remove the marked trigger", Transactional: true}})
	if e != nil {
		t.Fatal(e)
	}
	cfg := Config{URL: url, Schema: "ct_state", Target: "primary", Environment: "test", LockTimeoutMS: 500}
	now := time.Now().UTC()
	approval := Approval{PlanDigest: s.Digest, Approver: "dba", Reason: "old clients drained", ApprovedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}
	blocked := Evidence{OldVersionSessions: 1, BackfillsComplete: true, ChecksPassed: true, DriftFree: true}
	var calls int32
	exec := func(ctx context.Context, x Step) error {
		atomic.AddInt32(&calls, 1)
		_, e := c.Exec(ctx, x.SQL)
		return e
	}
	verify := func(ctx context.Context) error {
		var old, tr bool
		if e := c.QueryRow(ctx, `select to_regnamespace('ct_old') is not null,exists(select 1 from pg_trigger where tgname='compat' and not tgisinternal)`).Scan(&old, &tr); e != nil {
			return e
		}
		if old || tr {
			return errors.New("orphan compatibility objects")
		}
		return nil
	}
	st, e := Complete(ctx, cfg, s, func(context.Context) (Evidence, error) { return blocked, nil }, exec, verify, approval)
	if !errors.Is(e, ErrRefused) || st.State != "blocked" || len(st.Blockers) == 0 || atomic.LoadInt32(&calls) != 0 {
		t.Fatalf("gates mutated: %+v %v calls=%d", st, e, calls)
	}
	ready := Evidence{BackfillsComplete: true, ChecksPassed: true, DriftFree: true}
	st, e = Complete(ctx, cfg, s, func(context.Context) (Evidence, error) { return ready, nil }, exec, verify, approval)
	if e != nil {
		t.Fatal(e)
	}
	if st.State != "complete" || st.CompletedSteps != 2 {
		t.Fatalf("bad completion: %+v", st)
	}
	if _, e = Complete(ctx, cfg, s, func(context.Context) (Evidence, error) { return ready, nil }, exec, verify, approval); e != nil {
		t.Fatal(e)
	}
	if calls != 2 {
		t.Fatalf("retry duplicated work: %d", calls)
	}
}

func TestLiveInterruptedStepIsRecoverable(t *testing.T) {
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
	_, _ = c.Exec(ctx, "drop schema if exists ct_retry cascade")
	defer c.Exec(context.Background(), "drop schema if exists ct_retry cascade")
	s, _ := New("retry_release", strings.Repeat("c", 64), "v1", "v2", []Step{{ID: "one", Summary: "one", SQL: "create table ct_retry.done(id int)", CheckSQL: "select to_regclass('ct_retry.done') is not null", Recovery: "retry one", Transactional: true}})
	cfg := Config{URL: url, Schema: "ct_retry", Target: "primary", Environment: "test", LockTimeoutMS: 500}
	now := time.Now().UTC()
	a := Approval{PlanDigest: s.Digest, Approver: "dba", Reason: "approved", ApprovedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour)}
	ev := func(context.Context) (Evidence, error) {
		return Evidence{BackfillsComplete: true, ChecksPassed: true, DriftFree: true}, nil
	}
	boom := errors.New("boom")
	st, e := Complete(ctx, cfg, s, ev, func(context.Context, Step) error { return boom }, func(context.Context) error { return nil }, a)
	if !errors.Is(e, boom) || st.State != "interrupted" || len(st.RecoveryActions) == 0 {
		t.Fatalf("not recoverable: %+v %v", st, e)
	}
	st, e = Complete(ctx, cfg, s, ev, func(ctx context.Context, step Step) error { _, err := c.Exec(ctx, step.SQL); return err }, func(context.Context) error { return nil }, a)
	if e != nil || st.State != "complete" {
		t.Fatalf("retry failed: %+v %v", st, e)
	}
}
