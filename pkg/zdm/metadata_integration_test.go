package zdm

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func liveStore(t *testing.T) (*Store, *pgx.Conn, string, string) {
	t.Helper()
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	suffix := strings.ToLower(strings.NewReplacer("/", "_", "-", "_").Replace(t.Name()))
	if len(suffix) > 24 {
		suffix = suffix[len(suffix)-24:]
	}
	n := fmt.Sprintf("zdm_%d_%s", time.Now().UnixNano()%1e8, suffix)
	app := n + "_app"
	_, err = c.Exec(ctx, "create schema "+q(app)+`; create table `+q(app, "accounts")+`(id bigint primary key,balance integer not null); insert into `+q(app, "accounts")+` values(7,42)`)
	if err != nil {
		c.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.Exec(context.Background(), "drop schema if exists "+q(n)+" cascade; drop schema if exists "+q(app)+" cascade")
		c.Close(context.Background())
	})
	s, err := Open(Config{URL: url, Schema: n})
	if err != nil {
		t.Fatal(err)
	}
	return s, c, n, app
}

func TestLiveInitConcurrentIdempotentAndIncompatible(t *testing.T) {
	s, c, n, _ := liveStore(t)
	ctx := context.Background()
	var wg sync.WaitGroup
	errs := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errs <- s.Init(ctx) }()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	st, err := s.Status(ctx)
	if err != nil || !st.Initialized || st.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("status=%+v err=%v", st, err)
	}
	var count int
	if err = c.QueryRow(ctx, `select count(*) from `+q(n, "meta")).Scan(&count); err != nil || count != 1 {
		t.Fatalf("meta rows=%d err=%v", count, err)
	}
	_, err = c.Exec(ctx, `update `+q(n, "meta")+` set schema_version=999`)
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.Status(ctx)
	if !errors.Is(err, ErrIncompatible) {
		t.Fatalf("want incompatible, got %v", err)
	}
}

func TestLiveInitCrashRollsBackAndRetryRecovers(t *testing.T) {
	s, c, n, _ := liveStore(t)
	ctx := context.Background()
	boom := errors.New("simulated crash")
	err := s.InitWithHooks(ctx, InitHooks{AfterVersion: func(v int) error {
		if v == 2 {
			return boom
		}
		return nil
	}})
	if !errors.Is(err, boom) {
		t.Fatalf("want crash, got %v", err)
	}
	var exists bool
	if err = c.QueryRow(ctx, `select exists(select 1 from pg_namespace where nspname=$1)`, n).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	if exists {
		t.Fatal("partial namespace survived rollback")
	}
	if err = s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := s.Status(ctx)
	if err != nil || st.SchemaVersion != CurrentSchemaVersion {
		t.Fatalf("retry status=%+v err=%v", st, err)
	}
}

func TestLiveExplicitUpgradeFromSupportedV1AndCorruptLayoutRefusal(t *testing.T) {
	s, c, n, _ := liveStore(t)
	ctx := context.Background()
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `create schema `+q(n)); err != nil {
		t.Fatal(err)
	}
	if err = s.createV1(ctx, tx); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	st, err := s.Status(ctx)
	if err != nil || st.SchemaVersion != 2 {
		t.Fatalf("upgraded status=%+v err=%v", st, err)
	}
	if _, err = c.Exec(ctx, `alter table `+q(n, "baselines")+` drop column fingerprint`); err != nil {
		t.Fatal(err)
	}
	_, err = s.Status(ctx)
	if !errors.Is(err, ErrCorrupt) {
		t.Fatalf("corrupt layout accepted: %v", err)
	}
}

