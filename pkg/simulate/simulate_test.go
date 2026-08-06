package simulate

import (
	"autosql/pkg/plan"
	"autosql/pkg/plugin/sample"
	"autosql/pkg/schema"
	"context"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

type fakeFactory struct {
	iso *fakeIsolation
	err error
}

func (f fakeFactory) Create(context.Context, Config) (Isolation, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.iso.event("create")
	return f.iso, nil
}

type fakeIsolation struct {
	mu                  sync.Mutex
	events              []string
	actual              schema.Document
	fail                string
	cleanupFail         bool
	cleanupContextAlive bool
}

func (f *fakeIsolation) event(s string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.events = append(f.events, s)
}
func (f *fakeIsolation) Identity() string { return "dev/isolation" }
func (f *fakeIsolation) Materialize(context.Context, schema.Document) error {
	f.event("materialize")
	if f.fail == "materialize" {
		return errors.New("secret materialize")
	}
	return nil
}
func (f *fakeIsolation) Execute(ctx context.Context, _ plan.Plan) error {
	f.event("execute")
	if f.fail == "cancel" {
		<-ctx.Done()
		return ctx.Err()
	}
	if f.fail == "execute" {
		return errors.New("secret execute")
	}
	if f.fail == "postgres" {
		return &pgconn.PgError{Code: "42704", Message: `role "app_owner" does not exist`, Detail: "seeded secret detail"}
	}
	return nil
}
func (f *fakeIsolation) Inspect(context.Context) (schema.Document, error) {
	f.event("inspect")
	if f.fail == "inspect" {
		return schema.Document{}, errors.New("secret inspect")
	}
	return f.actual, nil
}
func (f *fakeIsolation) Cleanup(ctx context.Context) error {
	f.event("cleanup")
	f.cleanupContextAlive = ctx.Err() == nil
	if f.fail == "cleanup" || f.cleanupFail {
		return errors.New("secret cleanup")
	}
	return nil
}
func TestPrimaryAndCancelErrorsJoinCleanupFailure(t *testing.T) {
	from, to, p := plans(t)
	for _, mode := range []string{"execute", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			iso := &fakeIsolation{actual: to, fail: mode, cleanupFail: true}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "cancel" {
				go func() { time.Sleep(10 * time.Millisecond); cancel() }()
			}
			_, e := Run(ctx, fakeFactory{iso: iso}, Request{Config: Config{DevelopmentURL: "postgres://dev", ProductionIdentity: "prod/db", CleanupTimeout: time.Second}, From: from, Plan: p})
			if e == nil || !contains(e.Error(), "simulation execute") || !contains(e.Error(), "simulation cleanup") {
				t.Fatalf("joined error=%v", e)
			}
		})
	}
}
func plans(t *testing.T) (schema.Document, schema.Document, plan.Plan) {
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	n := schema.Name{Name: "app"}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{{ID: schema.StableID(schema.KindSchema, n), Kind: schema.KindSchema, Name: n, Spec: []byte(`{}`)}}}}
	p, e := plan.Build(context.Background(), sample.Driver{}, empty, desired, plan.Options{})
	if e != nil {
		t.Fatal(e)
	}
	return empty, desired, p
}
func TestLifecycleAlwaysCleansUp(t *testing.T) {
	from, to, p := plans(t)
	for _, failure := range []string{"", "materialize", "execute", "inspect", "cleanup"} {
		t.Run(failure, func(t *testing.T) {
			iso := &fakeIsolation{actual: to, fail: failure}
			result, e := Run(context.Background(), fakeFactory{iso: iso}, Request{Config: Config{DevelopmentURL: "postgres://dev", ProductionIdentity: "prod/db", CleanupTimeout: time.Second}, From: from, Plan: p})
			if failure == "" && (e != nil || !result.Verified) {
				t.Fatalf("result=%+v err=%v", result, e)
			}
			if failure != "" && e == nil {
				t.Fatal("failure accepted")
			}
			if iso.events[len(iso.events)-1] != "cleanup" || !iso.cleanupContextAlive {
				t.Fatalf("events=%v alive=%v", iso.events, iso.cleanupContextAlive)
			}
		})
	}
}
func TestCancelStillCleansWithFreshContext(t *testing.T) {
	from, to, p := plans(t)
	iso := &fakeIsolation{actual: to, fail: "cancel"}
	ctx, cancel := context.WithCancel(context.Background())
	go func() { time.Sleep(10 * time.Millisecond); cancel() }()
	_, e := Run(ctx, fakeFactory{iso: iso}, Request{Config: Config{DevelopmentURL: "postgres://dev", ProductionIdentity: "prod/db", CleanupTimeout: time.Second}, From: from, Plan: p})
	if e == nil || !iso.cleanupContextAlive || iso.events[len(iso.events)-1] != "cleanup" {
		t.Fatalf("err=%v events=%v", e, iso.events)
	}
}
func TestFingerprintMismatchAndRedaction(t *testing.T) {
	from, _, p := plans(t)
	iso := &fakeIsolation{actual: from}
	_, e := Run(context.Background(), fakeFactory{iso: iso}, Request{Config: Config{DevelopmentURL: "postgres://user:seeded-secret@dev", ProductionIdentity: "prod/db"}, From: from, Plan: p})
	if !errors.Is(e, ErrFingerprint) || contains(e.Error(), "seeded-secret") {
		t.Fatalf("error=%v", e)
	}
}

