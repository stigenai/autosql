package dbtest

import (
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
)

func TestPostgresFactoryIntegration(t *testing.T) {
	dsn := os.Getenv("AUTOSQL_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set AUTOSQL_POSTGRES_TEST_DSN to run PostgreSQL integration")
	}
	c := Case{Name: "blank-to-v2", Timeout: 30 * time.Second, Versions: []Version{{Name: "v1", Migrations: []Command{{SQL: "CREATE TABLE widgets (id bigint PRIMARY KEY)"}}, Assertions: []Assertion{{Name: "blank creation", SQL: "SELECT count(*) FROM widgets", Want: 0}}}, {Name: "v2", Migrations: []Command{{SQL: "ALTER TABLE widgets ADD COLUMN label text"}}, Plan: []Command{{SQL: "INSERT INTO widgets (id, label) VALUES (1, 'ready')"}}, Assertions: []Assertion{{Name: "upgrade", SQL: "SELECT count(*) FROM widgets WHERE label = 'ready'", Want: 1}}}}}
	var schema string
	result, err := (Runner{Factory: PostgresFactory{DSN: dsn, OnSchema: func(name string) { schema = name }}}).Run(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Versions) != 2 {
		t.Fatalf("versions: %v", result.Versions)
	}
	admin, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var remaining int
	if err := admin.QueryRow(context.Background(), "SELECT count(*) FROM pg_namespace WHERE nspname=$1", schema).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("isolated schema %q was not deleted", schema)
	}
}

func TestPostgresPartialSetupCancellationDeletesSchema(t *testing.T) {
	dsn := os.Getenv("AUTOSQL_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set AUTOSQL_POSTGRES_TEST_DSN to run PostgreSQL integration")
	}
	ctx, cancel := context.WithCancel(context.Background())
	var schema string
	factory := PostgresFactory{DSN: dsn, OnSchema: func(name string) { schema = name; cancel() }}
	db, err := factory.OpenIsolated(ctx, "cancel-partial-setup")
	if db != nil || !errors.Is(err, context.Canceled) {
		t.Fatalf("database=%v error=%v", db, err)
	}
	admin, err := pgx.Connect(context.Background(), dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	var remaining int
	if err := admin.QueryRow(context.Background(), "SELECT count(*) FROM pg_namespace WHERE nspname=$1", schema).Scan(&remaining); err != nil {
		t.Fatal(err)
	}
	if remaining != 0 {
		t.Fatalf("partially-created schema %q was not deleted", schema)
	}
}
