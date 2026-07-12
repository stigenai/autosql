package cli

import (
	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/migrate"
	"autosql/pkg/plan"
	"autosql/pkg/policy"
	"autosql/pkg/secret"
	"autosql/pkg/simulate"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
)

type generateResult struct {
	Status, Version, File, ManifestDigest, PlanDigest string
	Changes                                           int
}

func runMigrateGenerate(ctx context.Context, args []string, o output, reader ReadPlanService) error {
	fs := newFlags("migrate generate", o.streams.Err)
	dir := fs.String("dir", "", "migration directory")
	from := fs.String("from", "", "current source")
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
	if reader == nil || *dir == "" || *from == "" || *to == "" || *version == "" || *label == "" || *format != "sql" || fs.NArg() != 0 {
		return usageError(errors.New("--dir, --from, --to, --version, --label and --format=sql required"))
	}
	if strings.ToLower(*label) != *label || strings.IndexFunc(*label, func(r rune) bool { return !(r >= 'a' && r <= 'z' || r >= '0' && r <= '9' || r == '-' || r == '_') }) >= 0 {
		return usageError(errors.New("label must be canonical lowercase ASCII"))
	}
	if _, e := migrate.ParseVersion(*version); e != nil {
		return &Error{Kind: "validation", Message: "invalid migration version", Code: ExitValidation}
	}
	snap, e := migrate.LoadSnapshot(*dir)
	if e != nil {
		return &Error{Kind: "validation", Message: "verify migration directory failed", Code: ExitValidation, Cause: e}
	}
	if len(snap.Manifest.Entries) > 0 && snap.Manifest.Entries[len(snap.Manifest.Entries)-1].NonLinear {
		return &Error{Kind: "validation", Message: "generation requires a linear manifest head", Code: ExitValidation}
	}
	current, e := reader.Load(ctx, LoadRequest{Spec: *from})
	if e != nil {
		return &Error{Kind: "validation", Message: "load replayed schema failed", Code: ExitValidation}
	}
	desired, e := reader.Load(ctx, LoadRequest{Spec: *to})
	if e != nil {
		return &Error{Kind: "validation", Message: "load desired schema failed", Code: ExitValidation}
	}
	p, e := reader.Plan(ctx, current, desired)
	if e != nil {
		return &Error{Kind: "validation", Message: "plan migration failed", Code: ExitValidation}
	}
	o.json = *jf
	if len(p.Changes.Changes) == 0 {
		return o.success(generateResult{Status: "no_op", ManifestDigest: snap.Manifest.Digest, PlanDigest: p.Digest}, "no migration generated")
	}
	var ss []string
	for _, s := range p.Steps {
		if s.Kind == plan.StepExecutable {
			ss = append(ss, strings.TrimSpace(s.SQL))
		}
	}
	sql := fmt.Sprintf("-- autosql:transaction=auto\n-- autosql:plan-digest=%s\n-- autosql:check-digest=%s\n-- autosql:bundle-digest=%s\n-- autosql:check-bundle-digest=%s\n-- rename-hints: %s\n%s\n", p.Digest, p.Digest, p.Digest, p.Digest, *hints, strings.Join(ss, ";\n"))
	name := fmt.Sprintf("V%s__%s.sql", *version, *label)
	files := make([]migrate.File, 0, len(snap.Manifest.Entries)+1)
	for _, x := range snap.Manifest.Entries {
		files = append(files, migrate.File{Name: x.File, SQL: append([]byte(nil), snap.Files[x.File]...), Parents: append([]string(nil), x.Parents...), NonLinear: x.NonLinear})
	}
	files = append(files, migrate.File{Name: name, SQL: []byte(sql)})
	man, e := migrate.Update(*dir, migrate.UpdateRequest{Files: files, ManifestVersion: migrate.ManifestVersion, ExpectedManifestDigest: snap.Manifest.Digest})
	if e != nil {
		return &Error{Kind: "validation", Message: "publish migration generation failed", Code: ExitValidation, Cause: e}
	}
	return o.success(generateResult{Status: "generated", Version: *version, File: name, ManifestDigest: man.Digest, PlanDigest: p.Digest, Changes: len(p.Changes.Changes)}, name)
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
	Approval                                                                                                 artifact.Approval
	Lifetime                                                                                                 string
	GeneratorKeyID, GeneratorPurpose, GeneratorPrivateKeyReference, SigningKeyID, SigningPrivateKeyReference string
	Metadata                                                                                                 map[string]string
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
	result, err := (migrate.GenerateService{}).Generate(ctx, migrate.GenerateRequest{Directory: a.dir, Version: a.version, Label: a.label, Format: a.format, RenameHints: a.hints, Desired: desired, DevelopmentURL: dev, DevelopmentIdentity: devID, ProductionIdentity: prodID, Environment: c.Environment, DatabaseIdentity: c.DatabaseIdentity, SourceRevision: c.SourceRevision, Author: c.Author, Requester: c.Requester, PostgresVersion: c.PostgresVersion, Policy: *doc, PolicyIdentity: c.PolicyIdentity, ApprovalPolicy: c.ApprovalPolicy, CreatedAt: now, ExpiresAt: now.Add(lifetime), Approval: c.Approval, GeneratorKeyID: c.GeneratorKeyID, GeneratorPurpose: c.GeneratorPurpose, SigningKeyID: c.SigningKeyID, GeneratorPrivateKey: genKey, SigningPrivateKey: signKey, Metadata: c.Metadata})
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
