package dbtest

import (
	"context"
	"os"
	"testing"
	"time"
)

func TestPostgresFactoryIntegration(t *testing.T) {
	dsn := os.Getenv("AUTOSQL_POSTGRES_TEST_DSN")
	if dsn == "" {
		t.Skip("set AUTOSQL_POSTGRES_TEST_DSN to run PostgreSQL integration")
	}
	c := Case{Name: "blank-to-v2", Timeout: 30 * time.Second, Versions: []Version{{Name: "v1", Migrations: []Command{{SQL: "CREATE TABLE widgets (id bigint PRIMARY KEY)"}}, Assertions: []Assertion{{Name: "blank creation", SQL: "SELECT count(*) FROM widgets", Want: 0}}}, {Name: "v2", Migrations: []Command{{SQL: "ALTER TABLE widgets ADD COLUMN label text"}}, Plan: []Command{{SQL: "INSERT INTO widgets (id, label) VALUES (1, 'ready')"}}, Assertions: []Assertion{{Name: "upgrade", SQL: "SELECT count(*) FROM widgets WHERE label = 'ready'", Want: 1}}}}}
	result, err := (Runner{Factory: PostgresFactory{DSN: dsn}}).Run(context.Background(), c)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Versions) != 2 {
		t.Fatalf("versions: %v", result.Versions)
	}
}
