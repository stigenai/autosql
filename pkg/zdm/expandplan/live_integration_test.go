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
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = c.Close(cleanup)
	})
	if _, err = c.Exec(ctx, `select pg_catalog.pg_advisory_lock(pg_catalog.hashtextextended('autosql.expandplan.live-tests/v1',0::bigint))`); err != nil {
		t.Fatal(err)
	}
	suffix := strings.ToLower(strings.NewReplacer("/", "_", "-", "_").Replace(t.Name()))
	if len(suffix) > 18 {
		suffix = suffix[len(suffix)-18:]
	}
	meta := fmt.Sprintf("ep_%d_%s", time.Now().UnixNano()%1e7, suffix)
	app := meta + "_app"
	q := func(s ...string) string { return pgx.Identifier(s).Sanitize() }
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := c.Exec(cleanup, "drop schema if exists "+q(meta)+" cascade; drop schema if exists "+q(app)+" cascade"); err != nil {
			t.Error(err)
		}
	})
	if _, err = c.Exec(ctx, "create schema "+q(app)+"; create table "+q(app, "accounts")+"(id bigint primary key,name text); insert into "+q(app, "accounts")+" values(1,'a')"); err != nil {
		t.Fatal(err)
	}
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
	url := trackedPlanningURL(t, c, os.Getenv("AUTOSQL_TEST_POSTGRES_URL"))
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
	domain, err := expandplan.PlanningAdvisoryDomain(meta, "primary", "test")
	if err != nil {
		t.Fatal(err)
	}
	if p.MetadataSchema != meta {
		t.Fatalf("metadata binding=%q", p.MetadataSchema)
	}
	matched := false
	for _, l := range p.PlanningLocks {
		if l.Kind == "advisory" {
			matched = true
			if l.Object != domain {
				t.Fatalf("reported advisory=%q acquired=%q", l.Object, domain)
			}
		}
	}
	if !matched {
		t.Fatal("reported advisory lock missing")
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

func TestLiveAdvisoryDomainExactlyCoordinatesMetadataSchema(t *testing.T) {
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
	domain, err := expandplan.PlanningAdvisoryDomain(meta, "primary", "test")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.Exec(ctx, `select pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended($1,0::bigint))`, domain); err != nil {
		t.Fatal(err)
	}
	policy := livePolicy()
	policy.MaxLockMS = 25
	_, err = expandplan.InspectLive(ctx, expandplan.InspectRequest{URL: url, MetadataSchema: meta, Target: "primary", Environment: "test", ArtifactDigest: "sha256:domain", Schemas: []string{app}, Policy: policy})
	if err == nil || !strings.Contains(strings.ToLower(err.Error()), "lock timeout") {
		t.Fatalf("canonical advisory domain did not coordinate: %v", err)
	}
	otherDomain, e := expandplan.PlanningAdvisoryDomain(meta+"_other", "primary", "test")
	if e != nil {
		t.Fatal(e)
	}
	if otherDomain == domain {
		t.Fatal("metadata domains collide")
	}
}

func TestLiveConcurrentDDLWaitsAndPlanRefusesFreshSnapshot(t *testing.T) {
	ctx, c, meta, app, _ := live(t)
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	hook := queuedDDLHook(t, ctx, c, url, app, "accounts")
	_, err := expandplan.InspectLive(ctx, expandplan.InspectRequest{URL: trackedPlanningURL(t, c, url), MetadataSchema: meta, Target: "primary", Environment: "test", ArtifactDigest: "sha256:artifact", Schemas: []string{app}, BeforeFinalInspection: hook, Policy: livePolicy()})
	if !errors.Is(err, expandplan.ErrRefused) || !strings.Contains(err.Error(), "conflicting relation change queued") {
		t.Fatalf("concurrent DDL was not refused at the queued-change boundary: %v", err)
	}
}

func queuedDDLHook(t *testing.T, ctx context.Context, c *pgx.Conn, url, app, relation string) func(context.Context) error {
	t.Helper()
	other, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	ddlCtx, cancelDDL := context.WithCancel(context.Background())
	finished := make(chan struct{})
	started := false
	// Register cancellation and joining before any assertion can terminate the
	// test. Closing a connection during Exec races inside pgx.
	t.Cleanup(func() {
		cancelDDL()
		if started {
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Error("concurrent DDL goroutine did not terminate")
				return
			}
		}
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = other.Close(cleanup)
	})
	var pid int
	if err = other.QueryRow(ctx, "select pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	return func(hold context.Context) error {
		started = true
		go func() {
			defer close(finished)
			_, _ = other.Exec(ddlCtx, "alter table "+pgx.Identifier{app, relation}.Sanitize()+" add column concurrent_ddl text")
		}()
		return waitRelationLock(hold, c, pid, app, relation)
	}
}

