package expandplan

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestPlanningInspectionFailureIsBoundedAndActionable(t *testing.T) {
	for _, test := range []struct {
		name  string
		cause error
		want  string
	}{
		{"caller deadline", context.DeadlineExceeded, "inspection timeout"},
		{"server statement timeout", &pgconn.PgError{Code: "57014", Message: "seeded-secret-query"}, "inspection timeout"},
		{"server lock timeout", &pgconn.PgError{Code: "55P03", Message: "seeded-secret-query"}, "inspection timeout"},
		{"unavailable", errors.New("postgres://user:seeded-secret@db/private"), "inspection unavailable"},
		{"cancellation", context.Canceled, "inspection canceled"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := planningInspectionFailure(context.Background(), test.cause)
			if !errors.Is(err, ErrRefused) || !strings.Contains(err.Error(), test.want) || strings.Contains(err.Error(), "seeded-secret") {
				t.Fatalf("unsafe planning diagnostic: %v", err)
			}
			if test.cause == context.Canceled && !errors.Is(err, context.Canceled) {
				t.Fatal("lost caller cancellation")
			}
		})
	}
}
