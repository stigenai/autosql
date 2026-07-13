package rollback

import (
	"context"
	"errors"
	"github.com/jackc/pgx/v5"
	"os"
	"strings"
	"testing"
	"time"
)

func TestLiveRapidRollbackKeepsPreviousWritableAndBackfill(t *testing.T) {
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
	for _, n := range []string{"rb_state", "rb_old", "rb_new", "rb_app"} {
		_, _ = c.Exec(ctx, "drop schema if exists "+q(n)+" cascade")
	}
	defer func() {
		for _, n := range []string{"rb_state", "rb_old", "rb_new", "rb_app"} {
			_, _ = c.Exec(context.Background(), "drop schema if exists "+q(n)+" cascade")
		}
	}()
	_, e = c.Exec(ctx, `create schema rb_app;create table rb_app.items(id bigint primary key,old_name text,new_name text);insert into rb_app.items values(1,'ALICE','alice');create schema rb_old;create view rb_old.items as select id,old_name as name from rb_app.items;create schema rb_new;create view rb_new.items as select id,new_name as name from rb_app.items;create function rb_app.sync() returns trigger language plpgsql as 'begin new.new_name=lower(new.old_name);return new;end';create trigger sync before insert or update on rb_app.items for each row execute function rb_app.sync()`)
	if e != nil {
		t.Fatal(e)
	}
	s, _ := New("release_two", strings.Repeat("a", 64), "rb_old", "rb_new")
	cfg := Config{URL: url, Schema: "rb_state", Target: "primary", Environment: "test", LockTimeoutMS: 500}
	auth := Authorization{Operator: "dba", Reason: "new release unhealthy", AcknowledgeLossy: true, At: time.Now().UTC()}
	elig := func(context.Context) (Eligibility, error) {
		return Eligibility{Active: true, PreviousWritable: true, Lossy: true}, nil
	}
	actions := Actions{WithdrawNew: func(ctx context.Context) error { _, e := c.Exec(ctx, "drop schema if exists rb_new cascade"); return e }, RemoveCompatibility: func(ctx context.Context) error {
		_, e := c.Exec(ctx, "drop trigger if exists sync on rb_app.items;drop function if exists rb_app.sync()")
		return e
	}, VerifyPrevious: func(ctx context.Context) error {
		_, e := c.Exec(ctx, "insert into rb_old.items values(2,'BOB')")
		return e
	}}
	blocked := auth
	blocked.AcknowledgeLossy = false
	st, e := Rollback(ctx, cfg, s, elig, actions, blocked)
	if !errors.Is(e, ErrRefused) || st.State != "blocked" {
		t.Fatalf("lossy rollback not blocked: %+v %v", st, e)
	}
	st, e = Rollback(ctx, cfg, s, elig, actions, auth)
	if e != nil {
		t.Fatal(e)
	}
	if st.State != "complete" {
		t.Fatalf("not complete: %+v", st)
	}
	var old, newv string
	if e = c.QueryRow(ctx, "select old_name,new_name from rb_app.items where id=1").Scan(&old, &newv); e != nil || old != "ALICE" || newv != "alice" {
		t.Fatalf("backfill reversed: %q %q %v", old, newv, e)
	}
	if e = c.QueryRow(ctx, "select name from rb_old.items where id=2").Scan(&old); e != nil || old != "BOB" {
		t.Fatalf("old version not writable: %q %v", old, e)
	}
	if _, e = Rollback(ctx, cfg, s, func(context.Context) (Eligibility, error) { return Eligibility{}, nil }, actions, auth); e != nil {
		t.Fatalf("completed rollback retry failed: %v", e)
	}
}
func TestLiveRepairIsExplicitAppendOnlyAndVerified(t *testing.T) {
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
	_, _ = c.Exec(ctx, "drop schema if exists repair_state cascade;drop schema if exists repair_app cascade")
	defer c.Exec(context.Background(), "drop schema if exists repair_state cascade;drop schema if exists repair_app cascade")
	_, e = c.Exec(ctx, "create schema repair_app;create table repair_app.marker(value text);insert into repair_app.marker values('drifted')")
	if e != nil {
		t.Fatal(e)
	}
	cfg := Config{URL: url, Schema: "repair_state", Target: "primary", Environment: "test", LockTimeoutMS: 500}
	p, _ := NewRepair("restore_marker", "subject", "drifted", "set marker to expected", "expected")
	verify := func(ctx context.Context) (string, error) {
		var x string
		e := c.QueryRow(ctx, "select value from repair_app.marker").Scan(&x)
		return x, e
	}
	a := Authorization{Operator: "dba", Reason: "restore exact owned marker", At: time.Now().UTC()}
	if e = Repair(ctx, cfg, p, a, func(ctx context.Context) error {
		_, e := c.Exec(ctx, "update repair_app.marker set value='expected'")
		return e
	}, verify); e != nil {
		t.Fatal(e)
	}
	var count int
	if e = c.QueryRow(ctx, "select count(*) from repair_state.audit where subject_digest=$1", p.Digest).Scan(&count); e != nil || count != 2 {
		t.Fatalf("audit not append-only complete: %d %v", count, e)
	}
	if e = Repair(ctx, cfg, p, a, func(context.Context) error { return nil }, verify); !errors.Is(e, ErrRefused) {
		t.Fatalf("stale repair silently rewrote history: %v", e)
	}
}
