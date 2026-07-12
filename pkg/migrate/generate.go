package migrate

import (
	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/guardrail"
	"autosql/pkg/plan"
	"autosql/pkg/policy"
	"autosql/pkg/postgres"
	"autosql/pkg/precheck"
	"autosql/pkg/safety"
	"autosql/pkg/schema"
	"autosql/pkg/simulate"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

var (
	ErrGenerateConflict = errors.New("migration generation conflict")
	ErrGenerateConfig   = errors.New("migration generation configuration invalid")
	ErrGenerateStage    = errors.New("migration generation stage failed")
)

// GenerateError is deliberately value-free: database URLs, SQL and policy
// inputs are never retained in errors returned across the CLI boundary.
type GenerateError struct {
	Stage string
	Kind  error
}

func (e *GenerateError) Error() string { return fmt.Sprintf("%v: %s", e.Kind, e.Stage) }
func (e *GenerateError) Unwrap() error { return e.Kind }
func generationFailure(stage string, kind error) error {
	return &GenerateError{Stage: stage, Kind: kind}
}

type GenerateRequest struct {
	Directory, Version, Label, Format, RenameHints                   string
	Desired                                                          schema.Document
	DevelopmentURL, DevelopmentIdentity, ProductionIdentity          string
	Environment, DatabaseIdentity, SourceRevision, Author, Requester string
	PostgresVersion                                                  int
	Policy                                                           policy.Document
	PolicyIdentity                                                   string
	ApprovalPolicy                                                   approval.Policy
	CreatedAt, ExpiresAt                                             time.Time
	Approval                                                         artifact.Approval
	GeneratorKeyID, GeneratorPurpose, SigningKeyID                   string
	GeneratorPrivateKey, SigningPrivateKey                           ed25519.PrivateKey
	Metadata                                                         map[string]string
	Stage                                                            func(string) error
}

type GenerateResult struct {
	Status, File, ArtifactFile, ManifestDigest, PlanDigest, ChecksDigest, BundleDigest string
	Changes                                                                            int
}

type GenerateService struct{ Ops Ops }

func validateMigrationArtifact(raw []byte) error { _, err := artifact.Parse(raw); return err }

func (s GenerateService) checkpoint(r GenerateRequest, stage string) error {
	if r.Stage != nil {
		if err := r.Stage(stage); err != nil {
			return generationFailure(stage, ErrGenerateStage)
		}
	}
	return nil
}

func (s GenerateService) Generate(ctx context.Context, r GenerateRequest) (GenerateResult, error) {
	var out GenerateResult
	if err := validateGenerateRequest(r); err != nil {
		return out, err
	}
	if err := s.checkpoint(r, "snapshot"); err != nil {
		return out, err
	}
	snap, err := LoadSnapshot(r.Directory)
	if err != nil {
		return out, generationFailure("snapshot", ErrGenerateConflict)
	}
	if err = linearHead(snap.Manifest); err != nil {
		return out, err
	}
	if err = s.checkpoint(r, "replay"); err != nil {
		return out, err
	}
	current, err := replaySnapshot(ctx, snap, r)
	if err != nil {
		return out, generationFailure("replay", ErrGenerateStage)
	}
	current, err = postgres.New().Normalize(ctx, current)
	if err != nil {
		return out, generationFailure("replay_fingerprint", ErrGenerateStage)
	}
	desired, err := postgres.New().Normalize(ctx, r.Desired)
	if err != nil {
		return out, generationFailure("desired", ErrGenerateConfig)
	}
	fromFP, err := schema.SemanticFingerprint(current)
	if err != nil {
		return out, generationFailure("replay_fingerprint", ErrGenerateStage)
	}
	toFP, err := schema.SemanticFingerprint(desired)
	if err != nil {
		return out, generationFailure("desired_fingerprint", ErrGenerateConfig)
	}
	if fromFP == toFP {
		return GenerateResult{Status: "no_op", ManifestDigest: snap.Manifest.Digest}, nil
	}
	if err = s.checkpoint(r, "plan"); err != nil {
		return out, err
	}
	p, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{})
	if err != nil {
		return out, generationFailure("plan", ErrGenerateStage)
	}
	if p.FromFingerprint != fromFP || p.ToFingerprint != toFP || len(p.Changes.Changes) == 0 {
		return out, generationFailure("plan_binding", ErrGenerateStage)
	}
	if err = s.checkpoint(r, "simulate"); err != nil {
		return out, err
	}
	sim, err := simulate.Run(ctx, simulate.PostgresFactory{NamePrefix: "autosql_sim_generate"}, simulate.Request{Config: simulate.Config{DevelopmentURL: r.DevelopmentURL, DevelopmentIdentity: r.DevelopmentIdentity, ProductionIdentity: r.ProductionIdentity, CleanupTimeout: 20 * time.Second}, From: current, Plan: p})
	if err != nil || !sim.Verified || sim.ToFingerprint != toFP {
		return out, generationFailure("simulate", ErrGenerateStage)
	}
	statements := executableStatements(p)
	checks, err := generationChecks(p, statements)
	if err != nil {
		return out, generationFailure("prechecks", ErrGenerateStage)
	}
	g := generationGuardrail(r)
	si := safety.Input{Changes: p.Changes, Statements: p.SafetyStatements(), Target: safety.Target{Engine: "postgresql", Version: r.PostgresVersion}}
	if err = s.checkpoint(r, "safety"); err != nil {
		return out, err
	}
	diagnostics, err := g.Safety.Run(ctx, si)
	if err != nil {
		return out, generationFailure("safety", ErrGenerateStage)
	}
	for _, d := range diagnostics {
		if d.Suppressed == nil && d.Severity == safety.SeverityError {
			return out, generationFailure("safety", ErrGenerateStage)
		}
	}
	if err = s.checkpoint(r, "policy"); err != nil {
		return out, err
	}
	violations, err := g.Policy.Evaluate(ctx, r.Policy, schemaPolicyResources(desired), migrationPolicyResources(p))
	if err != nil || len(violations) != 0 {
		return out, generationFailure("policy", ErrGenerateStage)
	}
	bindings, err := guardrail.BuildStatementBindings(p.Changes, si.Statements)
	if err != nil {
		return out, generationFailure("guardrail_bindings", ErrGenerateStage)
	}
	in := guardrail.Input{Changes: p.Changes, Safety: si, Policy: r.Policy, PolicyIdentity: r.PolicyIdentity, SchemaResources: schemaPolicyResources(desired), MigrationResources: migrationPolicyResources(p), Precheck: checks, Approval: approval.Request{Plan: approval.Plan{Environment: r.Environment, Author: r.Author, ExpiresAt: r.ExpiresAt}, RequestedBy: r.Requester}, StatementBindings: bindings}
	if err = s.checkpoint(r, "guardrail"); err != nil {
		return out, err
	}
	bundle, err := g.BundleDigest(in)
	if err != nil {
		return out, generationFailure("guardrail", ErrGenerateStage)
	}
	in.Approval.Plan.Digest = bundle
	created := r.CreatedAt.UTC()
	metadata := cloneStrings(r.Metadata)
	metadata["autosql.migration.manifest"] = snap.Manifest.Digest
	metadata["autosql.migration.from"] = fromFP
	metadata["autosql.migration.to"] = toFP
	metadata["autosql.migration.rename_hints"] = r.RenameHints
	if err = s.checkpoint(r, "artifact"); err != nil {
		return out, err
	}
	a, err := artifact.NewGenerated(p, checks, created, r.ExpiresAt.UTC(), r.SourceRevision, r.Environment, r.DatabaseIdentity, bundle, r.Approval, metadata, r.GeneratorKeyID, r.GeneratorPurpose, r.GeneratorPrivateKey)
	if err != nil {
		return out, generationFailure("artifact", ErrGenerateStage)
	}
	if err = a.Sign(r.SigningKeyID, r.SigningPrivateKey); err != nil {
		return out, generationFailure("sign", ErrGenerateStage)
	}
	artifactBytes, err := a.MarshalCanonical()
	if err != nil {
		return out, generationFailure("artifact_encode", ErrGenerateStage)
	}
	sql := renderGeneratedSQL(p, checks.Digest, bundle, r.RenameHints)
	name := fmt.Sprintf("V%s__%s.sql", r.Version, r.Label)
	files := snapshotFiles(snap)
	artifactName := name + ".artifact.json"
	files = append(files, File{Name: name, SQL: sql, ArtifactName: artifactName, Artifact: artifactBytes})
	if err = s.checkpoint(r, "publish"); err != nil {
		return out, err
	}
	man, err := UpdateWithOps(r.Directory, UpdateRequest{Files: files, ManifestVersion: ManifestVersion, ExpectedManifestDigest: snap.Manifest.Digest}, s.Ops)
	if err != nil {
		return out, generationFailure("publish", ErrGenerateConflict)
	}
	return GenerateResult{Status: "generated", File: name, ArtifactFile: artifactName, ManifestDigest: man.Digest, PlanDigest: p.Digest, ChecksDigest: checks.Digest, BundleDigest: bundle, Changes: len(p.Changes.Changes)}, nil
}

