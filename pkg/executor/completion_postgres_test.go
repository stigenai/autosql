package executor

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/guardrail"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/precheck"
	"autosql/pkg/schema"

	"github.com/jackc/pgx/v5"
)

type liveCompletionFixture struct {
	a          artifact.Artifact
	verified   artifact.VerifiedObservation
	executable artifact.VerifiedArtifact
	config     PostgreSQLCompletionConfig
	conn       *pgx.Conn
	audit      *FileAudit
}

func completionDatabase(t *testing.T) *pgx.ConnConfig {
	t.Helper()
	config, err := pgx.ParseConfig(liveURL(t))
	if err != nil {
		t.Fatal(err)
	}
	nonce := make([]byte, 8)
	if _, err := rand.Read(nonce); err != nil {
		t.Fatal(err)
	}
	database := "autosql_completion_" + hex.EncodeToString(nonce)
	adminConfig := config.Copy()
	adminConfig.Database = "postgres"
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	admin, err := pgx.ConnectConfig(ctx, adminConfig)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(ctx)
	if _, err := admin.Exec(ctx, "create database "+pgx.Identifier{database}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		admin, err := pgx.ConnectConfig(ctx, adminConfig)
		if err != nil {
			t.Errorf("connect for owned database cleanup: %v", err)
			return
		}
		defer admin.Close(ctx)
		if _, err := admin.Exec(ctx, "drop database "+pgx.Identifier{database}.Sanitize()+" with (force)"); err != nil {
			t.Errorf("drop owned completion fixture: %v", err)
		}
	})
	// ConnString preserves the originally parsed string, so construct and parse
	// the owned database URL instead of only changing ConnConfig.Database.
	targetURL, err := url.Parse(config.ConnString())
	if err != nil || targetURL.Scheme == "" {
		t.Fatal("completion fixture requires a PostgreSQL test URL")
	}
	targetURL.Path = "/" + database
	query := targetURL.Query()
	query.Set("application_name", database)
	targetURL.RawQuery = query.Encode()
	config, err = pgx.ParseConfig(targetURL.String())
	if err != nil {
		t.Fatal(err)
	}
	return config
}

func newLiveCompletionFixture(t *testing.T) liveCompletionFixture {
	t.Helper()
	return newLiveCompletionFixtureWithApply(t, true)
}

func newLiveCompletionFixtureWithApply(t *testing.T, apply bool) liveCompletionFixture {
	t.Helper()
	config := completionDatabase(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, `create schema app; create table app.widgets (id bigint not null)`); err != nil {
		t.Fatal(err)
	}
	desired := completionInspect(t, ctx, conn)
	if _, err := conn.Exec(ctx, `drop schema app cascade`); err != nil {
		t.Fatal(err)
	}
	current := completionInspect(t, ctx, conn)
	p, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	changeDigest, err := guardrail.ChangeDigest(p.Changes)
	if err != nil {
		t.Fatal(err)
	}
	checks := precheck.Plan{ID: "completion-live", ChangeDigest: changeDigest}
	for _, step := range p.Steps {
		checks.Statements = append(checks.Statements, step.SQL)
	}
	checks.Digest, err = precheck.Digest(checks)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	a, err := artifact.New(p, checks, now.Add(-time.Minute), now.Add(time.Hour), "revision", "test", config.Database, "sha256:"+strings.Repeat("a", 64), artifact.Approval{Identity: "reviewer", ApprovedAt: now.Add(-time.Minute)}, map[string]string{"autosql.operator.mode": "transition"})
	if err != nil {
		t.Fatal(err)
	}
	pub, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Sign("release", private); err != nil {
		t.Fatal(err)
	}
	policy := artifact.VerifyPolicy{Now: func() time.Time { return now }, Expected: artifact.ExpectedBindings{PlanDigest: p.Digest, ChecksDigest: checks.Digest, GuardrailDigest: a.GuardrailDigest, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: a.DatabaseIdentity, ApprovalIdentity: a.Approval.Identity}, Keys: map[string]artifact.KeyRecord{"release": {PublicKey: pub, Issuer: "issuer", Identity: "signer", Environment: "test", Purpose: "release", Status: "active", NotBefore: now.Add(-time.Hour), NotAfter: now.Add(3 * time.Hour)}}, Issuer: "issuer", Identity: "signer", Purpose: "release"}
	verified, err := a.VerifyTrusted(policy)
	if err != nil {
		t.Fatal(err)
	}
	audit := &FileAudit{Path: filepath.Join(t.TempDir(), "lifecycle.jsonl")}
	exec, err := NewPostgreSQL(Config{URL: config.ConnString(), Audit: audit, State: func(ctx context.Context, session Session) (RuntimeState, error) {
		doc := completionInspect(t, ctx, session.Raw())
		fp, err := schema.SemanticFingerprint(ExcludeBookkeeping(doc))
		return RuntimeState{Fingerprint: fp, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: a.DatabaseIdentity}, err
	}}, verified)
	if err != nil {
		t.Fatal(err)
	}
	if apply {
		if _, err := exec.ApplyAuthorized(ctx, checks); err != nil {
			t.Fatal(err)
		}
		if exec.Result().AppliedSteps != len(p.Steps) {
			t.Fatalf("fixture did not execute every signed step: %+v", exec.Result())
		}
	}
	observedAt := a.ExpiresAt.Add(time.Minute)
	policy.Now = func() time.Time { return observedAt }
	observation, err := a.VerifyForObservation(policy)
	if err != nil {
		t.Fatal(err)
	}
	return liveCompletionFixture{a: a, executable: verified, verified: observation, conn: conn, audit: audit, config: PostgreSQLCompletionConfig{URL: config.ConnString(), DatabaseIdentity: a.DatabaseIdentity, Environment: a.TargetEnvironment, SourceRevision: a.SourceRevision, Schemas: []string{"app"}, Managed: desired, Audit: audit, Now: policy.Now}}
}

