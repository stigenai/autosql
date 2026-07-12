package cli

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"autosql/pkg/secret"
	"autosql/pkg/zdm"
	"autosql/pkg/zdm/expandplan"
	"autosql/pkg/zerodowntime"
)

func runMigrateExpandPlan(parent context.Context, args []string, o output, redactor *secret.Redactor) error {
	fs := newFlags("migrate expand-plan", o.streams.Err)
	file := fs.String("file", "", "signed zero-downtime artifact")
	format := fs.String("format", "json", "json or yaml")
	urlRef := fs.String("url", "", "database URL secret reference")
	keyRef := fs.String("public-key", "", "Ed25519 public key secret reference")
	planKeyRef := fs.String("plan-signing-key", "", "Ed25519 private plan signing key secret reference")
	planKeyID := fs.String("plan-key-id", "", "trusted plan signer key identity")
	metadata := fs.String("metadata-schema", zdm.DefaultSchema, "reserved metadata schema")
	target := fs.String("target", "", "stable target identity")
	environment := fs.String("env", "", "environment identity")
	expected := fs.String("expected-fingerprint", "", "required live fingerprint")
	maxLock := fs.Int("max-lock-ms", 5000, "maximum lock duration")
	maxStatement := fs.Int("max-statement-ms", 300000, "maximum statement duration")
	maxTx := fs.Int("max-transaction-ms", 300000, "maximum transaction duration")
	allowScan := fs.Bool("allow-table-scan", false, "allow table scans")
	allowValidation := fs.Bool("allow-validation-scan", false, "allow validation scans")
	allowNonTx := fs.Bool("allow-nontransactional", false, "allow nontransactional steps")
	allowRewrite := fs.Bool("allow-rewrite", false, "allow table rewrites")
	allowMaintenance := fs.Bool("allow-maintenance-required", false, "allow maintenance-required operations")
	timeout := fs.Duration("timeout", 2*time.Minute, "maximum planning duration")
	jsonFlag := fs.Bool("json", false, "emit JSON")
	var schemas stringList
	fs.Var(&schemas, "schema", "application schema (repeatable)")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 || *file == "" || *urlRef == "" || *keyRef == "" || *planKeyRef == "" || *planKeyID == "" || *target == "" || *environment == "" || *expected == "" || len(schemas.values) == 0 || *timeout <= 0 {
		return usageError(errors.New("--file, --url, --public-key, --plan-signing-key, --plan-key-id, --target, --env, --expected-fingerprint, at least one --schema, and positive --timeout are required"))
	}
	if strings.Contains(*target, "://") {
		return usageError(errors.New("--target is an identity, not a URL"))
	}
	o.json = *jsonFlag
	b, err := os.ReadFile(*file)
	if err != nil {
		return &Error{Kind: "config", Message: "cannot read migration artifact", Code: ExitConfig, Cause: err}
	}
	var m zerodowntime.Migration
	if *format == "json" {
		m, err = zerodowntime.ParseJSON(b)
	} else if *format == "yaml" {
		m, err = zerodowntime.ParseYAML(b)
	} else {
		return usageError(errors.New("--format must be json or yaml"))
	}
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
	rawKey, err := resolver.Resolve(ctx, secret.Reference(*keyRef))
	if err != nil {
		return &Error{Kind: "secret", Message: "resolve public key failed", Code: ExitSecret, Cause: err}
	}
	kb, err := base64.RawStdEncoding.Strict().DecodeString(strings.TrimSpace(rawKey))
	if err != nil || len(kb) != ed25519.PublicKeySize {
		return &Error{Kind: "secret", Message: "public key must be raw-base64 Ed25519 public key", Code: ExitSecret}
	}
	if err = m.Verify(ed25519.PublicKey(kb)); err != nil {
		return &Error{Kind: "validation", Message: "artifact signature verification failed", Code: ExitValidation, Cause: err}
	}
	rawPlanKey, err := resolver.Resolve(ctx, secret.Reference(*planKeyRef))
	if err != nil {
		return &Error{Kind: "secret", Message: "resolve plan signing key failed", Code: ExitSecret, Cause: err}
	}
	planKey, err := base64.RawStdEncoding.Strict().DecodeString(strings.TrimSpace(rawPlanKey))
	if err != nil || len(planKey) != ed25519.PrivateKeySize {
		return &Error{Kind: "secret", Message: "plan signing key must be raw-base64 Ed25519 private key", Code: ExitSecret}
	}
	snap, err := expandplan.InspectLive(ctx, expandplan.InspectRequest{URL: url, MetadataSchema: *metadata, Target: *target, Environment: *environment, ArtifactDigest: m.Digest, Schemas: schemas.value()})
	if err != nil {
		return &Error{Kind: "validation", Message: redactor.String(err.Error()), Code: ExitValidation, Cause: err}
	}
	req := expandplan.Request{Migration: m, Snapshot: snap, ExpectedFingerprint: *expected, Target: *target, Environment: *environment, Policy: expandplan.Policy{MaxLockMS: *maxLock, MaxStatementMS: *maxStatement, MaxTransactionMS: *maxTx, AllowRewrite: *allowRewrite, AllowTableScan: *allowScan, AllowValidationScan: *allowValidation, AllowNonTransactional: *allowNonTx, AllowMaintenanceRequired: *allowMaintenance}, Verify: func(x zerodowntime.Migration) error { return x.Verify(ed25519.PublicKey(kb)) }, PlanKeyID: *planKeyID, PlanSigner: ed25519.PrivateKey(planKey)}
	p, err := expandplan.Build(req)
	if err != nil {
		return &Error{Kind: "validation", Message: redactor.String(err.Error()), Code: ExitValidation, Cause: err}
	}
	if err = p.VerifyTrusted(req, ed25519.PrivateKey(planKey).Public().(ed25519.PublicKey)); err != nil {
		return &Error{Kind: "validation", Message: "generated plan attestation verification failed", Code: ExitValidation, Cause: err}
	}
	return o.success(p, fmt.Sprintf("expand plan %s: %d read-only planned steps for %s/%s", p.Digest, len(p.Steps), p.Target, p.Environment))
}