func validateGenerateRequest(r GenerateRequest) error {
	if r.Directory == "" || r.Version == "" || r.Label == "" || r.Format != "sql" || r.DevelopmentURL == "" || r.DevelopmentIdentity == "" || r.ProductionIdentity == "" || r.DevelopmentIdentity == r.ProductionIdentity || r.Environment == "" || r.DatabaseIdentity == "" || r.SourceRevision == "" || r.Author == "" || r.Requester == "" || r.Author == r.Requester || r.PolicyIdentity == "" || r.PostgresVersion <= 0 || r.CreatedAt.IsZero() || !r.ExpiresAt.After(r.CreatedAt) || len(r.GeneratorPrivateKey) != ed25519.PrivateKeySize || len(r.SigningPrivateKey) != ed25519.PrivateKeySize || r.GeneratorKeyID == "" || r.GeneratorPurpose == "" || r.SigningKeyID == "" {
		return generationFailure("config", ErrGenerateConfig)
	}
	if strings.ToLower(r.Label) != r.Label || strings.IndexFunc(r.Label, func(x rune) bool { return !(x >= 'a' && x <= 'z' || x >= '0' && x <= '9' || x == '-' || x == '_') }) >= 0 {
		return generationFailure("label", ErrGenerateConfig)
	}
	if _, err := ParseVersion(r.Version); err != nil {
		return generationFailure("version", ErrGenerateConfig)
	}
	if err := r.Desired.Validate(); err != nil || len(r.Policy.Rules) == 0 {
		return generationFailure("desired_or_policy", ErrGenerateConfig)
	}
	return nil
}

