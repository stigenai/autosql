package cli

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosql/pkg/zdm"
	"autosql/pkg/zerodowntime"
	"github.com/jackc/pgx/v5"
)

func TestExpandPlanCLILiveHumanJSONRefusalRedactionAndNoMutation(t *testing.T) {
	dsn := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	c, err := pgx.Connect(ctx, dsn)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = c.Exec(ctx, `select pg_catalog.pg_advisory_lock(pg_catalog.hashtextextended('autosql.expandplan.live-tests/v1',0::bigint))`); err != nil {
		t.Fatal(err)
	}
	suffix := fmt.Sprint(time.Now().UnixNano() % 1e8)
	meta := "ep_cli_" + suffix
	app := meta + "_app"
	q := func(s ...string) string { return pgx.Identifier(s).Sanitize() }
	if _, err = c.Exec(ctx, "create schema "+q(app)+"; create table "+q(app, "users")+"(id bigint primary key,name text)"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = c.Exec(context.Background(), "drop schema if exists "+q(meta)+" cascade; drop schema if exists "+q(app)+" cascade")
		_, _ = c.Exec(context.Background(), `select pg_catalog.pg_advisory_unlock(pg_catalog.hashtextextended('autosql.expandplan.live-tests/v1',0::bigint))`)
		c.Close(context.Background())
	})
	store, err := zdm.Open(zdm.Config{URL: dsn, Schema: meta})
	if err != nil {
		t.Fatal(err)
	}
	if err = store.Init(ctx); err != nil {
		t.Fatal(err)
	}
	baseline, err := store.Baseline(ctx, zdm.BaselineRequest{ID: "adopt", Target: "primary", Environment: "test", Operator: "tester", Schemas: []string{app}})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := zerodowntime.AddColumnEffects("none")
	m, err := zerodowntime.New("v2", zerodowntime.VersionSchema{Name: "v2", ExposeDuringExpand: true}, zerodowntime.Requirements{MinimumPostgres: 14, LockTimeoutMS: 100, StatementTimeoutMS: 1000}, []zerodowntime.Operation{{ID: "01", Kind: zerodowntime.AddColumn, Table: "users", Column: "email", DataType: "text", SynchronizationMode: "none", Effects: e, Reversal: zerodowntime.Reversal{Mode: "automatic"}}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	artifactPub, artifactPriv, _ := ed25519.GenerateKey(nil)
	if err = m.Sign("release", artifactPriv); err != nil {
		t.Fatal(err)
	}
	planPub, planPriv, _ := ed25519.GenerateKey(nil)
	_ = planPub
	data, _ := m.MarshalJSONCanonical()
	path := filepath.Join(t.TempDir(), "migration.json")
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_EP_DSN", dsn)
	t.Setenv("AUTOSQL_EP_ARTIFACT_PUB", base64.RawStdEncoding.EncodeToString(artifactPub))
	t.Setenv("AUTOSQL_EP_PLAN_PRIV", base64.RawStdEncoding.EncodeToString(planPriv))
	wrongPub, _, _ := ed25519.GenerateKey(nil)
	t.Setenv("AUTOSQL_EP_WRONG_PUB", base64.RawStdEncoding.EncodeToString(wrongPub))
	secretMarker := "postgres:postgres@"
	run := func(extra ...string) (int, string, string) {
		args := []string{"migrate", "expand-plan", "--file", path, "--url", "env://AUTOSQL_EP_DSN", "--public-key", "env://AUTOSQL_EP_ARTIFACT_PUB", "--plan-signing-key", "env://AUTOSQL_EP_PLAN_PRIV", "--plan-key-id", "planner", "--metadata-schema", meta, "--target", "primary", "--env", "test", "--expected-fingerprint", baseline.Fingerprint, "--schema", app, "--max-lock-ms", "100", "--max-statement-ms", "1000", "--max-transaction-ms", "1000"}
		args = append(args, extra...)
		var out, stderr bytes.Buffer
		code := Run(ctx, args, Streams{Out: &out, Err: &stderr})
		combined := out.String() + stderr.String()
		if strings.Contains(combined, dsn) || strings.Contains(combined, secretMarker) || strings.Contains(combined, base64.RawStdEncoding.EncodeToString(planPriv)) {
			t.Fatalf("credential leaked: %s", combined)
		}
		return code, out.String(), stderr.String()
	}
	var before int
	if err = c.QueryRow(ctx, `select count(*) from pg_catalog.pg_class c join pg_catalog.pg_namespace n on n.oid=c.relnamespace where n.nspname=any($1)`, []string{app, meta}).Scan(&before); err != nil {
		t.Fatal(err)
	}
	if code, out, stderr := run(); code != 0 || stderr != "" || !strings.Contains(out, "read-only planned steps") {
		t.Fatalf("human code=%d out=%q err=%q", code, out, stderr)
	}
	if code, out, stderr := run("--json"); code != 0 || stderr != "" {
		t.Fatalf("json code=%d out=%q err=%q", code, out, stderr)
	} else {
		var env Envelope
		if json.Unmarshal([]byte(out), &env) != nil || !env.OK || !strings.Contains(out, "attestation") {
			t.Fatalf("bad JSON: %s", out)
		}
	}
	var after int
	if err = c.QueryRow(ctx, `select count(*) from pg_catalog.pg_class c join pg_catalog.pg_namespace n on n.oid=c.relnamespace where n.nspname=any($1)`, []string{app, meta}).Scan(&after); err != nil || after != before {
		t.Fatalf("CLI mutated target before=%d after=%d err=%v", before, after, err)
	}
	if code, out, _ := run("--public-key", "env://AUTOSQL_EP_WRONG_PUB", "--json"); code != int(ExitValidation) || !strings.Contains(out, "signature") {
		t.Fatalf("refusal code=%d out=%s", code, out)
	}
}
