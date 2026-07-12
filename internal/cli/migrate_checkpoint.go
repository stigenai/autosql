package cli

import (
	"autosql/pkg/approval"
	"autosql/pkg/migrate"
	"autosql/pkg/policy"
	"autosql/pkg/schema"
	"autosql/pkg/secret"
	"autosql/pkg/simulate"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
)

func runMigrateCheckpoint(ctx context.Context, args []string, o output) error {
	if len(args) == 0 {
		return usageError(errors.New("checkpoint create or verify required"))
	}
	switch args[0] {
	case "create":
		return runMigrateCheckpointCreate(ctx, args[1:], o)
	case "verify":
		return runMigrateCheckpointVerify(args[1:], o)
	default:
		return usageError(errors.New("checkpoint create or verify required"))
	}
}

func runMigrateCheckpointVerify(args []string, o output) error {
	fs := newFlags("migrate checkpoint verify", o.streams.Err)
	dir := fs.String("dir", "", "migration directory")
	jf := fs.Bool("json", false, "JSON")
	if err := fs.Parse(args); err != nil || *dir == "" || fs.NArg() != 0 {
		return usageError(errors.New("--dir required"))
	}
	r, err := migrate.VerifyCheckpoints(*dir)
	if err != nil {
		return &Error{Kind: "migration", Message: "checkpoint verification failed", Code: ExitMigration, Cause: err}
	}
	o.json = *jf
	return o.success(r, "checkpoint history verified")
}

func runMigrateCheckpointCreate(ctx context.Context, args []string, o output) error {
	fs := newFlags("migrate checkpoint create", o.streams.Err)
	dir := fs.String("dir", "", "migration directory")
	version := fs.String("version", "", "version")
	label := fs.String("label", "checkpoint", "label")
	cfg := fs.String("generation-config", "", "trusted generation configuration")
	data := fs.String("data-policy", "schema_only", "schema_only or declared_replay")
	jf := fs.Bool("json", false, "JSON")
	var replay stringList
	fs.Var(&replay, "declare-replay", "covered data migration version")
	if err := fs.Parse(args); err != nil || *dir == "" || *version == "" || *cfg == "" || fs.NArg() != 0 {
		return usageError(errors.New("--dir, --version and --generation-config required"))
	}
	raw, err := os.ReadFile(*cfg)
	if err != nil {
		return &Error{Kind: "config", Message: "read generation configuration failed", Code: ExitConfig}
	}
	var c generationConfig
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return &Error{Kind: "config", Message: "parse generation configuration failed", Code: ExitConfig}
	}
	if c.CheckpointDataPolicy != *data || strings.Join(c.CheckpointDeclaredReplay, "\x00") != strings.Join(replay.value(), "\x00") {
		return &Error{Kind: "config", Message: "checkpoint data policy must match trusted generation configuration", Code: ExitConfig}
	}
	policyRaw, err := os.ReadFile(c.PolicyFile)
	if err != nil {
		return &Error{Kind: "config", Message: "read generation policy failed", Code: ExitConfig}
	}
	pd, err := policy.Parse(policyRaw)
	if err != nil {
		return &Error{Kind: "config", Message: "parse generation policy failed", Code: ExitConfig}
	}
	resolver := secret.NewResolver()
	dev, err := resolver.Resolve(ctx, c.DevelopmentURL)
	if err != nil {
		return &Error{Kind: "secret", Message: "resolve development database failed", Code: ExitSecret}
	}
	prod, err := resolver.Resolve(ctx, c.ProductionURL)
	if err != nil {
		return &Error{Kind: "secret", Message: "resolve production database failed", Code: ExitSecret}
	}
	devID, err := simulate.ResolvePostgresIdentity(ctx, dev)
	if err != nil {
		return &Error{Kind: "connection", Message: "resolve development identity failed", Code: ExitConnection}
	}
	prodID, err := simulate.ResolvePostgresIdentity(ctx, prod)
	if err != nil {
		return &Error{Kind: "connection", Message: "resolve production identity failed", Code: ExitConnection}
	}
	genText, err := resolver.Resolve(ctx, secret.Reference(c.GeneratorPrivateKeyReference))
	if err != nil {
		return &Error{Kind: "secret", Message: "resolve generator key failed", Code: ExitSecret}
	}
	gen, err := decodePrivate(genText)
	if err != nil {
		return &Error{Kind: "config", Message: "decode generator key failed", Code: ExitConfig}
	}
	signText, err := resolver.Resolve(ctx, secret.Reference(c.SigningPrivateKeyReference))
	if err != nil {
		return &Error{Kind: "secret", Message: "resolve signing key failed", Code: ExitSecret}
	}
	sign, err := decodePrivate(signText)
	if err != nil {
		return &Error{Kind: "config", Message: "decode signing key failed", Code: ExitConfig}
	}
	lifetime, err := time.ParseDuration(c.Lifetime)
	if err != nil || lifetime <= 0 {
		return &Error{Kind: "config", Message: "positive generation lifetime required", Code: ExitConfig}
	}
	now := time.Now().UTC()
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	r, err := (migrate.GenerateService{}).CreateCheckpoint(ctx, migrate.CheckpointRequest{GenerateRequest: migrate.GenerateRequest{Directory: *dir, Version: *version, Label: *label, Format: "sql", Desired: empty, DevelopmentURL: dev, DevelopmentIdentity: devID, ProductionIdentity: prodID, Environment: c.Environment, DatabaseIdentity: c.DatabaseIdentity, SourceRevision: c.SourceRevision, Author: c.Author, Requester: c.Requester, PostgresVersion: c.PostgresVersion, Policy: *pd, PolicyIdentity: c.PolicyIdentity, ApprovalPolicy: c.ApprovalPolicy, Authority: configuredGenerationAuthority{actors: c.Actors, verified: c.VerifiedApprovals}, ApprovalAudit: &approval.Chain{Sink: &approval.FileSink{Path: c.ApprovalAuditPath}}, Approvals: c.Approvals, PrecheckAssertions: c.Prechecks, CreatedAt: now, ExpiresAt: now.Add(lifetime), GeneratorKeyID: c.GeneratorKeyID, GeneratorPurpose: c.GeneratorPurpose, SigningKeyID: c.SigningKeyID, GeneratorPrivateKey: gen, SigningPrivateKey: sign, Metadata: c.Metadata}, DataPolicy: *data, DeclaredReplay: replay.value(), PolicyApproved: c.CheckpointPolicyApproved})
	if err != nil {
		code := ExitMigration
		kind := "migration"
		if errors.Is(err, migrate.ErrGenerateConflict) {
			code = ExitConflict
			kind = "conflict"
		}
		return &Error{Kind: kind, Message: err.Error(), Code: code, Cause: err}
	}
	o.json = *jf
	return o.success(r, r.File)
}
