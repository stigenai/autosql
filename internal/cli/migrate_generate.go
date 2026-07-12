package cli

import (
	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/migrate"
	"autosql/pkg/policy"
	"autosql/pkg/precheck"
	"autosql/pkg/secret"
	"autosql/pkg/simulate"
	"context"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"time"
)

func runMigrateGenerate(ctx context.Context, args []string, o output, reader ReadPlanService) error {
	fs := newFlags("migrate generate", o.streams.Err)
	dir := fs.String("dir", "", "migration directory")
	_ = fs.String("from", "", "deprecated untrusted current source")
	to := fs.String("to", "", "desired source")
	version := fs.String("version", "", "version")
	label := fs.String("label", "", "label")
	format := fs.String("format", "sql", "format")
	hints := fs.String("rename-hints", "", "rename hints")
	trustedConfig := fs.String("generation-config", "", "trusted generation configuration")
	jf := fs.Bool("json", false, "JSON")
	if e := fs.Parse(args); e != nil {
		return usageError(e)
	}
	if *trustedConfig != "" {
		return runTrustedMigrateGenerate(ctx, trustedGenerateArgs{dir: *dir, to: *to, version: *version, label: *label, format: *format, hints: *hints, json: *jf, config: *trustedConfig}, o, reader)
	}
	return &Error{Kind: "config", Message: "trusted --generation-config is required; untrusted generation cannot publish", Code: ExitConfig}
}

type trustedGenerateArgs struct {
	dir, to, version, label, format, hints, config string
	json                                           bool
}
type generationConfig struct {
	DevelopmentURL, ProductionURL                                                                            secret.Reference
	Environment, DatabaseIdentity, SourceRevision, Author, Requester                                         string
	PostgresVersion                                                                                          int
	PolicyFile, PolicyIdentity                                                                               string
	ApprovalPolicy                                                                                           approval.Policy
	Actors                                                                                                   map[string]approval.Identity
	VerifiedApprovals                                                                                        map[string]approval.VerifiedApproval
	Approvals                                                                                                []approval.Approval
	ApprovalAuditPath                                                                                        string
	Prechecks                                                                                                []precheck.Assertion
	Lifetime                                                                                                 string
	GeneratorKeyID, GeneratorPurpose, GeneratorPrivateKeyReference, SigningKeyID, SigningPrivateKeyReference string
	Metadata                                                                                                 map[string]string
	CheckpointDataPolicy                                                                                     string
	CheckpointDeclaredReplay                                                                                 []string
	CheckpointPolicyApproved                                                                                 bool
	CheckpointExpectedValidationContextDigests                                                               map[string]string
	CheckpointExpectedValidationAttestations                                                                 map[string]artifact.ValidationAttestation
	CheckpointExpectedApprovalIdentity, CheckpointExpectedApprovalProofDigest                                string
	CheckpointReleaseIssuer, CheckpointReleaseIdentity, CheckpointReleasePurpose                             string
}
type configuredGenerationAuthority struct {
	actors   map[string]approval.Identity
	verified map[string]approval.VerifiedApproval
}

func (a configuredGenerationAuthority) ResolveActor(_ context.Context, id string) (approval.Identity, error) {
	v, ok := a.actors[id]
	if !ok {
		return v, errors.New("untrusted actor")
	}
	return v, nil
}
func (a configuredGenerationAuthority) VerifyApproval(_ context.Context, p approval.Approval) (approval.VerifiedApproval, error) {
	v, ok := a.verified[p.Proof]
	if !ok {
		return v, errors.New("untrusted approval proof")
	}
	return v, nil
}

func runTrustedMigrateGenerate(ctx context.Context, a trustedGenerateArgs, o output, reader ReadPlanService) error {
	if reader == nil || a.dir == "" || a.to == "" || a.version == "" || a.label == "" || a.format != "sql" {
		return usageError(errors.New("--dir, --to, --version, --label, --format=sql and --generation-config required"))
	}
	raw, err := os.ReadFile(a.config)
	if err != nil {
		return &Error{Kind: "config", Message: "read generation configuration failed", Code: ExitConfig}
	}
	var c generationConfig
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.DisallowUnknownFields()
	if err = d.Decode(&c); err != nil {
		return &Error{Kind: "config", Message: "parse generation configuration failed", Code: ExitConfig}
	}
	policyRaw, err := os.ReadFile(c.PolicyFile)
	if err != nil {
		return &Error{Kind: "config", Message: "read generation policy failed", Code: ExitConfig}
	}
	doc, err := policy.Parse(policyRaw)
	if err != nil {
		return &Error{Kind: "config", Message: "parse generation policy failed", Code: ExitConfig}
	}
	desired, err := reader.Load(ctx, LoadRequest{Spec: a.to})
	if err != nil {
		return &Error{Kind: "validation", Message: "load desired schema failed", Code: ExitValidation}
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
	genKey, err := decodePrivate(genText)
	if err != nil {
		return &Error{Kind: "config", Message: "decode generator key failed", Code: ExitConfig}
	}
	signText, err := resolver.Resolve(ctx, secret.Reference(c.SigningPrivateKeyReference))
	if err != nil {
		return &Error{Kind: "secret", Message: "resolve signing key failed", Code: ExitSecret}
	}
	signKey, err := decodePrivate(signText)
	if err != nil {
		return &Error{Kind: "config", Message: "decode signing key failed", Code: ExitConfig}
	}
	lifetime, err := time.ParseDuration(c.Lifetime)
	if err != nil || lifetime <= 0 {
		return &Error{Kind: "config", Message: "positive generation lifetime required", Code: ExitConfig}
	}
	now := time.Now().UTC()
	result, err := (migrate.GenerateService{}).Generate(ctx, migrate.GenerateRequest{Directory: a.dir, Version: a.version, Label: a.label, Format: a.format, RenameHints: a.hints, Desired: desired, DevelopmentURL: dev, DevelopmentIdentity: devID, ProductionIdentity: prodID, Environment: c.Environment, DatabaseIdentity: c.DatabaseIdentity, SourceRevision: c.SourceRevision, Author: c.Author, Requester: c.Requester, PostgresVersion: c.PostgresVersion, Policy: *doc, PolicyIdentity: c.PolicyIdentity, ApprovalPolicy: c.ApprovalPolicy, Authority: configuredGenerationAuthority{actors: c.Actors, verified: c.VerifiedApprovals}, ApprovalAudit: &approval.Chain{Sink: &approval.FileSink{Path: c.ApprovalAuditPath}}, Approvals: c.Approvals, PrecheckAssertions: c.Prechecks, CreatedAt: now, ExpiresAt: now.Add(lifetime), GeneratorKeyID: c.GeneratorKeyID, GeneratorPurpose: c.GeneratorPurpose, SigningKeyID: c.SigningKeyID, GeneratorPrivateKey: genKey, SigningPrivateKey: signKey, Metadata: c.Metadata})
	if err != nil {
		code := ExitMigration
		kind := "migration"
		if errors.Is(err, migrate.ErrGenerateConflict) {
			code = ExitConflict
			kind = "conflict"
		}
		return &Error{Kind: kind, Message: err.Error(), Code: code, Cause: err}
	}
	o.json = a.json
	human := result.File
	if result.Status == "no_op" {
		human = "no migration generated"
	}
	return o.success(result, human)
}
