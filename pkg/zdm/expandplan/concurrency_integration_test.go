package expandplan_test

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
	"time"

	"autosql/pkg/zdm/expandplan"
	"github.com/jackc/pgx/v5"
)

func TestLivePlanningDetectsCompletedCatalogChange(t *testing.T) {
	ctx, c, meta, app, _ := live(t)
	request := expandplan.InspectRequest{URL: trackedPlanningURL(t, c, os.Getenv("AUTOSQL_TEST_POSTGRES_URL")), MetadataSchema: meta, Target: "primary", Environment: "test", ArtifactDigest: "pending", Schemas: []string{app}, Policy: livePolicy()}
	request.BeforeFinalInspection = func(ctx context.Context) error {
		// A new relation need not wait for locks on existing application tables.
		_, err := c.Exec(ctx, "create table "+pgx.Identifier{app, "committed_change"}.Sanitize()+"(id bigint)")
		return err
	}
	if _, err := expandplan.InspectLive(ctx, request); !errors.Is(err, expandplan.ErrRefused) || !strings.Contains(err.Error(), "schema changed at planning fence") {
		t.Fatalf("fresh inspection missed committed catalog change: %v", err)
	}
}

func TestLivePlanningCancellationReleasesSessions(t *testing.T) {
	ctx, c, meta, app, _ := live(t)
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	queued := queuedDDLHook(t, ctx, c, url, app, "accounts")
	request := expandplan.InspectRequest{URL: trackedPlanningURL(t, c, url), MetadataSchema: meta, Target: "primary", Environment: "test", ArtifactDigest: "pending", Schemas: []string{app}, Policy: livePolicy()}
	request.BeforeFinalInspection = func(hold context.Context) error {
		if err := queued(hold); err != nil {
			return err
		}
		cancel()
		return hold.Err()
	}
	started := time.Now()
	if _, err := expandplan.InspectLive(ctx, request); !errors.Is(err, context.Canceled) {
		t.Fatalf("planning cancellation lost: %v", err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatal("planning cancellation exceeded bound")
	}
}

func TestLivePlanningIgnoresUnrelatedActivity(t *testing.T) {
	for _, activity := range []string{"data-write", "queued-other-schema"} {
		t.Run(activity, func(t *testing.T) {
			ctx, c, meta, app, _ := live(t)
			url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
			writer, err := pgx.Connect(ctx, url)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = writer.Close(context.Background()) })
			tx, err := writer.Begin(ctx)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = tx.Rollback(context.Background()) })
			request := expandplan.InspectRequest{URL: trackedPlanningURL(t, c, url), MetadataSchema: meta, Target: "primary", Environment: "test", ArtifactDigest: "pending", Schemas: []string{app}, Policy: livePolicy()}
			if activity == "data-write" {
				request.BeforeFinalInspection = func(ctx context.Context) error {
					_, err := tx.Exec(ctx, "update "+pgx.Identifier{app, "accounts"}.Sanitize()+" set name='changed' where id=1")
					return err
				}
			} else {
				otherSchema := app + "_other"
				t.Cleanup(func() {
					// The queued writer is canceled and joined first. Release this
					// test's own blocker before dropping its unrelated schema.
					_ = tx.Rollback(context.Background())
					_, err := c.Exec(context.Background(), "drop schema if exists "+pgx.Identifier{otherSchema}.Sanitize()+" cascade")
					if err != nil {
						t.Error(err)
					}
				})
				if _, err := c.Exec(ctx, "create schema "+pgx.Identifier{otherSchema}.Sanitize()+"; create table "+pgx.Identifier{otherSchema, "other_table"}.Sanitize()+"(id bigint)"); err != nil {
					t.Fatal(err)
				}
				if _, err := tx.Exec(ctx, "lock table "+pgx.Identifier{otherSchema, "other_table"}.Sanitize()+" in access exclusive mode"); err != nil {
					t.Fatal(err)
				}
				request.BeforeFinalInspection = queuedDDLHook(t, ctx, c, url, otherSchema, "other_table")
			}
			if _, err := expandplan.InspectLive(ctx, request); err != nil {
				t.Fatalf("unrelated activity refused stable planning: %v", err)
			}
		})
	}
}
