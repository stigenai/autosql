package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"autosql/pkg/secret"
	"autosql/pkg/zdm/start"
)

func runMigrateStartStatus(parent context.Context, args []string, o output, redactor *secret.Redactor) error {
	fs := newFlags("migrate start-status", o.streams.Err)
	file := fs.String("file", "", "migration start specification JSON")
	urlRef := fs.String("url", "", "database URL secret reference")
	schema := fs.String("state-schema", start.DefaultSchema, "start state schema")
	target := fs.String("target", "", "target identity")
	env := fs.String("env", "", "environment")
	lock := fs.Int("lock-timeout-ms", 1000, "lock timeout")
	timeout := fs.Duration("timeout", time.Minute, "command timeout")
	jsonFlag := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 || *file == "" || *urlRef == "" || *target == "" || *env == "" || *timeout <= 0 || *lock <= 0 {
		return usageError(errors.New("--file, --url, --target, --env, and positive timeouts are required"))
	}
	if strings.Contains(*target, "://") {
		return usageError(errors.New("--target is not a URL"))
	}
	o.json = *jsonFlag
	b, err := os.ReadFile(*file)
	if err != nil {
		return &Error{Kind: "config", Message: "cannot read migration start specification", Code: ExitConfig, Cause: err}
	}
	spec, err := start.ParseJSON(b)
	if err != nil {
		return &Error{Kind: "validation", Message: err.Error(), Code: ExitValidation, Cause: err}
	}
	resolver := secret.NewResolver()
	resolver.Redactor = redactor
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	url, err := resolver.Resolve(ctx, secret.Reference(*urlRef))
	if err != nil {
		return &Error{Kind: "secret", Message: "resolve database URL failed", Code: ExitSecret, Cause: err}
	}
	st, err := start.StatusOf(ctx, start.Config{URL: url, Schema: *schema, Target: *target, Environment: *env, LockTimeoutMS: *lock}, spec)
	if err != nil {
		return &Error{Kind: "validation", Message: redactor.String(err.Error()), Code: ExitValidation, Cause: err}
	}
	human := fmt.Sprintf("migration start %s state=%s phase=%s progress=%d%% versions=%s->%s blockers=%s recovery=%s", st.OperationID, st.State, st.Phase, st.Progress, st.PreviousVersion, st.NewVersion, strings.Join(st.Blockers, "; "), strings.Join(st.RecoveryActions, "; "))
	return o.success(st, human)
}