func TestLiveLeastPrivilegeRoleCanInitializeAndBaseline(t *testing.T) {
	s, admin, n, app := liveStore(t)
	_ = s
	ctx := context.Background()
	role := n + "_role"
	var db, current string
	if err := admin.QueryRow(ctx, `select current_database(),current_user`).Scan(&db, &current); err != nil {
		t.Fatal(err)
	}
	_, err := admin.Exec(ctx, `create role `+q(role)+` nologin nosuperuser nocreatedb nocreaterole noinherit; grant `+q(role)+` to `+q(current)+`; grant create on database `+q(db)+` to `+q(role)+`; grant usage on schema `+q(app)+` to `+q(role)+`; grant select on all tables in schema `+q(app)+` to `+q(role))
	if err != nil {
		t.Skipf("test role setup requires CREATEROLE: %v", err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), `revoke `+q(role)+` from `+q(current)+`; drop owned by `+q(role)+`; drop role `+q(role))
	})
	u, err := url.Parse(os.Getenv("AUTOSQL_TEST_POSTGRES_URL"))
	if err != nil {
		t.Fatal(err)
	}
	values := u.Query()
	values.Set("options", "-c role="+role)
	u.RawQuery = values.Encode()
	limited, err := Open(Config{URL: u.String(), Schema: n})
	if err != nil {
		t.Fatal(err)
	}
	if err = limited.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = limited.Baseline(ctx, BaselineRequest{ID: "least", Target: "least-target", Environment: "test", Operator: "limited-role", Schemas: []string{app}}); err != nil {
		t.Fatal(err)
	}
	var super, createDB, createRole bool
	if err = admin.QueryRow(ctx, `select rolsuper,rolcreatedb,rolcreaterole from pg_roles where rolname=$1`, role).Scan(&super, &createDB, &createRole); err != nil {
		t.Fatal(err)
	}
	if super || createDB || createRole {
		t.Fatal("test role had elevated privileges")
	}
}

func TestLiveBaselineRetryDriftTamperIsolationAndNoMutation(t *testing.T) {
	s, c, n, app := liveStore(t)
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	req := BaselineRequest{ID: "adopt-1", Target: "customer-primary", Environment: "production", Operator: "release-bot", Schemas: []string{app}}
	b, err := s.Baseline(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	if b.Fingerprint == "" || len(b.CanonicalSchema) == 0 {
		t.Fatalf("incomplete evidence: %+v", b)
	}
	var balance int
	if err = c.QueryRow(ctx, `select balance from `+q(app, "accounts")+` where id=7`).Scan(&balance); err != nil || balance != 42 {
		t.Fatalf("user data changed: %d %v", balance, err)
	}
	again, err := s.Baseline(ctx, req)
	if err != nil || again.Fingerprint != b.Fingerprint {
		t.Fatalf("retry=%+v err=%v", again, err)
	}
	_, err = s.Baseline(ctx, BaselineRequest{ID: "other", Target: req.Target, Environment: req.Environment, Operator: req.Operator, Schemas: req.Schemas})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("target binding accepted: %v", err)
	}
	if _, err = c.Exec(ctx, `alter table `+q(app, "accounts")+` add column note text`); err != nil {
		t.Fatal(err)
	}
	_, err = s.Baseline(ctx, req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("drift accepted: %v", err)
	}
	if _, err = c.Exec(ctx, `update `+q(n, "baselines")+` set canonical_schema='{}'::jsonb`); err != nil {
		t.Fatal(err)
	}
	_, err = s.Baseline(ctx, req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("tamper accepted: %v", err)
	}
}

func TestLiveBaselineRefusesActiveAndMismatchAtomically(t *testing.T) {
	s, c, n, app := liveStore(t)
	ctx := context.Background()
	if err := s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	req := BaselineRequest{ID: "b", Target: "t", Environment: "e", Operator: "o", Schemas: []string{app}, ExpectedFingerprint: "wrong"}
	_, err := s.Baseline(ctx, req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("mismatch accepted: %v", err)
	}
	var count int
	_ = c.QueryRow(ctx, `select count(*) from `+q(n, "baselines")).Scan(&count)
	if count != 0 {
		t.Fatal("failed baseline was not atomic")
	}
	_, _ = c.Exec(ctx, `update `+q(n, "meta")+` set active_version='v2'`)
	req.ExpectedFingerprint = ""
	_, err = s.Baseline(ctx, req)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("active state accepted: %v", err)
	}
}