func completionInspect(t *testing.T, ctx context.Context, conn *pgx.Conn) schema.Document {
	t.Helper()
	doc, err := postgres.InspectConn(ctx, conn, postgres.Options{Schemas: []string{"app"}})
	if err != nil {
		t.Fatal(err)
	}
	doc, err = postgres.New().Normalize(ctx, doc)
	if err != nil {
		t.Fatal(err)
	}
	return doc
}

func completionSnapshot(t *testing.T, f liveCompletionFixture) (string, string, []byte) {
	t.Helper()
	ctx := context.Background()
	doc := completionInspect(t, ctx, f.conn)
	fingerprint, err := schema.SemanticFingerprint(doc)
	if err != nil {
		t.Fatal(err)
	}
	var exists bool
	if err := f.conn.QueryRow(ctx, `select to_regclass('autosql_migration_history') is not null`).Scan(&exists); err != nil {
		t.Fatal(err)
	}
	history := "missing"
	if exists {
		if err := f.conn.QueryRow(ctx, `select coalesce(jsonb_agg(to_jsonb(h) order by step_id,attempt)::text,'[]') from autosql_migration_history h`).Scan(&history); err != nil {
			t.Fatal(err)
		}
	}
	audit, err := os.ReadFile(f.audit.Path)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		t.Fatal(err)
	}
	return fingerprint, history, audit
}