func linearHead(m Manifest) error {
	if len(m.Entries) > 0 {
		e := m.Entries[len(m.Entries)-1]
		if e.NonLinear || len(e.Parents) > 1 {
			return generationFailure("branch_divergence", ErrGenerateConflict)
		}
	}
	return nil
}
func executableStatements(p plan.Plan) []string {
	var x []string
	for _, s := range p.Steps {
		if s.Kind == plan.StepExecutable {
			x = append(x, s.SQL)
		}
	}
	return x
}
func generationChecks(p plan.Plan, statements []string) (precheck.Plan, error) {
	cd, e := guardrail.ChangeDigest(p.Changes)
	if e != nil {
		return precheck.Plan{}, e
	}
	c := precheck.Plan{ID: "generate:" + p.Digest, ChangeDigest: cd, Statements: statements}
	c.Digest, e = precheck.Digest(c)
	return c, e
}
func generationGuardrail(r GenerateRequest) guardrail.Guardrail {
	return guardrail.Guardrail{Config: guardrail.Config{Environment: r.Environment, FailOn: safety.SeverityError, Risk: guardrail.RiskConfig{Baseline: approval.RiskLow}}, Safety: safety.Runner{Analyzers: safety.Builtins()}, Policy: policy.Evaluator{}, Approval: approval.Gate{Policy: r.ApprovalPolicy}}
}
func schemaPolicyResources(d schema.Document) []policy.Resource {
	out := make([]policy.Resource, 0, len(d.Graph.Resources))
	for _, x := range d.Graph.Resources {
		attributes := map[string]any{"id": x.ID, "catalog": x.Name.Catalog, "schema": x.Name.Schema, "parent": x.Name.Parent, "annotations": jsonValue(x.Annotations), "dependencies": jsonValue(x.Dependencies)}
		if len(x.Spec) > 0 {
			var spec map[string]any
			dec := json.NewDecoder(strings.NewReader(string(x.Spec)))
			dec.UseNumber()
			if dec.Decode(&spec) == nil {
				for k, v := range spec {
					attributes[k] = v
				}
			}
		}
		out = append(out, policy.Resource{Kind: string(x.Kind), Name: x.Name.String(), Attributes: attributes})
	}
	return out
}
func migrationPolicyResources(p plan.Plan) []policy.Resource {
	out := make([]policy.Resource, 0, len(p.Changes.Changes))
	for _, x := range p.Changes.Changes {
		out = append(out, policy.Resource{Kind: string(x.Operation), Name: x.ID, Attributes: map[string]any{"change_id": x.ID, "resource_id": x.ResourceID, "depends_on": jsonValue(x.DependsOn), "before": jsonValue(x.Before), "after": jsonValue(x.After)}})
	}
	return out
}
func jsonValue(v any) any {
	raw, err := json.Marshal(v)
	if err != nil {
		return nil
	}
	var out any
	d := json.NewDecoder(strings.NewReader(string(raw)))
	d.UseNumber()
	if d.Decode(&out) != nil {
		return nil
	}
	return out
}
func snapshotFiles(s Snapshot) []File {
	out := make([]File, 0, len(s.Manifest.Entries)+1)
	for _, x := range s.Manifest.Entries {
		f := File{Name: x.File, SQL: append([]byte(nil), s.Files[x.File]...), Parents: append([]string(nil), x.Parents...), NonLinear: x.NonLinear}
		if x.ArtifactFile != "" {
			f.ArtifactName = x.ArtifactFile
			f.Artifact = append([]byte(nil), s.Files[x.ArtifactFile]...)
		}
		out = append(out, f)
	}
	return out
}
func cloneStrings(v map[string]string) map[string]string {
	out := map[string]string{}
	for k, x := range v {
		out[k] = x
	}
	return out
}
func renderGeneratedSQL(p plan.Plan, checks, bundle, hints string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "-- autosql:transaction=auto\n-- autosql:plan-digest=%s\n-- autosql:check-digest=%s\n-- autosql:bundle-digest=%s\n-- autosql:check-bundle-digest=%s\n-- autosql-rename-hints-sha256: %s\n", p.Digest, directiveDigest(checks), bundle, bundle, sha(hints))
	for _, s := range executableStatements(p) {
		b.WriteString(strings.TrimSpace(s))
		b.WriteString(";\n")
	}
	return []byte(b.String())
}
func directiveDigest(v string) string {
	if strings.HasPrefix(v, "sha256:") {
		return v
	}
	return "sha256:" + v
}
func sha(v string) string { x := sha256.Sum256([]byte(v)); return "sha256:" + hex.EncodeToString(x[:]) }

