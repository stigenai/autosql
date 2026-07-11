package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"regexp"
	"strings"

	"github.com/jackc/pgx/v5"
)

// PostgresFactory creates a random PostgreSQL schema per case. DSN should
// reference a disposable test database; the connected role needs CREATE on it.
type PostgresFactory struct{ DSN string }

func (f PostgresFactory) OpenIsolated(ctx context.Context, caseName string) (Database, error) {
	conn, err := pgx.Connect(ctx, f.DSN)
	if err != nil {
		return nil, err
	}
	name := postgresSchemaName(caseName)
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		_ = conn.Close(ctx)
		return nil, err
	}
	if _, err := conn.Exec(ctx, "SET search_path TO "+quoted+", pg_catalog"); err != nil {
		_, _ = conn.Exec(ctx, "DROP SCHEMA "+quoted+" CASCADE")
		_ = conn.Close(ctx)
		return nil, err
	}
	return &postgresDB{conn: conn, schema: quoted}, nil
}

type postgresDB struct {
	conn   *pgx.Conn
	schema string
}

func (d *postgresDB) Exec(ctx context.Context, query string) error {
	_, err := d.conn.Exec(ctx, query)
	return err
}
func (d *postgresDB) QueryCount(ctx context.Context, query string, args ...any) (int64, error) {
	var n int64
	err := d.conn.QueryRow(ctx, query, args...).Scan(&n)
	return n, err
}
func (d *postgresDB) Close(ctx context.Context) error {
	_, dropErr := d.conn.Exec(ctx, "DROP SCHEMA "+d.schema+" CASCADE")
	closeErr := d.conn.Close(ctx)
	if dropErr != nil {
		return fmt.Errorf("drop isolated schema: %w", dropErr)
	}
	return closeErr
}

var nonIdentifier = regexp.MustCompile(`[^a-z0-9_]+`)

func postgresSchemaName(caseName string) string {
	base := nonIdentifier.ReplaceAllString(strings.ToLower(caseName), "_")
	if len(base) > 32 {
		base = base[:32]
	}
	if base == "" {
		base = "case"
	}
	var suffix [8]byte
	if _, err := rand.Read(suffix[:]); err != nil {
		panic("crypto/rand unavailable: " + err.Error())
	}
	return "autosql_" + base + "_" + hex.EncodeToString(suffix[:])
}
