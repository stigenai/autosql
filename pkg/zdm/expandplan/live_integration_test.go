package expandplan_test

import (
	"context"
	"crypto/ed25519"
	"errors"
	"fmt"
	neturl "net/url"
	"os"
	"strings"
	"testing"
	"time"

	"autosql/pkg/zdm"
	"autosql/pkg/zdm/expandplan"
	"autosql/pkg/zerodowntime"
	"github.com/jackc/pgx/v5"
)

func livePolicy() expandplan.Policy {
	return expandplan.Policy{MaxLockMS: 100, MaxLockHoldMS: 5000, MaxStatementMS: 5000, MaxTransactionMS: 5000}
}

func live(t *testing.T) (context.Context, *pgx.Conn, string, string, *zdm.Store) {
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
	if len(suffix) > 18 {
		suffix = suffix[len(suffix)-18:]
	}
	meta := fmt.Sprintf("ep_%d_%s", time.Now().UnixNano()%1e7, suffix)
	app := meta + "_app"
	q := func(s ...string) string { return pgx.Identifier(s).Sanitize() }
	if _, err = c.Exec(ctx, "create schema "+q(app)+"; create table "+q(app, "accounts")+"(id bigint primary key,name text); insert into "+q(app, "accounts")+" values(1,'a')"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.Exec(context.Background(), "drop schema if exists "+q(meta)+" cascade; drop schema if exists "+q(app)+" cascade")
		c.Close(context.Background())
	})
	s, err := zdm.Open(zdm.Config{URL: url, Schema: meta})
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Baseline(ctx, zdm.BaselineRequest{ID: "adopt", Target: "primary", Environment: "test", Operator: "tester", Schemas: []string{app}}); err != nil {
		t.Fatal(err)
	}
	return ctx, c, meta, app, s
}

