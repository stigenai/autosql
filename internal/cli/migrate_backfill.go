package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"autosql/pkg/secret"
	"autosql/pkg/zdm/backfill"
)

func runMigrateBackfill(parent context.Context, action string, args []string, o output, redactor *secret.Redactor) error {
	fs := newFlags("migrate backfill "+action, o.streams.Err)
	file := fs.String("file", "", "backfill spec JSON")
	urlRef := fs.String("url", "", "database URL secret reference")
	schema := fs.String("state-schema", backfill.DefaultSchema, "backfill state schema")
	target := fs.String("target", "", "target identity")
	env := fs.String("env", "", "environment")
	batch := fs.Int("batch-size", 100, "rows per batch")
	retries := fs.Int("max-retries", 3, "transient retries")
	lock := fs.Int("lock-timeout-ms", 1000, "lock timeout")
	statement := fs.Int("statement-timeout-ms", 30000, "statement timeout")
	rate := fs.Int("max-rows-per-second", 0, "rate limit, zero disables")
	delay := fs.Duration("delay", 0, "delay between batches")
	backoff := fs.Duration("backoff", 100*time.Millisecond, "retry backoff")
	timeout := fs.Duration("timeout", 10*time.Minute, "command timeout")
	jsonFlag := fs.Bool("json", false, "emit JSON")
	if e := fs.Parse(args); e != nil {
		return usageError(e)
	}
	if fs.NArg() != 0 || *file == "" || *urlRef == "" || *target == "" || *env == "" || *timeout <= 0 {
		return usageError(errors.New("--file, --url, --target, --env, and positive --timeout are required"))
	}
	if strings.Contains(*target, "://") {
		return usageError(errors.New("--target is not a URL"))
	}
	o.json = *jsonFlag
	b, e := os.ReadFile(*file)
	if e != nil {
		return &Error{Kind: "config", Message: "cannot read backfill spec", Code: ExitConfig, Cause: e}
	}
	spec, e := backfill.ParseJSON(b)
	if e != nil {
		return &Error{Kind: "validation", Message: e.Error(), Code: ExitValidation, Cause: e}
	}
	resolver := secret.NewResolver()
	resolver.Redactor = redactor
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	url, e := resolver.Resolve(ctx, secret.Reference(*urlRef))
	if e != nil {
		return &Error{Kind: "secret", Message: "resolve database URL failed", Code: ExitSecret, Cause: e}
	}
	cfg := backfill.Config{URL: url, Schema: *schema, Target: *target, Environment: *env, BatchSize: *batch, MaxRetries: *retries, LockTimeoutMS: *lock, StatementTimeoutMS: *statement, MaxRowsPerSecond: *rate, Delay: *delay, Backoff: *backoff}
	var st backfill.Status
	switch action {
	case "run":
		st, e = backfill.Run(ctx, cfg, spec)
	case "status":
		st, e = backfill.StatusOf(ctx, cfg, spec)
	case "pause", "resume", "cancel":
		st, e = backfill.Control(ctx, cfg, spec, action)
	default:
		return usageError(errors.New("backfill action must be run, status, pause, resume, or cancel"))
	}
	if e != nil {
		return &Error{Kind: "validation", Message: redactor.String(e.Error()), Code: ExitValidation, Cause: e}
	}
	human := fmt.Sprintf("backfill %s state=%s processed=%d remaining=%d throughput=%.2f rows/s retries=%d lag=%ds last_error=%s", st.JobID, st.State, st.Processed, st.Remaining, st.ThroughputRowsPerSecond, st.Retries, st.LagSeconds, st.LastError)
	return o.success(st, human)
}