// Every connection opened by InspectLive inherits this application name. Check
// its sessions are gone on all exits; session closure releases transaction locks.
func trackedPlanningURL(t *testing.T, observer *pgx.Conn, raw string) string {
	t.Helper()
	u, err := neturl.Parse(raw)
	if err != nil {
		t.Fatal(err)
	}
	name := fmt.Sprintf("autosql-planning-%d", time.Now().UnixNano())
	query := u.Query()
	query.Set("application_name", name)
	u.RawQuery = query.Encode()
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		ticker := time.NewTicker(5 * time.Millisecond)
		defer ticker.Stop()
		for {
			var count int
			if err := observer.QueryRow(ctx, `select count(*) from pg_catalog.pg_stat_activity where application_name=$1`, name).Scan(&count); err != nil {
				t.Error(err)
				return
			}
			if count == 0 {
				return
			}
			select {
			case <-ctx.Done():
				t.Errorf("planning leaked %d sessions", count)
				return
			case <-ticker.C:
			}
		}
	})
	return u.String()
}

func waitRelationLock(ctx context.Context, c *pgx.Conn, pid int, namespace, relation string) error {
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		var waiting bool
		err := c.QueryRow(wait, `select exists(select 1 from pg_catalog.pg_locks l join pg_catalog.pg_class r on r.oid=l.relation join pg_catalog.pg_namespace n on n.oid=r.relnamespace where l.database=(select oid from pg_catalog.pg_database where datname=current_database()) and l.pid=$1 and n.nspname=$2 and r.relname=$3 and l.locktype='relation' and l.mode='AccessExclusiveLock' and not l.granted)`, pid, namespace, relation).Scan(&waiting)
		if err != nil {
			return err
		}
		if waiting {
			return nil
		}
		select {
		case <-wait.Done():
			return fmt.Errorf("concurrent DDL did not queue against the inspected relation: %w", wait.Err())
		case <-ticker.C:
		}
	}
}

func TestLivePlanningLockTimeoutIsActuallyBounded(t *testing.T) {
	ctx, c, meta, app, _ := live(t)
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
	_, err = expandplan.InspectLive(ctx, expandplan.InspectRequest{URL: trackedPlanningURL(t, c, url), MetadataSchema: meta, Target: "primary", Environment: "test", ArtifactDigest: "sha256:held", Schemas: []string{app}, Policy: policy})
	elapsed := time.Since(started)
	if err == nil || elapsed > time.Second || !strings.Contains(strings.ToLower(err.Error()), "lock timeout") {
		t.Fatalf("bounded refusal elapsed=%v err=%v", elapsed, err)
	}
}