func TestPostgresCauseRetainsSQLStateWithoutSensitiveDetail(t *testing.T) {
	from, to, p := plans(t)
	iso := &fakeIsolation{actual: to, fail: "postgres"}
	_, err := Run(context.Background(), fakeFactory{iso: iso}, Request{Config: Config{DevelopmentURL: "postgres://dev", ProductionIdentity: "prod/db"}, From: from, Plan: p})
	var databaseError *PostgresError
	if !errors.As(err, &databaseError) || databaseError.SQLState() != "42704" || !strings.Contains(err.Error(), `role "app_owner" does not exist`) || strings.Contains(err.Error(), "seeded secret") {
		t.Fatalf("redacted PostgreSQL cause=%v", err)
	}
}

func TestPostgresCauseRedactsSecretBearingPrimaryMessage(t *testing.T) {
	err := RedactedCause(&pgconn.PgError{Code: "42704", Message: `role "postgres://user:secret@host/db" does not exist`})
	if err == nil || !strings.Contains(err.Error(), "SQLSTATE 42704") || strings.Contains(err.Error(), "secret") || strings.Contains(err.Error(), "://") {
		t.Fatalf("redacted PostgreSQL message=%v", err)
	}
}

func TestCleanupRetriesTransientTerminationDropAndAbsence(t *testing.T) {
	transient := func(message string) error { return &pgconn.PgError{Code: "55006", Message: message} }
	attempt, terminateCalls, dropCalls, absentCalls, sleeps := 0, 0, 0, 0, 0
	cycle := func(ctx context.Context) (bool, error) {
		attempt++
		return cleanupDatabaseAttempt(ctx,
			func(context.Context) error {
				terminateCalls++
				if attempt == 1 {
					return transient("termination raced backend")
				}
				return nil
			},
			func(context.Context) error {
				dropCalls++
				if attempt == 2 {
					return transient("drop database is being accessed")
				}
				return nil
			},
			func(context.Context) (bool, error) {
				absentCalls++
				return attempt == 4, nil
			})
	}
	if err := cleanupDatabaseCycles(context.Background(), cycle, func(context.Context, time.Duration) error { sleeps++; return nil }); err != nil {
		t.Fatal(err)
	}
	if attempt != 4 || terminateCalls != 4 || dropCalls != 3 || absentCalls != 2 || sleeps != 3 {
		t.Fatalf("attempts=%d terminate=%d drop=%d absent=%d sleeps=%d", attempt, terminateCalls, dropCalls, absentCalls, sleeps)
	}
}

func TestCleanupPersistentPresenceIsConfirmedFailure(t *testing.T) {
	attempts := 0
	cycle := func(context.Context) (bool, error) { attempts++; return false, nil }
	err := cleanupDatabaseCycles(context.Background(), cycle, func(context.Context, time.Duration) error { return nil })
	var present *databaseStillPresentError
	if !errors.As(err, &present) || attempts != 12 {
		t.Fatalf("err=%v attempts=%d", err, attempts)
	}
}

func TestCleanupCancellationPreservesLastAdministrativeError(t *testing.T) {
	last := &pgconn.PgError{Code: "55006", Message: "database is being accessed"}
	ctx, cancel := context.WithCancel(context.Background())
	cycle := func(context.Context) (bool, error) { return false, last }
	err := cleanupDatabaseCycles(ctx, cycle, func(context.Context, time.Duration) error { cancel(); return ctx.Err() })
	if !errors.Is(err, last) || !errors.Is(err, context.Canceled) {
		t.Fatalf("cleanup cancellation lost causes: %v", err)
	}
}
func contains(s, x string) bool {
	for i := 0; i+len(x) <= len(s); i++ {
		if s[i:i+len(x)] == x {
			return true
		}
	}
	return false
}
