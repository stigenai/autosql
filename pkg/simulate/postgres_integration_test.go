package simulate

import (
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"context"
	"encoding/json"
	"errors"
	"github.com/jackc/pgx/v5"
	"os"
	"sync"
	"testing"
	"time"
)

func liveFixture(t *testing.T) (schema.Document, plan.Plan) {
	t.Helper()
	ctx := context.Background()
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	empty, e := postgres.New().Normalize(ctx, empty)
	if e != nil {
		t.Fatal(e)
	}
	ns := schema.Resource{Kind: schema.KindSchema, Name: schema.Name{Name: "sim_app"}, Spec: json.RawMessage(`{}`)}
	ns.ID = schema.StableID(ns.Kind, ns.Name)
	table := schema.Resource{Kind: schema.KindTable, Name: schema.Name{Schema: "sim_app", Name: "widgets", Parent: ns.ID}, Dependencies: []schema.Dependency{{Target: ns.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(`{}`)}
	table.ID = schema.StableID(table.Kind, table.Name)
	column := schema.Resource{Kind: schema.KindColumn, Name: schema.Name{Schema: "sim_app", Name: "id", Parent: table.ID}, Dependencies: []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(`{"type":"bigint","not_null":false,"ordinal":1}`)}
	column.ID = schema.StableID(column.Kind, column.Name)
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, column}}}
	desired, e = postgres.New().Normalize(ctx, desired)
	if e != nil {
		t.Fatal(e)
	}
	p, e := plan.Build(ctx, postgres.New(), empty, desired, plan.Options{})
	if e != nil {
		t.Fatal(e)
	}
	return empty, p
}
func TestPostgresSimulationConcurrentIsolationAndCleanup(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	from, p := liveFixture(t)
	devIdentity, e := ResolvePostgresIdentity(context.Background(), url)
	if e != nil {
		t.Fatal(e)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	const count = 4
	ids := map[string]bool{}
	var mu sync.Mutex
	var wg sync.WaitGroup
	errs := make(chan error, count)
	for i := 0; i < count; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			r, e := Run(ctx, PostgresFactory{}, Request{Config: Config{DevelopmentURL: url, DevelopmentIdentity: devIdentity, ProductionIdentity: "production.example:5432/prod", CleanupTimeout: 15 * time.Second}, From: from, Plan: p})
			if e == nil {
				mu.Lock()
				if ids[r.IsolationIdentity] {
					e = errors.New("duplicate isolation")
				}
				ids[r.IsolationIdentity] = true
				mu.Unlock()
			}
			errs <- e
		}()
	}
	wg.Wait()
	close(errs)
	for e := range errs {
		if e != nil {
			t.Fatal(e)
		}
	}
	conn, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(context.Background())
	var remaining int
	if e = conn.QueryRow(ctx, `select count(*) from pg_database where datname like 'autosql_sim_%'`).Scan(&remaining); e != nil || remaining != 0 {
		t.Fatalf("remaining=%d err=%v", remaining, e)
	}
}
func TestPostgresFactoryRejectsProductionIdentityAndRemoteByDefault(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	devIdentity, e := ResolvePostgresIdentity(context.Background(), url)
	if e != nil {
		t.Fatal(e)
	}
	if _, e := (PostgresFactory{}).Create(context.Background(), Config{DevelopmentURL: url, DevelopmentIdentity: devIdentity, ProductionIdentity: devIdentity}); !errors.Is(e, ErrConfig) {
		t.Fatalf("same endpoint=%v", e)
	}
	if _, e := (PostgresFactory{}).Create(context.Background(), Config{DevelopmentURL: url, DevelopmentIdentity: devIdentity, ProductionIdentity: "postgres://different:credentials@localhost:32768/autosql"}); !errors.Is(e, ErrConfig) {
		t.Fatalf("resolved same endpoint=%v", e)
	}
	if _, e := (PostgresFactory{}).Create(context.Background(), Config{DevelopmentURL: url, DevelopmentIdentity: devIdentity + "-stale", ProductionIdentity: "other"}); !errors.Is(e, ErrConfig) {
		t.Fatalf("stale dev identity=%v", e)
	}
	if _, e := (PostgresFactory{}).Create(context.Background(), Config{DevelopmentURL: "postgres://user:secret@production.example/prod", ProductionIdentity: "other"}); !errors.Is(e, ErrConfig) || contains(e.Error(), "secret") {
		t.Fatalf("remote=%v", e)
	}
}
func TestAmbiguousCreateAlwaysCleansGeneratedDatabase(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	id, e := ResolvePostgresIdentity(context.Background(), url)
	if e != nil {
		t.Fatal(e)
	}
	factory := PostgresFactory{AfterCreate: func() error { return errors.New("server committed but client saw seeded secret") }}
	if _, e = factory.Create(context.Background(), Config{DevelopmentURL: url, DevelopmentIdentity: id, ProductionIdentity: "other", CleanupTimeout: 10 * time.Second}); e == nil || contains(e.Error(), "seeded") {
		t.Fatalf("error=%v", e)
	}
	conn, e := pgx.Connect(context.Background(), url)
	if e != nil {
		t.Fatal(e)
	}
	defer conn.Close(context.Background())
	var remaining int
	if e = conn.QueryRow(context.Background(), `select count(*) from pg_database where datname like 'autosql_sim_%'`).Scan(&remaining); e != nil || remaining != 0 {
		t.Fatalf("remaining=%d err=%v", remaining, e)
	}
}

