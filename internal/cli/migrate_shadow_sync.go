package cli

import (
	"context"
	"errors"
	"os"
	"strconv"
	"strings"
	"time"

	"autosql/pkg/secret"
	"autosql/pkg/zdm/shadowsync"
)

func runMigrateShadowSync(parent context.Context, action string, args []string, o output, redactor *secret.Redactor) error {
	fs := newFlags("migrate shadow-sync "+action, o.streams.Err)
	file := fs.String("file", "", "shadow synchronization spec JSON")
	urlRef := fs.String("url", "", "database URL secret reference")
	target := fs.String("target", "", "target identity")
	env := fs.String("env", "", "environment")
	lock := fs.Int("lock-timeout-ms", 5000, "lock timeout")
	allowLossy := fs.Bool("allow-lossy", false, "authorize lossy transforms")
	allowNonRev := fs.Bool("allow-non-reversible", false, "authorize non-reversible transforms")
	timeout := fs.Duration("timeout", time.Minute, "command timeout")
	jsonFlag := fs.Bool("json", false, "emit JSON")
	if e := fs.Parse(args); e != nil {
		return usageError(e)
	}
	if fs.NArg() != 0 || *file == "" || *urlRef == "" || *target == "" || *env == "" || *lock <= 0 || *timeout <= 0 {
		return usageError(errors.New("--file, --url, --target, --env, and positive timeouts required"))
	}
	if strings.Contains(*target, "://") {
		return usageError(errors.New("--target is not a URL"))
	}
	o.json = *jsonFlag
	b, e := os.ReadFile(*file)
	if e != nil {
		return &Error{Kind: "config", Message: "cannot read shadow synchronization spec", Code: ExitConfig, Cause: e}
	}
	spec, e := shadowsync.ParseJSON(b)
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
	cfg := shadowsync.Config{URL: url, Target: *target, Environment: *env, LockTimeoutMS: *lock}
	policy := shadowsync.Policy{AllowLossy: *allowLossy, AllowNonReversible: *allowNonRev}
	var st shadowsync.Status
	switch action {
	case "apply":
		st, e = shadowsync.Apply(ctx, cfg, spec, policy)
	case "remove":
		st, e = shadowsync.Remove(ctx, cfg, spec, policy)
	case "status":
		st, e = shadowsync.Inspect(ctx, cfg, spec, policy)
	default:
		return usageError(errors.New("shadow-sync action must be apply, remove, or status"))
	}
	if e != nil {
		return &Error{Kind: "validation", Message: redactor.String(e.Error()), Code: ExitValidation, Cause: e}
	}
	human := "shadow synchronization installed=" + strconv.FormatBool(st.Installed) + " rollback_eligible=" + strconv.FormatBool(st.RollbackEligible) + " tables=" + strconv.Itoa(len(st.Tables))
	return o.success(st, human)
}
