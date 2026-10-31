package dbtest

import (
	"context"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
)

// QueryScalar enforces exactly one row/column and retains PostgreSQL scalar
// types. QueryCount remains available with its legacy integer-scan behavior.
func (d *DisposableTarget) QueryScalar(ctx context.Context, query string, args ...any) (Scalar, error) {
	if err := ctx.Err(); err != nil {
		return Scalar{}, err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.beginUse(); err != nil {
		return Scalar{}, err
	}
	ctx, cancel := context.WithDeadline(ctx, d.lease.record.Deadline)
	defer cancel()
	rows, err := d.conn.Query(ctx, query, append([]any{pgx.QueryExecModeExec}, args...)...)
	if err != nil {
		return Scalar{}, boundedDatabaseError(err)
	}
	defer rows.Close()
	return readPostgresScalar(rows)
}

func readPostgresScalar(rows pgx.Rows) (Scalar, error) {
	if !rows.Next() {
		return Scalar{}, scalarRowsError(rows, ErrScalarCardinality)
	}
	fields := rows.FieldDescriptions()
	if len(fields) != 1 {
		return Scalar{}, ErrScalarCardinality
	}
	values, err := rows.Values()
	if err != nil {
		return Scalar{}, boundedDatabaseError(err)
	}
	if rows.Next() {
		return Scalar{}, ErrScalarCardinality
	}
	if err := rows.Err(); err != nil {
		return Scalar{}, boundedDatabaseError(err)
	}
	return postgresScalar(fields[0].DataTypeOID, values[0])
}

func scalarRowsError(rows pgx.Rows, fallback error) error {
	if err := rows.Err(); err != nil {
		return boundedDatabaseError(err)
	}
	return fallback
}

func postgresScalar(oid uint32, value any) (Scalar, error) {
	if value == nil {
		return NullScalar(), nil
	}
	switch oid {
	case pgtype.Int2OID, pgtype.Int4OID, pgtype.Int8OID:
		return postgresIntegerScalar(value)
	case pgtype.TextOID, pgtype.VarcharOID, pgtype.BPCharOID, pgtype.NameOID:
		if text, ok := value.(string); ok {
			return StringScalar(text), nil
		}
	case pgtype.BoolOID:
		if boolean, ok := value.(bool); ok {
			return BooleanScalar(boolean), nil
		}
	}
	return Scalar{}, ErrScalarType
}

func postgresIntegerScalar(value any) (Scalar, error) {
	switch integer := value.(type) {
	case int16:
		return IntegerScalar(int64(integer)), nil
	case int32:
		return IntegerScalar(int64(integer)), nil
	case int64:
		return IntegerScalar(integer), nil
	default:
		return Scalar{}, ErrScalarType
	}
}

// ExecParameters always uses the extended protocol: one statement, with bound
// parameters even when the list is empty. It does not interpolate fixture data.
func (d *DisposableTarget) ExecParameters(ctx context.Context, query string, args ...any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.beginUse(); err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(ctx, d.lease.record.Deadline)
	defer cancel()
	return boundedDatabaseError(executeTestStatement(ctx, d.conn, query, args))
}

func executeTestStatement(ctx context.Context, conn *pgx.Conn, query string, args []any) error {
	// pgx.Exec switches back to simple protocol when there are no arguments,
	// even with QueryExecModeExec. Keep that path explicitly single-statement.
	if len(args) == 0 {
		_, err := conn.PgConn().ExecParams(ctx, query, nil, nil, nil, nil).Close()
		return err
	}
	_, err := conn.Exec(ctx, query, append([]any{pgx.QueryExecModeExec}, args...)...)
	return err
}