func replaySnapshot(ctx context.Context, snap Snapshot, r GenerateRequest) (doc schema.Document, err error) {
	u, e := url.Parse(r.DevelopmentURL)
	if e != nil || u.Scheme == "" {
		return doc, e
	}
	admin, e := pgx.Connect(ctx, r.DevelopmentURL)
	if e != nil {
		return doc, e
	}
	defer admin.Close(context.Background())
	actual, e := simulate.ResolvePostgresIdentity(ctx, r.DevelopmentURL)
	if e != nil || actual != r.DevelopmentIdentity || actual == r.ProductionIdentity {
		return doc, errors.New("development identity mismatch")
	}
	random := make([]byte, 12)
	if _, e = rand.Read(random); e != nil {
		return doc, e
	}
	name := "autosql_gen_replay_" + hex.EncodeToString(random)
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); e != nil {
		return doc, e
	}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		_, ce := admin.Exec(cleanup, "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
		if ce != nil {
			err = errors.Join(err, ce)
		}
	}()
	du := *u
	du.Path = "/" + name
	conn, e := pgx.Connect(ctx, du.String())
	if e != nil {
		return doc, e
	}
	defer conn.Close(context.Background())
	for _, m := range snap.Manifest.Entries {
		if _, e = conn.Exec(ctx, string(snap.Files[m.File])); e != nil {
			return doc, e
		}
	}
	var schemas []string
	for _, resource := range r.Desired.Graph.Resources {
		if resource.Kind == schema.KindSchema {
			schemas = append(schemas, resource.Name.Name)
		}
	}
	return postgres.InspectURL(ctx, du.String(), postgres.Options{Schemas: schemas})
}
