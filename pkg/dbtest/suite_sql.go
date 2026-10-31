package dbtest

import (
	"context"

	"autosql/pkg/config"
	"autosql/pkg/source"
	"github.com/jackc/pgx/v5"
)

type sqlBatch struct {
	statements    []source.Statement
	transactional bool
}

func statementOperation(statement source.Statement) Operation {
	at := statement.Position
	return Operation{Range: config.SourceRange{Filename: at.File, StartLine: at.Line, StartColumn: at.Column, EndLine: at.Line, EndColumn: at.Column}}
}

func (d *DisposableTarget) executeBatch(ctx context.Context, batch sqlBatch, stage string) (*CaseFailure, []CaseFailure) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.beginUse(); err != nil {
		failure := testFailure(stage, Operation{}, err)
		return &failure, nil
	}
	ctx, cancel := context.WithDeadline(ctx, d.lease.record.Deadline)
	defer cancel()
	if !batch.transactional {
		return executeStatements(ctx, d.conn, batch.statements, stage), nil
	}
	return d.executeTransaction(ctx, batch.statements, stage)
}

func executeStatements(ctx context.Context, conn *pgx.Conn, statements []source.Statement, stage string) *CaseFailure {
	for _, statement := range statements {
		if err := ctx.Err(); err != nil {
			failure := testFailure(stage, statementOperation(statement), err)
			return &failure
		}
		if err := executeTestStatement(ctx, conn, statement.SQL, nil); err != nil {
			failure := testFailure(stage, statementOperation(statement), boundedDatabaseError(err))
			return &failure
		}
	}
	return nil
}

func (d *DisposableTarget) executeTransaction(ctx context.Context, statements []source.Statement, stage string) (*CaseFailure, []CaseFailure) {
	tx, err := d.conn.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		failure := testFailure(stage, Operation{}, boundedDatabaseError(err))
		return &failure, nil
	}
	primary := executeStatements(ctx, tx.Conn(), statements, stage)
	if primary == nil {
		if err := tx.Commit(ctx); err != nil {
			failure := testFailure(stage, Operation{}, boundedDatabaseError(err))
			return &failure, nil
		}
		return nil, nil
	}
	cleanupCtx, cancel := freshSetupContext(ctx, d.cleanupTimeout)
	defer cancel()
	if err := tx.Rollback(cleanupCtx); err != nil {
		return primary, []CaseFailure{testFailure("rollback", Operation{}, boundedDatabaseError(err))}
	}
	return primary, nil
}

func (d *DisposableTarget) executeCleanup(ctx context.Context, op Operation) (result error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ErrLease
	}
	conn := d.conn
	if conn.IsClosed() {
		var err error
		conn, err = pgx.ConnectConfig(ctx, d.conn.Config().Copy())
		if err != nil {
			return boundedDatabaseError(err)
		}
		defer func() {
			closeCtx, cancel := freshSetupContext(ctx, d.cleanupTimeout)
			defer cancel()
			result = operationCleanupError(result, boundedDatabaseError(conn.Close(closeCtx)))
		}()
		if err := d.verifyFreshFixture(ctx, conn); err != nil {
			return err
		}
	}
	return boundedDatabaseError(executeTestStatement(ctx, conn, op.SQL, scalarParameters(op.Params)))
}
