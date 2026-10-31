package dbtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestLanguageLiveScalarAndExpectedErrors(t *testing.T) {
	for _, mode := range []string{"container", "existing-admin"} {
		t.Run(mode, func(t *testing.T) {
			var target *DisposableTarget
			var err error
			if mode == "container" {
				target, err = liveContainerFactory(t).Create(context.Background(), "language")
			} else {
				factory, _ := liveAdminFactory(t)
				target, err = factory.Create(context.Background(), "language")
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := target.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			checkLiveScalars(t, target)
			checkLiveExpectedErrors(t, target)
			ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
			defer cancel()
			if err := target.ExpectError(ctx, "SELECT pg_sleep(20)", "23505", false); !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("deadline was swallowed: %v", err)
			}
		})
	}
}

func TestLanguageLiveExpectedErrorCleanupFailure(t *testing.T) {
	for _, mode := range []string{"container", "existing-admin"} {
		t.Run(mode, func(t *testing.T) {
			var target *DisposableTarget
			var err error
			if mode == "container" {
				target, err = liveContainerFactory(t).Create(context.Background(), "rollback-failure")
			} else {
				factory, _ := liveAdminFactory(t)
				target, err = factory.Create(context.Background(), "rollback-failure")
			}
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := target.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			original := target.cleanupTimeout
			target.cleanupTimeout = time.Nanosecond
			err = target.ExpectError(context.Background(), "SELECT 1/0", "22012", false)
			target.cleanupTimeout = original
			if !errors.Is(err, context.DeadlineExceeded) {
				t.Fatalf("expected SQLSTATE swallowed rollback failure: %v", err)
			}
		})
	}
}

func checkLiveScalars(t *testing.T, target *DisposableTarget) {
	t.Helper()
	ctx := context.Background()
	for _, test := range []struct {
		sql  string
		want Scalar
		args []any
	}{
		{"SELECT $1::bigint", IntegerScalar(9223372036854775807), []any{int64(9223372036854775807)}},
		{"SELECT $1::bigint", IntegerScalar(-9223372036854775808), []any{int64(-9223372036854775808)}},
		{"SELECT $1::text", StringScalar("'; DROP TABLE secret_value; --"), []any{"'; DROP TABLE secret_value; --"}},
		{"SELECT true", BooleanScalar(true), nil},
		{"SELECT NULL::text", NullScalar(), nil},
	} {
		got, err := target.QueryScalar(ctx, test.sql, test.args...)
		if err != nil || got != test.want {
			t.Fatalf("scalar query failed: %v", err)
		}
	}
	for _, sql := range []string{"SELECT 1 WHERE false", "SELECT 1,2", "SELECT generate_series(1,2)"} {
		if _, err := target.QueryScalar(ctx, sql); !errors.Is(err, ErrScalarCardinality) {
			t.Fatalf("cardinality error: %v", err)
		}
	}
	for _, sql := range []string{"SELECT 1.0::float8", "SELECT 9223372036854775808::numeric", "SELECT '{}'::jsonb"} {
		if _, err := target.QueryScalar(ctx, sql); !errors.Is(err, ErrScalarType) {
			t.Fatalf("scalar type error: %v", err)
		}
	}
	if _, err := target.QueryScalar(ctx, "SELECT 9223372036854775808::bigint"); err == nil {
		t.Fatal("numeric overflow passed")
	}
	if _, err := target.QueryScalar(ctx, "SELECT 1; SELECT 2"); err == nil {
		t.Fatal("multiple assertion statements accepted")
	}
	if err := target.ExecParameters(ctx, "CREATE TABLE items(id bigint PRIMARY KEY)"); err != nil {
		t.Fatal(err)
	}
	if err := target.ExecParameters(ctx, "INSERT INTO items VALUES ($1)", int64(1)); err != nil {
		t.Fatal(err)
	}
	if err := target.ExecParameters(ctx, "DELETE FROM items; DROP TABLE items"); err == nil {
		t.Fatal("multiple statements accepted")
	}
}

func checkLiveExpectedErrors(t *testing.T, target *DisposableTarget) {
	t.Helper()
	ctx := context.Background()
	if err := target.ExecParameters(ctx, "CREATE TEMP TABLE main_session_only(id int)"); err != nil {
		t.Fatal(err)
	}
	if err := target.ExpectError(ctx, "SELECT * FROM main_session_only", "42P01", true); err != nil {
		t.Fatalf("nontransactional execution reused the main connection: %v", err)
	}
	for _, fresh := range []bool{false, true} {
		if err := target.ExpectError(ctx, "INSERT INTO items VALUES (2); INSERT INTO items VALUES (1)", "23505", fresh); !errors.Is(err, ErrExpectedError) {
			t.Fatalf("multiple statements disguised as an expected error: %v", err)
		}
		if err := target.ExpectError(ctx, "INSERT INTO items VALUES ($1)", "23505", fresh, int64(1)); err != nil {
			t.Fatal(err)
		}
		got, err := target.QueryScalar(ctx, "SELECT count(*) FROM items")
		if err != nil || got != IntegerScalar(1) {
			t.Fatalf("expected error poisoned next step: %v", err)
		}
		if err := target.ExpectError(ctx, "SELECT 1", "23505", fresh); !errors.Is(err, ErrExpectedError) {
			t.Fatalf("missing expected error: %v", err)
		}
		if err := target.ExpectError(ctx, "SELECT 1/0", "23505", fresh); !errors.Is(err, ErrExpectedError) {
			t.Fatalf("wrong expected error: %v", err)
		}
	}
	if err := target.ExpectError(ctx, "INSERT INTO items VALUES (2)", "23505", false); !errors.Is(err, ErrExpectedError) {
		t.Fatal(err)
	}
	if got, err := target.QueryScalar(ctx, "SELECT count(*) FROM items"); err != nil || got != IntegerScalar(1) {
		t.Fatal("successful expected-error operation was not rolled back")
	}
	err := target.ExpectError(ctx, "DO $$ BEGIN RAISE EXCEPTION 'secret-value' USING ERRCODE='23503'; END $$", "23505", false)
	if err == nil || strings.Contains(err.Error(), "secret-value") {
		t.Fatalf("unbounded failure: %v", err)
	}
	ctx, cancel := context.WithCancel(ctx)
	cancel()
	if err := target.ExpectError(ctx, "SELECT 1", "23505", false); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel swallowed: %v", err)
	}
}
