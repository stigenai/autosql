package compatmatrix

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"autosql/pkg/zdm/start"
	"github.com/jackc/pgx/v5"
)

func TestLiveCancellationRestartAtEveryStartPhase(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL unset")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	c, e := pgx.Connect(ctx, url)
	if e != nil {
		t.Fatal(e)
	}
	defer c.Close(ctx)
	phases := []string{"validate", "record_intent", "expand", "compatibility", "backfill", "publish"}
	for i, phase := range phases {
		t.Run(phase, func(t *testing.T) {
			schema := fmt.Sprintf("mx_fault_%d", i)
			_, _ = c.Exec(ctx, "drop schema if exists "+pgx.Identifier{schema}.Sanitize()+" cascade")
			defer c.Exec(context.Background(), "drop schema if exists "+pgx.Identifier{schema}.Sanitize()+" cascade")
			s, e := start.New(fmt.Sprintf("release_%d", i), strings.Repeat(fmt.Sprintf("%x", i+1), 64)[:64], "v1", "v2")
			if e != nil {
				t.Fatal(e)
			}
			noop := func(context.Context) error { return nil }
			actions := start.Actions{Validate: noop, Expand: noop, Compatibility: noop, Backfill: noop, Publish: noop}
			injected := errors.New("injected cancellation")
			st, e := start.StartWithHooks(ctx, start.Config{URL: url, Schema: schema, Target: "matrix", Environment: "fault", LockTimeoutMS: 500}, s, actions, start.Hooks{AfterPhase: func(got string) error {
				if got == phase {
					return injected
				}
				return nil
			}})
			if !errors.Is(e, injected) || st.State != "interrupted" {
				t.Fatalf("phase %s not classified: %+v %v", phase, st, e)
			}
			st, e = start.Start(ctx, start.Config{URL: url, Schema: schema, Target: "matrix", Environment: "fault", LockTimeoutMS: 500}, s, actions)
			if e != nil || st.State != "complete" || st.Progress != 100 {
				t.Fatalf("phase %s did not resume: %+v %v", phase, st, e)
			}
		})
	}
}