type liveWrapperFactory struct{ mode string }

func (f liveWrapperFactory) Create(ctx context.Context, c Config) (Isolation, error) {
	iso, e := (PostgresFactory{}).Create(ctx, c)
	if e != nil {
		return nil, e
	}
	return &liveWrapper{Isolation: iso, mode: f.mode}, nil
}

type liveWrapper struct {
	Isolation
	mode string
}

func (w *liveWrapper) Execute(ctx context.Context, p plan.Plan) error {
	if w.mode == "fail" {
		return errors.New("seeded execution secret")
	}
	if w.mode == "cancel" {
		<-ctx.Done()
		return ctx.Err()
	}
	return w.Isolation.Execute(ctx, p)
}
func (w *liveWrapper) Inspect(ctx context.Context) (schema.Document, error) {
	if w.mode == "mismatch" {
		return schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}, nil
	}
	return w.Isolation.Inspect(ctx)
}
func TestPostgresCleanupAfterLiveFailureCancelAndMismatch(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	from, p := liveFixture(t)
	devIdentity, e := ResolvePostgresIdentity(context.Background(), url)
	if e != nil {
		t.Fatal(e)
	}
	for _, mode := range []string{"fail", "cancel", "mismatch"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			if mode == "cancel" {
				go func() { time.Sleep(50 * time.Millisecond); cancel() }()
			}
			_, e := Run(ctx, liveWrapperFactory{mode: mode}, Request{Config: Config{DevelopmentURL: url, DevelopmentIdentity: devIdentity, ProductionIdentity: "production.example:5432/prod", CleanupTimeout: 10 * time.Second}, From: from, Plan: p})
			if e == nil || contains(e.Error(), "seeded") {
				t.Fatalf("error=%v", e)
			}
			checkCtx, stop := context.WithTimeout(context.Background(), 5*time.Second)
			defer stop()
			conn, ce := pgx.Connect(checkCtx, url)
			if ce != nil {
				t.Fatal(ce)
			}
			defer conn.Close(context.Background())
			var remaining int
			if ce = conn.QueryRow(checkCtx, `select count(*) from pg_database where datname like 'autosql_sim_%'`).Scan(&remaining); ce != nil || remaining != 0 {
				t.Fatalf("remaining=%d err=%v", remaining, ce)
			}
		})
	}
}
