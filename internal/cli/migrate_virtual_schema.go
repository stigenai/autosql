package cli

import (
	"context"
	"errors"
	"os"
	"strings"
	"time"

	"autosql/pkg/secret"
	"autosql/pkg/zdm/virtualschema"
)

func runMigrateVirtualSchema(parent context.Context, args []string, o output, redactor *secret.Redactor, apply bool) error {
	name := "migrate virtual-schema-status"
	if apply {
		name = "migrate virtual-schema-apply"
	}
	fs := newFlags(name, o.streams.Err)
	file := fs.String("file", "", "virtual schema specification JSON")
	urlRef := fs.String("url", "", "database URL secret reference")
	target := fs.String("target", "", "stable target identity")
	env := fs.String("env", "", "environment identity")
	lock := fs.Int("lock-timeout-ms", 5000, "schema lock acquisition timeout")
	timeout := fs.Duration("timeout", time.Minute, "command timeout")
	jsonFlag := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 || *file == "" || *urlRef == "" || *target == "" || *env == "" || *timeout <= 0 || *lock <= 0 {
		return usageError(errors.New("--file, --url, --target, --env, positive --lock-timeout-ms and --timeout are required"))
	}
	if strings.Contains(*target, "://") {
		return usageError(errors.New("--target is an identity, not a URL"))
	}
	o.json = *jsonFlag
	b, err := os.ReadFile(*file)
	if err != nil {
		return &Error{Kind: "config", Message: "cannot read virtual schema specification", Code: ExitConfig, Cause: err}
	}
	spec, err := virtualschema.ParseJSON(b)
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
	cfg := virtualschema.Config{URL: url, Target: *target, Environment: *env, LockTimeoutMS: *lock}
	var status virtualschema.Status
	if apply {
		status, err = virtualschema.Apply(ctx, cfg, spec)
	} else {
		status, err = virtualschema.Inspect(ctx, cfg, spec)
	}
	if err != nil {
		return &Error{Kind: "validation", Message: redactor.String(err.Error()), Code: ExitValidation, Cause: err}
	}
	human := "virtual schemas previous=" + status.Previous.Name + " current=" + status.Current.Name + " previous_search_path=" + status.Connection.PreviousSearchPath + " current_search_path=" + status.Connection.CurrentSearchPath
	return o.success(status, human)
}
