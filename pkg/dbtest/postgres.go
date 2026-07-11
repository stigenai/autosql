package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// PostgresFactory creates a random PostgreSQL schema per case. DSN should
// reference a disposable test database; the connected role needs CREATE on it.
type PostgresFactory struct {
	DSN                 string
	SetupCleanupTimeout time.Duration
	// OnSchema is an optional observability hook useful for CI cleanup checks.
	OnSchema func(string)
}

func (f PostgresFactory) OpenIsolated(ctx context.Context, caseName string) (Database, error) {
	conn, err := pgx.Connect(ctx, f.DSN)
	if err != nil {
		return nil, err
	}
	name := postgresSchemaName(caseName)
	quoted := pgx.Identifier{name}.Sanitize()
	if _, err := conn.Exec(ctx, "CREATE SCHEMA "+quoted); err != nil {
		closeCtx, cancel := freshSetupContext(ctx, f.SetupCleanupTimeout)
		closeErr := conn.Close(closeCtx)
		cancel()
		return nil, errors.Join(err, closeErr)
	}
	if f.OnSchema != nil {
		f.OnSchema(name)
	}
	if _, err := conn.Exec(ctx, "SET search_path TO "+quoted+", pg_catalog"); err != nil {
		dropCtx, cancel := freshSetupContext(ctx, f.SetupCleanupTimeout)
		_, dropErr := conn.Exec(dropCtx, "DROP SCHEMA "+quoted+" CASCADE")
		cancel()
		closeCtx, cancel := freshSetupContext(ctx, f.SetupCleanupTimeout)
		closeErr := conn.Close(closeCtx)
		cancel()
		return nil, errors.Join(err, dropErr, closeErr)
	}
	return &postgresDB{conn: conn, schema: quoted, cleanupTimeout: f.SetupCleanupTimeout}, nil
}

type postgresDB struct {
	conn           *pgx.Conn
	schema         string
	cleanupTimeout time.Duration
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
	dropCtx, cancel := freshSetupContext(ctx, d.cleanupTimeout)
	_, dropErr := d.conn.Exec(dropCtx, "DROP SCHEMA "+d.schema+" CASCADE")
	cancel()
	closeCtx, cancel := freshSetupContext(ctx, d.cleanupTimeout)
	closeErr := d.conn.Close(closeCtx)
	cancel()
	if dropErr != nil {
		dropErr = fmt.Errorf("drop isolated schema: %w", dropErr)
	}
	return errors.Join(dropErr, closeErr)
}

func freshSetupContext(parent context.Context, timeout time.Duration) (context.Context, context.CancelFunc) {
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	return context.WithTimeout(context.WithoutCancel(parent), timeout)
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