func TestLivePlanningTotalHoldDeadlineReleasesCompetingDDL(t *testing.T) {
	ctx, c, meta, app, _ := live(t)
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	other, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	ddlCtx, cancelDDL := context.WithCancel(context.Background())
	finished := make(chan struct{})
	startedDDL := false
	t.Cleanup(func() {
		cancelDDL()
		if startedDDL {
			select {
			case <-finished:
			case <-time.After(5 * time.Second):
				t.Error("deadline test leaked DDL goroutine")
				return
			}
		}
		_ = other.Close(context.Background())
	})
	var pid int
	if err = other.QueryRow(ctx, "select pg_backend_pid()").Scan(&pid); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	hook := func(hold context.Context) error {
		startedDDL = true
		go func() {
			defer close(finished)
			_, e := other.Exec(ddlCtx, "alter table "+pgx.Identifier{app, "accounts"}.Sanitize()+" add column hold_probe text")
			done <- e
		}()
		if err := waitRelationLock(hold, c, pid, app, "accounts"); err != nil {
			return err
		}
		<-hold.Done()
		return hold.Err()
	}
	policy := livePolicy()
	policy.MaxLockMS = 1000
	policy.MaxLockHoldMS = 300
	started := time.Now()
	_, err = expandplan.InspectLive(ctx, expandplan.InspectRequest{URL: trackedPlanningURL(t, c, url), MetadataSchema: meta, Target: "primary", Environment: "test", ArtifactDigest: "sha256:hold", Schemas: []string{app}, Policy: policy, BeforeFinalInspection: hook})
	elapsed := time.Since(started)
	var ddlErr error
	select {
	case ddlErr = <-done:
	case <-time.After(time.Second):
		t.Fatal("competing ACCESS EXCLUSIVE did not proceed after hold bound")
	}
	if ddlErr != nil {
		t.Fatal(ddlErr)
	}
	if !errors.Is(err, expandplan.ErrRefused) || !strings.Contains(err.Error(), "hold deadline") || elapsed > time.Second {
		t.Fatalf("hold deadline elapsed=%v err=%v", elapsed, err)
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
	t.Cleanup(func() { _ = admin.Close(context.Background()) })
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e7)
	role := "ep_role_" + suffix
	meta := "ep_meta_" + suffix
	app := "ep_app_" + suffix
	q := func(s ...string) string { return pgx.Identifier(s).Sanitize() }
	t.Cleanup(func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if _, err := admin.Exec(cleanup, "drop schema if exists "+q(meta)+" cascade; drop schema if exists "+q(app)+" cascade; drop role if exists "+q(role)); err != nil {
			t.Error(err)
		}
	})
	if _, err = admin.Exec(ctx, "create role "+q(role)+" login password 'ep_password'; create schema "+q(app)+"; revoke create on schema "+q(app)+" from public; create table "+q(app, "items")+"(id bigint primary key,name text); alter table "+q(app, "items")+" owner to "+q(role)+"; grant usage on schema "+q(app)+" to "+q(role)+"; revoke create on schema "+q(app)+" from "+q(role)); err != nil {
		t.Fatal(err)
	}
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
	t.Cleanup(func() { _ = roleConn.Close(context.Background()) })
	var connectedAs string
	var super bool
	if err = roleConn.QueryRow(ctx, `select current_user,r.rolsuper from pg_catalog.pg_roles r where r.rolname=current_user`).Scan(&connectedAs, &super); err != nil || connectedAs != role || super {
		t.Fatalf("non-superuser connection identity=%s super=%v err=%v", connectedAs, super, err)
	}
	if _, err = roleConn.Exec(ctx, "create index "+q(app, "should_refuse")+" on "+q(app, "items")+"(name)"); err == nil {
		t.Fatal("database unexpectedly allowed CREATE INDEX without schema CREATE")
	}
	request := expandplan.InspectRequest{URL: trackedPlanningURL(t, admin, roleURL), MetadataSchema: meta, Target: "primary", Environment: "test", ArtifactDigest: "pending", Schemas: []string{app}, Policy: livePolicy()}
	snap, err := expandplan.InspectLive(ctx, request)
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
	// This role has no monitoring membership or superuser capability. Lock
	// identities remain visible even when another role's query text is hidden.
	request.BeforeFinalInspection = queuedDDLHook(t, ctx, roleConn, url, app, "items")
	if _, err = expandplan.InspectLive(ctx, request); !errors.Is(err, expandplan.ErrRefused) || !strings.Contains(err.Error(), "conflicting relation change queued") {
		t.Fatalf("minimum-role queued-change refusal: %v", err)
	}
}