func TestLivePlanningIsReadOnlyAndFencedToBaseline(t *testing.T) {
	ctx, c, meta, app, _ := live(t)
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	var before int
	if err := c.QueryRow(ctx, `select count(*) from pg_catalog.pg_class c join pg_catalog.pg_namespace n on n.oid=c.relnamespace where n.nspname=any($1)`, []string{meta, app}).Scan(&before); err != nil {
		t.Fatal(err)
	}
	snap, err := expandplan.InspectLive(ctx, expandplan.InspectRequest{URL: url, MetadataSchema: meta, Target: "primary", Environment: "test", ArtifactDigest: "pending", Schemas: []string{app}, Policy: livePolicy()})
	if err != nil {
		t.Fatal(err)
	}
	var after int
	if err = c.QueryRow(ctx, `select count(*) from pg_catalog.pg_class c join pg_catalog.pg_namespace n on n.oid=c.relnamespace where n.nspname=any($1)`, []string{meta, app}).Scan(&after); err != nil || after != before {
		t.Fatalf("planning mutated catalog before=%d after=%d err=%v", before, after, err)
	}
	e, _ := zerodowntime.AddColumnEffects("none")
	m, err := zerodowntime.New("v2", zerodowntime.VersionSchema{Name: "v2", ExposeDuringExpand: true}, zerodowntime.Requirements{MinimumPostgres: 14, LockTimeoutMS: 100, StatementTimeoutMS: 1000}, []zerodowntime.Operation{{ID: "01", Kind: zerodowntime.AddColumn, Table: "accounts", Column: "email", DataType: "text", SynchronizationMode: "none", Effects: e, Reversal: zerodowntime.Reversal{Mode: "automatic"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	if err = m.Sign("release", priv); err != nil {
		t.Fatal(err)
	}
	snap.ArtifactDigest = m.Digest
	p, err := expandplan.Build(expandplan.Request{Migration: m, Snapshot: snap, ExpectedFingerprint: snap.Fingerprint, Target: "primary", Environment: "test", Policy: expandplan.Policy{MaxLockMS: 100, MaxLockHoldMS: 1000, MaxStatementMS: 1000, MaxTransactionMS: 2000}, Verify: func(m zerodowntime.Migration) error { return m.Verify(pub) }})
	if err != nil || len(p.Steps) != 1 {
		t.Fatalf("plan=%+v err=%v", p, err)
	}
	var exists bool
	if err = c.QueryRow(ctx, `select exists(select 1 from pg_catalog.pg_attribute a join pg_catalog.pg_class c on c.oid=a.attrelid join pg_catalog.pg_namespace n on n.oid=c.relnamespace where n.nspname=$1 and c.relname='accounts' and a.attname='email')`, app).Scan(&exists); err != nil || exists {
		t.Fatalf("dry run created column exists=%v err=%v", exists, err)
	}
	if _, err = c.Exec(ctx, "alter table "+pgx.Identifier{app, "accounts"}.Sanitize()+" add column drifted boolean"); err != nil {
		t.Fatal(err)
	}
	if _, err = expandplan.InspectLive(ctx, expandplan.InspectRequest{URL: url, MetadataSchema: meta, Target: "primary", Environment: "test", ArtifactDigest: m.Digest, Schemas: []string{app}, Policy: livePolicy()}); err == nil {
		t.Fatal("expected baseline drift refusal")
	}
}

func TestLiveConcurrentDDLWaitsAndPlanRefusesFreshSnapshot(t *testing.T) {
	ctx, c, meta, app, _ := live(t)
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	other, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = other.Close(context.Background()) })
	var pid int
	if err = other.QueryRow(ctx, "select pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	ddlCtx, cancelDDL := context.WithCancel(context.Background())
	hook := func() error {
		go func() {
			_, e := other.Exec(ddlCtx, "alter table "+pgx.Identifier{app, "accounts"}.Sanitize()+" add column concurrent_ddl text")
			done <- e
		}()
		deadline := time.Now().Add(5 * time.Second)
		for time.Now().Before(deadline) {
			var waiting bool
			e := c.QueryRow(ctx, `select coalesce(wait_event_type='Lock',false) from pg_catalog.pg_stat_activity where pid=$1`, pid).Scan(&waiting)
			if e == nil && waiting {
				return nil
			}
			time.Sleep(10 * time.Millisecond)
		}
		return fmt.Errorf("concurrent DDL did not wait on planning relation lock")
	}
	_, err = expandplan.InspectLive(ctx, expandplan.InspectRequest{URL: url, MetadataSchema: meta, Target: "primary", Environment: "test", ArtifactDigest: "sha256:artifact", Schemas: []string{app}, BeforeFinalInspection: hook, Policy: livePolicy()})
	if err == nil {
		t.Fatal("concurrent DDL produced a stale plan")
	}
	// Cancel the in-flight command, await its goroutine, and only then allow the
	// connection cleanup to close the socket. Closing while Exec is active races
	// inside pgx and does not model production cancellation.
	cancelDDL()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("concurrent DDL goroutine did not terminate")
	}
	_ = other.Close(ctx)
}

func TestLivePlanningLockTimeoutIsActuallyBounded(t *testing.T) {
	ctx, _, meta, app, _ := live(t)
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	blocker, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer blocker.Close(ctx)
	tx, err := blocker.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "lock table "+pgx.Identifier{app, "accounts"}.Sanitize()+" in access exclusive mode"); err != nil {
		t.Fatal(err)
	}
	policy := livePolicy()
	policy.MaxLockMS = 25
	started := time.Now()
	_, err = expandplan.InspectLive(ctx, expandplan.InspectRequest{URL: url, MetadataSchema: meta, Target: "primary", Environment: "test", ArtifactDigest: "sha256:held", Schemas: []string{app}, Policy: policy})
	elapsed := time.Since(started)
	if err == nil || elapsed > time.Second || !strings.Contains(strings.ToLower(err.Error()), "lock timeout") {
		t.Fatalf("bounded refusal elapsed=%v err=%v", elapsed, err)
	}
}

func TestLiveSearchPathHijackIsNeutralizedByRequiredSessionSetup(t *testing.T) {
	ctx, c, _, app, _ := live(t)
	attacker := app + "_attacker"
	q := func(s ...string) string { return pgx.Identifier(s).Sanitize() }
	if _, err := c.Exec(ctx, "create schema "+q(attacker)+"; create function "+q(attacker, "lower")+"(text) returns text language sql immutable as 'select ''hijacked''::text'"); err != nil {
		t.Fatal(err)
	}
	defer c.Exec(context.Background(), "drop schema if exists "+q(attacker)+" cascade")
	tx, err := c.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	if _, err = tx.Exec(ctx, "set local search_path="+q(attacker)+",pg_catalog"); err != nil {
		t.Fatal(err)
	}
	var got string
	if err = tx.QueryRow(ctx, "select lower('X')").Scan(&got); err != nil || got != "hijacked" {
		t.Fatalf("hijack proof got=%q err=%v", got, err)
	}
	if _, err = tx.Exec(ctx, "set local search_path=pg_catalog"); err != nil {
		t.Fatal(err)
	}
	if err = tx.QueryRow(ctx, "select lower('X')").Scan(&got); err != nil || got != "x" {
		t.Fatalf("trusted search_path got=%q err=%v", got, err)
	}
}

func TestLiveNonSuperuserIndexPlanningMatchesSchemaPermissionRefusal(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	admin, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e7)
	role := "ep_role_" + suffix
	meta := "ep_meta_" + suffix
	app := "ep_app_" + suffix
	q := func(s ...string) string { return pgx.Identifier(s).Sanitize() }
	if _, err = admin.Exec(ctx, "create role "+q(role)+" login password 'ep_password'; create schema "+q(app)+"; revoke create on schema "+q(app)+" from public; create table "+q(app, "items")+"(id bigint primary key,name text); alter table "+q(app, "items")+" owner to "+q(role)+"; grant usage on schema "+q(app)+" to "+q(role)+"; revoke create on schema "+q(app)+" from "+q(role)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = admin.Exec(context.Background(), "drop schema if exists "+q(meta)+" cascade; drop schema if exists "+q(app)+" cascade; drop role if exists "+q(role))
	})
	store, err := zdm.Open(zdm.Config{URL: url, Schema: meta})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = store.Baseline(ctx, zdm.BaselineRequest{ID: "adopt", Target: "primary", Environment: "test", Operator: "admin", Schemas: []string{app}}); err != nil {
		t.Fatal(err)
	}
	if _, err = admin.Exec(ctx, "grant usage on schema "+q(meta)+" to "+q(role)+"; grant select on all tables in schema "+q(meta)+" to "+q(role)); err != nil {
		t.Fatal(err)
	}
	u, err := neturl.Parse(url)
	if err != nil {
		t.Fatal(err)
	}
	u.User = neturl.UserPassword(role, "ep_password")
	roleURL := u.String()
	roleConn, err := pgx.Connect(ctx, roleURL)
	if err != nil {
		t.Fatal(err)
	}
	defer roleConn.Close(ctx)
	var connectedAs string
	var super bool
	if err = roleConn.QueryRow(ctx, `select current_user,r.rolsuper from pg_catalog.pg_roles r where r.rolname=current_user`).Scan(&connectedAs, &super); err != nil || connectedAs != role || super {
		t.Fatalf("non-superuser connection identity=%s super=%v err=%v", connectedAs, super, err)
	}
	if _, err = roleConn.Exec(ctx, "create index "+q(app, "should_refuse")+" on "+q(app, "items")+"(name)"); err == nil {
		t.Fatal("database unexpectedly allowed CREATE INDEX without schema CREATE")
	}
	snap, err := expandplan.InspectLive(ctx, expandplan.InspectRequest{URL: roleURL, MetadataSchema: meta, Target: "primary", Environment: "test", ArtifactDigest: "pending", Schemas: []string{app}, Policy: livePolicy()})
	if err != nil {
		t.Fatal(err)
	}
	if snap.SchemaCreate[app] {
		t.Fatal("schema CREATE privilege incorrectly reported")
	}
	e, _ := zerodowntime.Effects(zerodowntime.CreateIndex)
	m, err := zerodowntime.New("v2", zerodowntime.VersionSchema{Name: "v2", ExposeDuringExpand: true}, zerodowntime.Requirements{MinimumPostgres: 14, LockTimeoutMS: 100, StatementTimeoutMS: 1000}, []zerodowntime.Operation{{ID: "01", Kind: zerodowntime.CreateIndex, Table: "items", Index: "items_name_idx", Expression: "name", IndexMode: &zerodowntime.IndexMode{Concurrent: true}, Effects: e, Reversal: zerodowntime.Reversal{Mode: "automatic"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	snap.ArtifactDigest = m.Digest
	if _, err = expandplan.Build(expandplan.Request{Migration: m, Snapshot: snap, ExpectedFingerprint: snap.Fingerprint, Target: "primary", Environment: "test", Policy: expandplan.Policy{MaxLockMS: 100, MaxLockHoldMS: 1000, MaxStatementMS: 1000, MaxTransactionMS: 1000, AllowTableScan: true, AllowNonTransactional: true}, Verify: func(zerodowntime.Migration) error { return nil }}); !errors.Is(err, expandplan.ErrRefused) || !strings.Contains(err.Error(), "CREATE privilege") {
		t.Fatalf("planner refusal=%v", err)
	}
}
