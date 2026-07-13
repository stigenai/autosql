package compatmatrix

import (
	"strings"
	"testing"
	"time"

	"autosql/pkg/zdm/backfill"
)

func TestLiveBackfillThroughputAndBatchLockBudget(t *testing.T) {
	ctx, url, c, cleanup := live(t)
	defer cleanup()
	_, err := c.Exec(ctx, `create schema mx_app;create table mx_app.items(id bigint primary key,old_name text,new_name text);insert into mx_app.items select g,'NAME'||g,null from generate_series(1,2000) g`)
	if err != nil {
		t.Fatal(err)
	}
	s, err := backfill.New(strings.Repeat("9", 64), "perf", "mx_app", "items", "id", "old_name", "new_name", "lower(value)")
	if err != nil {
		t.Fatal(err)
	}
	cfg := backfill.Config{URL: url, Schema: "mx_backfill", Target: "matrix", Environment: "perf", BatchSize: 100, MaxRetries: 2, LockTimeoutMS: 500, StatementTimeoutMS: 2000, Backoff: time.Millisecond}
	started := time.Now()
	maxBatch := time.Duration(0)
	for {
		at := time.Now()
		st, err := backfill.RunBatch(ctx, cfg, s)
		duration := time.Since(at)
		if duration > maxBatch {
			maxBatch = duration
		}
		if err != nil {
			t.Fatal(err)
		}
		if st.State == "complete" {
			break
		}
	}
	elapsed := time.Since(started)
	throughput := 2000 / elapsed.Seconds()
	if throughput < 50 {
		t.Fatalf("backfill regression: %.2f rows/s", throughput)
	}
	if maxBatch > 2*time.Second {
		t.Fatalf("batch lock/latency regression: %v", maxBatch)
	}
	t.Logf("backfill_rows_per_second=%.2f max_batch_duration_ms=%.2f thresholds=50_rows_per_second,2000_ms", throughput, float64(maxBatch.Microseconds())/1000)
}