func assertCompletionLockAvailable(t *testing.T, f liveCompletionFixture) {
	t.Helper()
	key, err := LockKey(f.a.DatabaseIdentity, f.a.TargetEnvironment)
	if err != nil {
		t.Fatal(err)
	}
	var locked, unlocked bool
	if err := f.conn.QueryRow(context.Background(), `select pg_try_advisory_lock(hashtextextended($1,0::bigint))`, key).Scan(&locked); err != nil || !locked {
		t.Fatalf("observation left target lock held: %v", err)
	}
	if err := f.conn.QueryRow(context.Background(), `select pg_advisory_unlock(hashtextextended($1,0::bigint))`, key).Scan(&unlocked); err != nil || !unlocked {
		t.Fatalf("release test lock: %v", err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	for {
		var remaining int
		if err := f.conn.QueryRow(ctx, `select count(*) from pg_stat_activity where application_name=$1 and pid<>pg_backend_pid()`, f.a.DatabaseIdentity).Scan(&remaining); err != nil {
			t.Fatalf("inspect owned observer sessions: %v", err)
		}
		if remaining == 0 {
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("observation left a database session open")
		case <-time.After(10 * time.Millisecond):
		}
	}
}

type completionQueryTrace struct{ statements []string }

func (t *completionQueryTrace) TraceQueryStart(ctx context.Context, _ *pgx.Conn, data pgx.TraceQueryStartData) context.Context {
	t.statements = append(t.statements, data.SQL)
	return ctx
}
func (*completionQueryTrace) TraceQueryEnd(context.Context, *pgx.Conn, pgx.TraceQueryEndData) {}

func TestCompletionObservationLiveUsesReadOnlyTransaction(t *testing.T) {
	f := newLiveCompletionFixture(t)
	config, err := pgx.ParseConfig(f.config.URL)
	if err != nil {
		t.Fatal(err)
	}
	trace := &completionQueryTrace{}
	config.Tracer = trace
	conn, err := pgx.ConnectConfig(context.Background(), config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	got := observeCompletionConnection(context.Background(), conn, f.verified, f.config, f.a)
	if got.Outcome != CompletionProven {
		t.Fatalf("completion=%+v", got)
	}
	readOnly := false
	for _, statement := range trace.statements {
		sql := strings.ToLower(strings.TrimSpace(statement))
		if strings.HasPrefix(sql, "begin ") && strings.Contains(sql, "read only") {
			readOnly = true
		}
		for _, forbidden := range []string{"create ", "alter ", "drop ", "insert ", "update ", "delete ", "truncate "} {
			if strings.HasPrefix(sql, forbidden) {
				t.Fatalf("observation issued mutation: %s", forbidden)
			}
		}
	}
	if !readOnly {
		t.Fatal("observation did not open a read-only transaction")
	}
}

func TestCompletionObservationLivePreservesStateAndHistory(t *testing.T) {
	cases := []struct {
		name, sql, reason string
		outcome           CompletionOutcome
	}{
		{"completed", "", "completion_proven", CompletionProven},
		{"missing history", "drop table autosql_migration_history", "history_missing_bindings", CompletionIneligible},
		{"legacy history", "alter table autosql_migration_history drop column bundle_digest", "history_missing_bindings", CompletionIneligible},
		{"missing step", "delete from autosql_migration_history where step_id=(select min(step_id) from autosql_migration_history)", "history_incomplete", CompletionRefused},
		{"intended step", "update autosql_migration_history set state='intended'", "history_invalid", CompletionRefused},
		{"wrong hash", "update autosql_migration_history set step_hash='other'", "history_invalid", CompletionRefused},
		{"drift", "create table app.unplanned(id int)", "state_drifted", CompletionRefused},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			f := newLiveCompletionFixture(t)
			if tc.sql != "" {
				if _, err := f.conn.Exec(context.Background(), tc.sql); err != nil {
					t.Fatal(err)
				}
			}
			beforeFP, beforeHistory, beforeAudit := completionSnapshot(t, f)
			got := ObservePostgreSQLCompletion(context.Background(), f.verified, f.config)
			if got.Outcome != tc.outcome || got.Reason != tc.reason {
				t.Fatalf("result=%+v, want %s/%s", got, tc.outcome, tc.reason)
			}
			afterFP, afterHistory, afterAudit := completionSnapshot(t, f)
			if beforeFP != afterFP || beforeHistory != afterHistory || !bytes.Equal(beforeAudit, afterAudit) {
				t.Fatal("observation changed managed state, history, or audit")
			}
			assertCompletionLockAvailable(t, f)
		})
	}
}

type completionAuditFunc func(context.Context, string) ([]LifecycleEvent, error)

func (f completionAuditFunc) ReadExecution(ctx context.Context, digest string) ([]LifecycleEvent, error) {
	return f(ctx, digest)
}

func TestCompletionObservationLiveCancellationAndContention(t *testing.T) {
	f := newLiveCompletionFixture(t)
	key, _ := LockKey(f.a.DatabaseIdentity, f.a.TargetEnvironment)
	if _, err := f.conn.Exec(context.Background(), `select pg_advisory_lock(hashtextextended($1,0::bigint))`, key); err != nil {
		t.Fatal(err)
	}
	got := ObservePostgreSQLCompletion(context.Background(), f.verified, f.config)
	if got.Outcome != CompletionRetryable || got.Reason != "target_busy" {
		t.Fatalf("contention=%+v", got)
	}
	if _, err := f.conn.Exec(context.Background(), `select pg_advisory_unlock(hashtextextended($1,0::bigint))`, key); err != nil {
		t.Fatal(err)
	}
	entered := make(chan struct{})
	f.config.Audit = completionAuditFunc(func(ctx context.Context, _ string) ([]LifecycleEvent, error) {
		close(entered)
		<-ctx.Done()
		return nil, ctx.Err()
	})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	result := make(chan CompletionEvidence, 1)
	go func() { result <- ObservePostgreSQLCompletion(ctx, f.verified, f.config) }()
	select {
	case <-entered:
		cancel()
	case got := <-result:
		t.Fatalf("observer did not reach audit: %+v", got)
	case <-ctx.Done():
		t.Fatal("observer did not reach audit before timeout")
	}
	select {
	case got := <-result:
		if got.Outcome != CompletionRetryable {
			t.Fatalf("canceled observation=%+v", got)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("canceled observer did not release its connection")
	}
	assertCompletionLockAvailable(t, f)
}

func TestCompletionObservationLiveRechecksChangesDuringAudit(t *testing.T) {
	for _, sql := range []string{"create table app.unplanned(id int)", "update autosql_migration_history set state='intended'"} {
		t.Run(sql, func(t *testing.T) {
			f := newLiveCompletionFixture(t)
			f.config.Audit = completionAuditFunc(func(ctx context.Context, digest string) ([]LifecycleEvent, error) {
				if _, err := f.conn.Exec(ctx, sql); err != nil {
					return nil, err
				}
				return f.audit.ReadExecution(ctx, digest)
			})
			if got := ObservePostgreSQLCompletion(context.Background(), f.verified, f.config); got.Outcome != CompletionRefused {
				t.Fatalf("changed evidence accepted: %+v", got)
			}
			assertCompletionLockAvailable(t, f)
		})
	}
}
