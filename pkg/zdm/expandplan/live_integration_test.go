package expandplan_test

import (
	"context"
	"crypto/ed25519"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"autosql/pkg/zdm"
	"autosql/pkg/zdm/expandplan"
	"autosql/pkg/zerodowntime"
	"github.com/jackc/pgx/v5"
)

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
	snap, err := expandplan.InspectLive(ctx, expandplan.InspectRequest{URL: url, MetadataSchema: meta, Target: "primary", Environment: "test", Schemas: []string{app}})
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
	p, err := expandplan.Build(expandplan.Request{Migration: m, Snapshot: snap, ExpectedFingerprint: snap.Fingerprint, Target: "primary", Environment: "test", Policy: expandplan.Policy{MaxLockMS: 100, MaxStatementMS: 1000, MaxTransactionMS: 2000}, Verify: func(m zerodowntime.Migration) error { return m.Verify(pub) }})
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
	if _, err = expandplan.InspectLive(ctx, expandplan.InspectRequest{URL: url, MetadataSchema: meta, Target: "primary", Environment: "test", Schemas: []string{app}}); err == nil {
		t.Fatal("expected baseline drift refusal")
	}
}
