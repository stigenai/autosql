package dbtest

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ExpectError runs exactly one operation. Normally it rolls back a disposable
// transaction even on success. Explicit nontransactional operations get a fresh
// restricted connection which is closed before returning. Cleanup failures are
// joined after matching and can never be swallowed by an expected SQLSTATE.
func (d *DisposableTarget) ExpectError(ctx context.Context, query, state string, nonTransactional bool, args ...any) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if !validExpectedState(state) {
		return ErrExpectedError
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.beginUse(); err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(ctx, d.lease.record.Deadline)
	defer cancel()
	if nonTransactional {
		return d.expectNonTransactional(ctx, query, state, args)
	}
	tx, err := d.conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return boundedDatabaseError(err)
	}
	operationErr := executeTestStatement(ctx, tx.Conn(), query, args)
	primary := checkExpectedSQLState(ctx, operationErr, state)
	cleanupCtx, cleanupCancel := freshSetupContext(ctx, d.cleanupTimeout)
	rollbackErr := tx.Rollback(cleanupCtx)
	cleanupCancel()
	return operationCleanupError(primary, boundedDatabaseError(rollbackErr))
}

func (d *DisposableTarget) expectNonTransactional(ctx context.Context, query, state string, args []any) (result error) {
	conn, err := pgx.ConnectConfig(ctx, d.conn.Config().Copy())
	if err != nil {
		return boundedDatabaseError(err)
	}
	defer func() {
		cleanupCtx, cancel := freshSetupContext(ctx, d.cleanupTimeout)
		defer cancel()
		result = operationCleanupError(result, boundedDatabaseError(conn.Close(cleanupCtx)))
	}()
	if err := d.verifyFreshFixture(ctx, conn); err != nil {
		return err
	}
	operationErr := executeTestStatement(ctx, conn, query, args)
	return checkExpectedSQLState(ctx, operationErr, state)
}

func (d *DisposableTarget) verifyFreshFixture(ctx context.Context, conn *pgx.Conn) error {
	if conn.PgConn().Conn().RemoteAddr().String() != d.conn.PgConn().Conn().RemoteAddr().String() {
		return ErrOwnership
	}
	var database, name string
	var databaseOID, roleOID uint32
	var unprivileged bool
	err := conn.QueryRow(ctx, `SELECT current_database(),current_user,d.oid,r.oid,
 NOT(rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls OR rolinherit)
 FROM pg_catalog.pg_database d,pg_catalog.pg_roles r
 WHERE d.datname=current_database() AND r.rolname=current_user`).Scan(&database, &name, &databaseOID, &roleOID, &unprivileged)
	if err != nil {
		return boundedDatabaseError(err)
	}
	lease := d.lease.record
	if !unprivileged || database != lease.resourceName() || name != lease.resourceName() || databaseOID != resourceOID(lease, "database") || roleOID != resourceOID(lease, "role") {
		return ErrOwnership
	}
	return nil
}

func checkExpectedSQLState(ctx context.Context, operationErr error, state string) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if operationErr == nil || !validExpectedState(state) {
		return ErrExpectedError
	}
	// Accept only the direct PostgreSQL operation error, not a wrapped/joined
	// error which might also contain transport, context or cleanup failures.
	postgres, ok := operationErr.(*pgconn.PgError)
	if !ok {
		return boundedDatabaseError(operationErr)
	}
	severity := postgres.SeverityUnlocalized
	if severity == "" {
		severity = postgres.Severity
	}
	if severity == "FATAL" || severity == "PANIC" {
		return ErrFactoryConnection
	}
	if postgres.Code != state {
		return errors.Join(ErrExpectedError, boundedDatabaseError(operationErr))
	}
	return nil
}

// OperationCleanupError separates rollback/close failure from the SQL result.
type OperationCleanupError struct{ Primary, Cleanup error }

func (e *OperationCleanupError) Error() string { return "test operation cleanup failed" }
func (e *OperationCleanupError) Unwrap() []error {
	if e.Primary == nil {
		return []error{e.Cleanup}
	}
	return []error{e.Primary, e.Cleanup}
}
func operationCleanupError(primary, cleanup error) error {
	if cleanup == nil {
		return primary
	}
	return &OperationCleanupError{Primary: primary, Cleanup: cleanup}
}
