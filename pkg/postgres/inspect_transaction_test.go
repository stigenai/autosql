package postgres

import (
	"context"
	"errors"
	"strings"
	"testing"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type unusedCatalogQueryer struct{}

func (unusedCatalogQueryer) Query(context.Context, string, ...any) (pgx.Rows, error) {
	return nil, errors.New("unexpected catalog query")
}

func TestInspectTransactionsRetriesOnlyAfterCleanRollback(t *testing.T) {
	transient := &pgconn.PgError{Code: "XX000", Message: "could not open relation with OID 42"}
	for _, tc := range []struct {
		name     string
		rollback error
	}{
		{name: "rollback success"},
		{name: "transaction already closed", rollback: pgx.ErrTxClosed},
	} {
		t.Run(tc.name, func(t *testing.T) {
			begins, rollbacks, inspections := 0, 0, 0
			begin := func(context.Context) (catalogQueryer, func(context.Context) error, error) {
				begins++
				return unusedCatalogQueryer{}, func(context.Context) error { rollbacks++; return tc.rollback }, nil
			}
			run := func(context.Context, catalogQueryer, plugin.InspectRequest) (schema.Document, error) {
				inspections++
				if inspections == 1 {
					return schema.Document{}, transient
				}
				return schema.Document{Version: schema.SchemaVersion}, nil
			}
			doc, err := inspectTransactions(context.Background(), plugin.InspectRequest{}, begin, run)
			if err != nil || doc.Version != schema.SchemaVersion || begins != 2 || rollbacks != 2 || inspections != 2 {
				t.Fatalf("doc=%+v err=%v begins=%d rollbacks=%d inspections=%d", doc, err, begins, rollbacks, inspections)
			}
		})
	}
}

func TestInspectTransactionsRollbackFailurePreservesInspectionAndStopsRetry(t *testing.T) {
	transient := &pgconn.PgError{Code: "XX000", Message: "cache lookup failed for relation 42"}
	rollbackFailure := errors.New("connection rollback failed")
	begins, inspections := 0, 0
	begin := func(context.Context) (catalogQueryer, func(context.Context) error, error) {
		begins++
		return unusedCatalogQueryer{}, func(context.Context) error { return rollbackFailure }, nil
	}
	run := func(context.Context, catalogQueryer, plugin.InspectRequest) (schema.Document, error) {
		inspections++
		return schema.Document{}, transient
	}
	_, err := inspectTransactions(context.Background(), plugin.InspectRequest{}, begin, run)
	var typed *snapshotRollbackError
	if !errors.Is(err, transient) || !errors.Is(err, rollbackFailure) || !errors.As(err, &typed) {
		t.Fatalf("joined error lost inspection or typed rollback cause: %v", err)
	}
	if begins != 1 || inspections != 1 {
		t.Fatalf("dirty connection retried: begins=%d inspections=%d", begins, inspections)
	}
}

func TestInspectTransactionsNonretryablePreservesOriginal(t *testing.T) {
	original := errors.New("semantic catalog failure")
	begins, inspections := 0, 0
	begin := func(context.Context) (catalogQueryer, func(context.Context) error, error) {
		begins++
		return unusedCatalogQueryer{}, func(context.Context) error { return nil }, nil
	}
	run := func(context.Context, catalogQueryer, plugin.InspectRequest) (schema.Document, error) {
		inspections++
		return schema.Document{}, original
	}
	_, err := inspectTransactions(context.Background(), plugin.InspectRequest{}, begin, run)
	if !errors.Is(err, original) || begins != 1 || inspections != 1 {
		t.Fatalf("original=%v got=%v begins=%d inspections=%d", original, err, begins, inspections)
	}
}

func TestInspectTransactionsSuccessfulInspectionStillRequiresRollback(t *testing.T) {
	rollbackFailure := errors.New("rollback transport failure")
	begin := func(context.Context) (catalogQueryer, func(context.Context) error, error) {
		return unusedCatalogQueryer{}, func(context.Context) error { return rollbackFailure }, nil
	}
	run := func(context.Context, catalogQueryer, plugin.InspectRequest) (schema.Document, error) {
		return schema.Document{Version: schema.SchemaVersion}, nil
	}
	_, err := inspectTransactions(context.Background(), plugin.InspectRequest{}, begin, run)
	var typed *snapshotRollbackError
	if !errors.Is(err, rollbackFailure) || !errors.As(err, &typed) {
		t.Fatalf("rollback failure was masked: %v", err)
	}
}

func TestInspectTransactionsPersistentCatalogDisappearanceExhaustsRetries(t *testing.T) {
	disappeared := &catalogDisappearanceError{resource: "index definition", oid: 42}
	begins, inspections := 0, 0
	begin := func(context.Context) (catalogQueryer, func(context.Context) error, error) {
		begins++
		return unusedCatalogQueryer{}, func(context.Context) error { return nil }, nil
	}
	run := func(context.Context, catalogQueryer, plugin.InspectRequest) (schema.Document, error) {
		inspections++
		return schema.Document{}, classify("indexes", "catalog metadata", "postgres://user:secret@db/app", disappeared)
	}
	_, err := inspectTransactions(context.Background(), plugin.InspectRequest{}, begin, run)
	var got *catalogDisappearanceError
	if !errors.As(err, &got) || !transientCatalogOID(err) || begins != 5 || inspections != 5 {
		t.Fatalf("persistent disappearance err=%v begins=%d inspections=%d", err, begins, inspections)
	}
	if strings.Contains(err.Error(), "secret") || !strings.Contains(err.Error(), "OID 42") {
		t.Fatalf("persistent disappearance was masked or leaked DSN: %v", err)
	}
}
