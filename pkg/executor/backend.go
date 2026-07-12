package executor

import (
	"context"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type Row interface{ Scan(...any) error }
type Rows interface {
	Next() bool
	Scan(...any) error
	Err() error
	Close()
}
type Tag interface{ RowsAffected() int64 }
type Tx interface {
	Exec(context.Context, string, ...any) (Tag, error)
	QueryRow(context.Context, string, ...any) Row
	Commit(context.Context) error
	Rollback(context.Context) error
}
type Session interface {
	Exec(context.Context, string, ...any) (Tag, error)
	QueryRow(context.Context, string, ...any) Row
	Query(context.Context, string, ...any) (Rows, error)
	Begin(context.Context) (Tx, error)
	Close(context.Context) error
	Raw() *pgx.Conn
}
type Connector interface {
	Connect(context.Context, string) (Session, error)
}
type PGXConnector struct{}

// WrapPGX adapts an already-pinned pgx connection. The executor will not own
// or close it when Config.LockedSession is used.
func WrapPGX(c *pgx.Conn) Session { return pgxSession{c} }
func WrapPGXTx(x pgx.Tx) Tx       { return pgxTx{x} }

// RawPGXTx returns the underlying PostgreSQL transaction for trusted
// inspection code. Other Tx implementations deliberately return nil.
func RawPGXTx(x Tx) pgx.Tx {
	if tx, ok := x.(pgxTx); ok {
		return tx.x
	}
	return nil
}

type borrowedSession struct {
	Session
	tx Tx
}

func (s borrowedSession) Begin(context.Context) (Tx, error) { return borrowedTx{s.tx}, nil }

type borrowedTx struct{ Tx }

func (b borrowedTx) Commit(context.Context) error   { return nil }
func (b borrowedTx) Rollback(context.Context) error { return nil }

func (PGXConnector) Connect(ctx context.Context, url string) (Session, error) {
	c, e := pgx.Connect(ctx, url)
	if e != nil {
		return nil, e
	}
	return pgxSession{c}, nil
}

type pgxSession struct{ c *pgx.Conn }

func (s pgxSession) Exec(c context.Context, q string, a ...any) (Tag, error) {
	return s.c.Exec(c, q, a...)
}
func (s pgxSession) QueryRow(c context.Context, q string, a ...any) Row {
	return s.c.QueryRow(c, q, a...)
}
func (s pgxSession) Query(c context.Context, q string, a ...any) (Rows, error) {
	return s.c.Query(c, q, a...)
}
func (s pgxSession) Begin(c context.Context) (Tx, error) {
	x, e := s.c.Begin(c)
	if e != nil {
		return nil, e
	}
	return pgxTx{x}, nil
}
func (s pgxSession) Close(c context.Context) error { return s.c.Close(c) }
func (s pgxSession) Raw() *pgx.Conn                { return s.c }

type pgxTx struct{ x pgx.Tx }

func (t pgxTx) Exec(c context.Context, q string, a ...any) (Tag, error) { return t.x.Exec(c, q, a...) }
func (t pgxTx) QueryRow(c context.Context, q string, a ...any) Row      { return t.x.QueryRow(c, q, a...) }
func (t pgxTx) Commit(c context.Context) error                          { return t.x.Commit(c) }
func (t pgxTx) Rollback(c context.Context) error                        { return t.x.Rollback(c) }

var _ Tag = pgconn.CommandTag{}
