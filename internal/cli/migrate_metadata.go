package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"autosql/pkg/secret"
	"autosql/pkg/zdm"
)

func metadataStore(ctx context.Context, urlRef, namespace string, redactor *secret.Redactor) (*zdm.Store, error) {
	ref := secret.Reference(urlRef)
	if err := ref.Validate(); err != nil {
		return nil, &Error{Kind: "secret", Message: "--url must be an env:// or file:// secret reference", Code: ExitSecret}
	}
	resolver := secret.NewResolver()
	resolver.Redactor = redactor
	url, err := resolver.Resolve(ctx, ref)
	if err != nil {
		return nil, &Error{Kind: "secret", Message: "resolve metadata database failed", Code: ExitSecret, Cause: err}
	}
	store, err := zdm.Open(zdm.Config{URL: url, Schema: namespace})
	if err != nil {
		return nil, &Error{Kind: "config", Message: err.Error(), Code: ExitConfig, Cause: err}
	}
	return store, nil
}

func runMetadataInit(parent context.Context, args []string, o output, redactor *secret.Redactor) error {
	fs := newFlags("migrate metadata-init", o.streams.Err)
	url := fs.String("url", "", "database URL secret reference")
	namespace := fs.String("metadata-schema", zdm.DefaultSchema, "reserved internal namespace")
	timeout := fs.Duration("timeout", 30*time.Second, "maximum duration")
	jsonFlag := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 || *url == "" || *timeout <= 0 {
		return usageError(errors.New("--url and a positive --timeout are required"))
	}
	o.json = *jsonFlag
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	store, err := metadataStore(ctx, *url, *namespace, redactor)
	if err != nil {
		return err
	}
	if err = store.Init(ctx); err != nil {
		return &Error{Kind: "migration", Message: redactor.String(err.Error()), Code: ExitMigration, Cause: err}
	}
	status, err := store.Status(ctx)
	if err != nil {
		return &Error{Kind: "migration", Message: redactor.String(err.Error()), Code: ExitMigration, Cause: err}
	}
	return o.success(status, fmt.Sprintf("zero-downtime metadata %s initialized at version %d", status.Schema, status.SchemaVersion))
}

func runMetadataStatus(parent context.Context, args []string, o output, redactor *secret.Redactor) error {
	fs := newFlags("migrate metadata-status", o.streams.Err)
	url := fs.String("url", "", "database URL secret reference")
	namespace := fs.String("metadata-schema", zdm.DefaultSchema, "reserved internal namespace")
	timeout := fs.Duration("timeout", 30*time.Second, "maximum duration")
	jsonFlag := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 || *url == "" || *timeout <= 0 {
		return usageError(errors.New("--url and a positive --timeout are required"))
	}
	o.json = *jsonFlag
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	store, err := metadataStore(ctx, *url, *namespace, redactor)
	if err != nil {
		return err
	}
	status, err := store.Status(ctx)
	if err != nil {
		return &Error{Kind: "migration", Message: redactor.String(err.Error()), Code: ExitMigration, Cause: err}
	}
	human := "zero-downtime metadata is not initialized"
	if status.Initialized {
		human = fmt.Sprintf("zero-downtime metadata %s version %d, recovery %s", status.Schema, status.SchemaVersion, status.RecoveryState)
	}
	return o.success(status, human)
}

func runMetadataBaseline(parent context.Context, args []string, o output, redactor *secret.Redactor) error {
	fs := newFlags("migrate metadata-baseline", o.streams.Err)
	url := fs.String("url", "", "database URL secret reference")
	namespace := fs.String("metadata-schema", zdm.DefaultSchema, "reserved internal namespace")
	id := fs.String("id", "", "unique baseline ID")
	target := fs.String("target", "", "stable target identity (not a URL)")
	environment := fs.String("env", "", "environment identity")
	operator := fs.String("operator", "", "operator identity")
	expected := fs.String("expected-fingerprint", "", "required live fingerprint when supplied")
	timeout := fs.Duration("timeout", 2*time.Minute, "maximum duration")
	jsonFlag := fs.Bool("json", false, "emit JSON")
	var schemas stringList
	fs.Var(&schemas, "schema", "application schema to capture (repeatable)")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 || *url == "" || *id == "" || *target == "" || *environment == "" || *operator == "" || len(schemas.values) == 0 || *timeout <= 0 {
		return usageError(errors.New("--url, --id, --target, --env, --operator, at least one --schema, and a positive --timeout are required"))
	}
	if strings.Contains(*target, "://") {
		return usageError(errors.New("--target is a stable identity, not a database URL"))
	}
	o.json = *jsonFlag
	ctx, cancel := context.WithTimeout(parent, *timeout)
	defer cancel()
	store, err := metadataStore(ctx, *url, *namespace, redactor)
	if err != nil {
		return err
	}
	b, err := store.Baseline(ctx, zdm.BaselineRequest{ID: *id, Target: *target, Environment: *environment, Operator: *operator, ExpectedFingerprint: *expected, Schemas: schemas.value()})
	if err != nil {
		return &Error{Kind: "migration", Message: redactor.String(err.Error()), Code: ExitMigration, Cause: err}
	}
	return o.success(b, fmt.Sprintf("baseline %s recorded for %s/%s at %s", b.ID, b.Target, b.Environment, b.Fingerprint))
}
