package dbtest

import (
	"context"
	"errors"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
)

func TestStrictPostgresScalarTypes(t *testing.T) {
	for _, test := range []struct {
		oid   uint32
		value any
		want  Scalar
	}{
		{pgtype.Int2OID, int16(-2), IntegerScalar(-2)},
		{pgtype.Int4OID, int32(4), IntegerScalar(4)},
		{pgtype.Int8OID, int64(9223372036854775807), IntegerScalar(9223372036854775807)},
		{pgtype.TextOID, "literal", StringScalar("literal")},
		{pgtype.VarcharOID, "literal", StringScalar("literal")},
		{pgtype.BoolOID, true, BooleanScalar(true)},
		{pgtype.Int8OID, nil, NullScalar()},
	} {
		got, err := postgresScalar(test.oid, test.value)
		if err != nil || got != test.want {
			t.Fatalf("oid=%d: %v", test.oid, err)
		}
	}
	for _, test := range []struct {
		oid   uint32
		value any
	}{
		{pgtype.Float8OID, float64(1)}, {pgtype.NumericOID, "9223372036854775808"}, {pgtype.JSONOID, "secret-value"}, {pgtype.ByteaOID, []byte("secret-value")}, {pgtype.TextOID, int64(1)},
	} {
		if _, err := postgresScalar(test.oid, test.value); !errors.Is(err, ErrScalarType) {
			t.Fatalf("oid=%d error=%v", test.oid, err)
		}
	}
	if IntegerScalar(1) == StringScalar("1") || NullScalar() == StringScalar("") || BooleanScalar(false) == NullScalar() {
		t.Fatal("scalar type collapsed")
	}
}

func TestExpectedSQLStateNeverSwallowsInfrastructure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	for _, test := range []struct {
		name  string
		ctx   context.Context
		err   error
		state string
		pass  bool
	}{
		{"exact", context.Background(), &pgconn.PgError{Code: "23505", Message: "secret-value"}, "23505", true},
		{"generic exact", context.Background(), &pgconn.PgError{Code: "22000"}, "22000", true},
		{"wrong", context.Background(), &pgconn.PgError{Code: "23503"}, "23505", false},
		{"none", context.Background(), nil, "23505", false},
		{"context", ctx, &pgconn.PgError{Code: "23505"}, "23505", false},
		{"connection", context.Background(), &pgconn.PgError{Code: "08006"}, "08006", false},
		{"fatal expected code", context.Background(), &pgconn.PgError{Code: "23505", Severity: "FATAL"}, "23505", false},
		{"localized fatal expected code", context.Background(), &pgconn.PgError{Code: "23505", Severity: "localized", SeverityUnlocalized: "FATAL"}, "23505", false},
		{"server cancellation", context.Background(), &pgconn.PgError{Code: "57014"}, "57014", false},
		{"transport", context.Background(), io.EOF, "23505", false},
		{"joined cancellation", context.Background(), errors.Join(&pgconn.PgError{Code: "23505"}, context.Canceled), "23505", false},
		{"joined cleanup", context.Background(), errors.Join(&pgconn.PgError{Code: "23505"}, errors.New("cleanup")), "23505", false},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := checkExpectedSQLState(test.ctx, test.err, test.state)
			if (err == nil) != test.pass {
				t.Fatalf("pass=%v error=%v", test.pass, err)
			}
		})
	}
}
