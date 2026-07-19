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
	"sort"
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
	Authority                                                        approval.IdentityAuthority
	ApprovalProvider                                                 ApprovalProvider
	ApprovalAudit                                                    approval.AuditTrail
	Approvals                                                        []approval.Approval
	PrecheckAssertions                                               []precheck.Assertion
	CreatedAt, ExpiresAt                                             time.Time
	GeneratorKeyID, GeneratorPurpose, SigningKeyID                   string
	GeneratorPrivateKey, SigningPrivateKey                           ed25519.PrivateKey
	Metadata                                                         map[string]string
	Stage                                                            func(string) error
}

// ApprovalProvider issues approval only after the exact guardrail bundle is
// known. It is the non-interactive boundary used by CI release automation and
// avoids requiring callers to predict a digest before planning has run.
type ApprovalProvider interface {
	Issue(context.Context, string, string, time.Time, time.Time) ([]approval.Approval, approval.IdentityAuthority, error)
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
	workspace, err := replaySnapshot(ctx, snap, r)
	if err != nil {
		return out, generationFailure("replay", ErrGenerateStage)
	}
	defer workspace.Close()
	current, err := postgres.New().Normalize(ctx, workspace.Document)
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
	hints, canonicalHints, err := parseRenameHints(r.RenameHints, current, desired)
	if err != nil {
		return out, generationFailure("rename_hints", ErrGenerateConfig)
	}
	if fromFP == toFP {
		return GenerateResult{Status: "no_op", ManifestDigest: snap.Manifest.Digest}, nil
	}
	metadata := cloneStrings(r.Metadata)
	metadata["autosql.migration.manifest"] = snap.Manifest.Digest
	metadata["autosql.migration.from"] = fromFP
	metadata["autosql.migration.to"] = toFP
	metadata["autosql.migration.rename_hints"] = canonicalHints
	built, err := s.buildGeneratedArtifact(ctx, r, current, desired, workspace.URL, hints, plan.Options{}, nil, metadata)
	if err != nil {
		return out, err
	}
	p, checks, bundle, artifactBytes := built.Plan, built.Checks, built.BundleDigest, built.Bytes
	sql := renderGeneratedSQL(p, checks.Digest, bundle, canonicalHints)
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

type generatedArtifact struct {
	Artifact                                        artifact.Artifact
	Plan                                            plan.Plan
	Checks                                          precheck.Plan
	BundleDigest                                    string
	Bytes                                           []byte
	SchemaPolicyResources, MigrationPolicyResources []policy.Resource
}

func (s GenerateService) buildGeneratedArtifact(ctx context.Context, r GenerateRequest, current, desired schema.Document, workspaceURL string, hints []schema.RenameHint, options plan.Options, prebuilt *plan.Plan, metadata map[string]string) (generatedArtifact, error) {
	var out generatedArtifact
	fromFP, err := schema.SemanticFingerprint(current)
	if err != nil {
		return out, generationFailure("source_fingerprint", ErrGenerateStage)
	}
	toFP, err := schema.SemanticFingerprint(desired)
	if err != nil {
		return out, generationFailure("desired_fingerprint", ErrGenerateConfig)
	}
	if err = s.checkpoint(r, "plan"); err != nil {
		return out, err
	}
	var p plan.Plan
	if prebuilt == nil {
		options.Diff = schema.DiffOptions{RenameHints: hints}
		p, err = plan.Build(ctx, postgres.New(), current, desired, options)
		if err != nil {
			return out, generationFailure("plan", ErrGenerateStage)
		}
	} else {
		p = *prebuilt
	}
	if p.FromFingerprint != fromFP || p.ToFingerprint != toFP {
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
	checks, err := generationChecks(p, statements, r.PrecheckAssertions)
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
	for _, diagnostic := range diagnostics {
		if diagnostic.Suppressed == nil && diagnostic.Severity == safety.SeverityError {
			return out, generationFailure("safety", ErrGenerateStage)
		}
	}
	if err = s.checkpoint(r, "policy"); err != nil {
		return out, err
	}
	schemaResources, migrationResources := schemaPolicyResources(desired), migrationPolicyResources(p)
	violations, err := g.Policy.Evaluate(ctx, r.Policy, schemaResources, migrationResources)
	if err != nil || len(violations) != 0 {
		return out, generationFailure("policy", ErrGenerateStage)
	}
	bindings, err := guardrail.BuildStatementBindings(p.Changes, si.Statements)
	if err != nil {
		return out, generationFailure("guardrail_bindings", ErrGenerateStage)
	}
	in := guardrail.Input{Changes: p.Changes, Safety: si, Policy: r.Policy, PolicyIdentity: r.PolicyIdentity, SchemaResources: schemaResources, MigrationResources: migrationResources, Precheck: checks, Approval: approval.Request{Plan: approval.Plan{Environment: r.Environment, Author: r.Author, ExpiresAt: r.ExpiresAt}, Approvals: append([]approval.Approval(nil), r.Approvals...), RequestedBy: r.Requester}, StatementBindings: bindings, Mutation: generationPlanMutation{url: workspaceURL, plan: p}}
	if err = s.checkpoint(r, "guardrail"); err != nil {
		return out, err
	}
	bundle, err := g.BundleDigest(in)
	if err != nil {
		return out, generationFailure("guardrail", ErrGenerateStage)
	}
	authority := r.Authority
	if r.ApprovalProvider != nil {
		in.Approval.Approvals, authority, err = r.ApprovalProvider.Issue(ctx, bundle, r.Environment, r.CreatedAt.UTC(), r.ExpiresAt.UTC())
		if err != nil {
			return out, generationFailure("approval_issue", ErrGenerateStage)
		}
	}
	for i := range in.Approval.Approvals {
		in.Approval.Approvals[i].PlanDigest = bundle
		in.Approval.Approvals[i].Environment = r.Environment
	}
	in.Approval.Plan.Digest = bundle
	g.Approval.Authority = authority
	g.Approval.Audit = r.ApprovalAudit
	if _, err = g.Apply(ctx, in); err != nil {
		return out, generationFailure("guardrail_approval_precheck", ErrGenerateStage)
	}
	approved, err := trustedArtifactApproval(ctx, authority, in.Approval.Approvals, bundle, r.Environment)
	if err != nil {
		return out, generationFailure("approval_evidence", ErrGenerateStage)
	}
	created := r.CreatedAt.UTC()
	if err = s.checkpoint(r, "artifact"); err != nil {
		return out, err
	}
	a, err := artifact.NewGenerated(p, checks, created, r.ExpiresAt.UTC(), r.SourceRevision, r.Environment, r.DatabaseIdentity, bundle, approved, metadata, r.GeneratorKeyID, r.GeneratorPurpose, r.GeneratorPrivateKey)
	if err != nil {
		return out, generationFailure("artifact", ErrGenerateStage)
	}
	attExpiry := r.ExpiresAt.UTC()
	simConfig := sha(strings.Join([]string{r.ProductionIdentity, r.DevelopmentIdentity, fromFP, toFP}, "\x00"))
	safetyConfig := shaJSON(si)
	policyConfig := shaJSON(struct {
		Policy         policy.Document
		Identity       string
		Checks, Bundle string
	}{r.Policy, r.PolicyIdentity, checks.Digest, bundle})
	atts := []artifact.ValidationAttestation{{Stage: "replay_simulation", Implementation: "autosql/pkg/migrate.GenerateService", Version: "1", ConfigDigest: simConfig, ResultDigest: toFP, At: created, ExpiresAt: attExpiry, Simulation: &artifact.SimulationAttestation{TargetIdentity: r.ProductionIdentity, DevelopmentIdentity: r.DevelopmentIdentity, FromFingerprint: fromFP, ToFingerprint: toFP, DatabaseVersion: fmt.Sprint(r.PostgresVersion), ConfigDigest: simConfig}}, {Stage: "safety", Implementation: "autosql/pkg/safety.Runner", Version: "1", ConfigDigest: safetyConfig, ResultDigest: shaJSON(diagnostics), At: created, ExpiresAt: attExpiry, Safety: &artifact.SafetyAttestation{Analyzers: []string{"compatibility", "postgresql-operational"}, Threshold: string(safety.SeverityError), SuppressionsDigest: shaJSON([]safety.Diagnostic{}), DiagnosticsDigest: shaJSON(diagnostics), ConfigDigest: safetyConfig}}, {Stage: "policy_precheck_guardrail", Implementation: "autosql/pkg/guardrail.Guardrail", Version: "1", ConfigDigest: policyConfig, ResultDigest: bundle, At: created, ExpiresAt: attExpiry, Policy: &artifact.PolicyAttestation{DocumentDigest: shaJSON(r.Policy), LimitsDigest: shaJSON(g.Policy.Limits), ResourcesDigest: shaJSON([]any{in.SchemaResources, in.MigrationResources}), ConfigDigest: policyConfig}, Precheck: &artifact.PrecheckGuardrailAttestation{ChecksDigest: checks.Digest, GuardrailDigest: bundle, ConfigDigest: policyConfig}}}
	if err = a.SetValidationAttestations(atts); err != nil {
		return out, generationFailure("attest", ErrGenerateStage)
	}
	if err = a.Sign(r.SigningKeyID, r.SigningPrivateKey); err != nil {
		return out, generationFailure("sign", ErrGenerateStage)
	}
	encoded, err := a.MarshalCanonical()
	if err != nil {
		return out, generationFailure("artifact_encode", ErrGenerateStage)
	}
	return generatedArtifact{Artifact: a, Plan: p, Checks: checks, BundleDigest: bundle, Bytes: encoded, SchemaPolicyResources: schemaResources, MigrationPolicyResources: migrationResources}, nil
}

func validateGenerateRequest(r GenerateRequest) error {
	if r.Directory == "" || r.Version == "" || r.Label == "" || r.Format != "sql" || r.DevelopmentURL == "" || r.DevelopmentIdentity == "" || r.ProductionIdentity == "" || r.DevelopmentIdentity == r.ProductionIdentity || r.Environment == "" || r.DatabaseIdentity == "" || r.SourceRevision == "" || r.Author == "" || r.Requester == "" || r.Author == r.Requester || r.PolicyIdentity == "" || r.PostgresVersion <= 0 || r.CreatedAt.IsZero() || !r.ExpiresAt.After(r.CreatedAt) || len(r.GeneratorPrivateKey) != ed25519.PrivateKeySize || len(r.SigningPrivateKey) != ed25519.PrivateKeySize || r.GeneratorKeyID == "" || r.GeneratorPurpose == "" || r.SigningKeyID == "" || r.ApprovalAudit == nil || (r.ApprovalProvider == nil && (r.Authority == nil || len(r.Approvals) == 0)) {
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
func generationChecks(p plan.Plan, statements []string, assertions []precheck.Assertion) (precheck.Plan, error) {
	cd, e := guardrail.ChangeDigest(p.Changes)
	if e != nil {
		return precheck.Plan{}, e
	}
	c := precheck.Plan{ID: "generate:" + p.Digest, ChangeDigest: cd, Statements: statements, Assertions: append([]precheck.Assertion(nil), assertions...)}
	for i := range c.Assertions {
		c.Assertions[i].ChangeDigest = cd
		c.Assertions[i].PlanDigest = ""
	}
	c.Digest, e = precheck.Digest(c)
	for i := range c.Assertions {
		c.Assertions[i].PlanDigest = c.Digest
	}
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
func parseRenameHints(raw string, current, desired schema.Document) ([]schema.RenameHint, string, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, "{}", nil
	}
	data := []byte(strings.TrimSpace(raw))
	if err := rejectDuplicates(data); err != nil {
		return nil, "", err
	}
	var values map[string]string
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if d.Decode(&values) != nil {
		return nil, "", ErrGenerateConfig
	}
	canonical, _ := json.Marshal(values)
	if string(canonical) != string(data) {
		return nil, "", ErrGenerateConfig
	}
	currentIDs := map[string]bool{}
	desiredNames := map[string]int{}
	for _, x := range current.Graph.Resources {
		currentIDs[x.ID] = true
	}
	for _, x := range desired.Graph.Resources {
		desiredNames[x.Name.String()]++
	}
	keys := make([]string, 0, len(values))
	for k := range values {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	out := make([]schema.RenameHint, 0, len(keys))
	seenTo := map[string]bool{}
	for _, from := range keys {
		to := values[from]
		if !currentIDs[from] || desiredNames[to] != 1 || seenTo[to] {
			return nil, "", ErrGenerateConfig
		}
		seenTo[to] = true
		out = append(out, schema.RenameHint{From: from, To: to})
	}
	return out, string(canonical), nil
}
func snapshotFiles(s Snapshot) []File {
	out := make([]File, 0, len(s.Manifest.Entries)+1)
	for _, x := range s.Manifest.Entries {
		f := File{Name: x.File, SQL: append([]byte(nil), s.Files[x.File]...), Parents: append([]string(nil), x.Parents...), NonLinear: x.NonLinear, Kind: x.Kind, CoveredFrom: x.CoveredFrom, CoveredTo: x.CoveredTo, SchemaFingerprint: x.SchemaFingerprint, DataPolicy: x.DataPolicy}
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
func sha(v string) string  { x := sha256.Sum256([]byte(v)); return "sha256:" + hex.EncodeToString(x[:]) }
func shaJSON(v any) string { b, _ := json.Marshal(v); return sha(string(b)) }

type replayWorkspace struct {
	Document            schema.Document
	URL, adminURL, name string
}

func (w replayWorkspace) Close() error {
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	c, e := pgx.Connect(ctx, w.adminURL)
	if e != nil {
		return e
	}
	defer c.Close(context.Background())
	_, e = c.Exec(ctx, "DROP DATABASE IF EXISTS "+pgx.Identifier{w.name}.Sanitize()+" WITH (FORCE)")
	return e
}
func replaySnapshot(ctx context.Context, snap Snapshot, r GenerateRequest) (out replayWorkspace, err error) {
	out, err = createReplayWorkspace(ctx, r)
	if err != nil {
		return out, err
	}
	created := true
	defer func() {
		if err != nil && created {
			_ = out.Close()
		}
	}()
	conn, e := pgx.Connect(ctx, out.URL)
	if e != nil {
		return out, e
	}
	defer conn.Close(context.Background())
	start := 0
	for i, m := range snap.Manifest.Entries {
		if m.Kind == "checkpoint" {
			start = i
		}
	}
	for _, m := range snap.Manifest.Entries[start:] {
		if _, e = conn.Exec(ctx, string(snap.Files[m.File])); e != nil {
			return out, e
		}
	}
	var schemas []string
	for _, resource := range r.Desired.Graph.Resources {
		if resource.Kind == schema.KindSchema {
			schemas = append(schemas, resource.Name.Name)
		}
	}
	doc, e := postgres.InspectURL(ctx, out.URL, postgres.Options{Schemas: schemas})
	if e != nil {
		return out, e
	}
	out.Document = doc
	created = false
	return out, nil
}

func createReplayWorkspace(ctx context.Context, r GenerateRequest) (out replayWorkspace, err error) {
	u, e := url.Parse(r.DevelopmentURL)
	if e != nil || u.Scheme == "" {
		return out, e
	}
	admin, e := pgx.Connect(ctx, r.DevelopmentURL)
	if e != nil {
		return out, e
	}
	defer admin.Close(context.Background())
	actual, e := simulate.ResolvePostgresIdentity(ctx, r.DevelopmentURL)
	if e != nil || actual != r.DevelopmentIdentity || actual == r.ProductionIdentity {
		return out, errors.New("development identity mismatch")
	}
	random := make([]byte, 12)
	if _, e = rand.Read(random); e != nil {
		return out, e
	}
	name := "autosql_gen_replay_" + hex.EncodeToString(random)
	if _, e = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); e != nil {
		return out, e
	}
	created := true
	defer func() {
		if err != nil && created {
			_ = (replayWorkspace{adminURL: r.DevelopmentURL, name: name}).Close()
		}
	}()
	du := *u
	du.Path = "/" + name
	created = false
	return replayWorkspace{URL: du.String(), adminURL: r.DevelopmentURL, name: name}, nil
}

type replayDB struct{ url string }
type replayTx struct {
	conn *pgx.Conn
	tx   pgx.Tx
}

func (d replayDB) Begin(ctx context.Context) (precheck.Tx, error) {
	c, e := pgx.Connect(ctx, d.url)
	if e != nil {
		return nil, e
	}
	tx, e := c.Begin(ctx)
	if e != nil {
		c.Close(context.Background())
		return nil, e
	}
	return &replayTx{conn: c, tx: tx}, nil
}
func (t *replayTx) AcquireLock(ctx context.Context) error {
	_, e := t.tx.Exec(ctx, "SELECT pg_advisory_xact_lock(746812934)")
	return e
}
func (t *replayTx) QueryCount(ctx context.Context, q string, args ...any) (int64, error) {
	var n int64
	e := t.tx.QueryRow(ctx, q, args...).Scan(&n)
	return n, e
}
func (t *replayTx) Exec(ctx context.Context, q string) error { _, e := t.tx.Exec(ctx, q); return e }
func (t *replayTx) Commit(ctx context.Context) error {
	e := t.tx.Commit(ctx)
	t.conn.Close(context.Background())
	return e
}
func (t *replayTx) Rollback(ctx context.Context) error {
	e := t.tx.Rollback(ctx)
	t.conn.Close(context.Background())
	return e
}
func trustedArtifactApproval(ctx context.Context, a approval.IdentityAuthority, items []approval.Approval, digest, env string) (artifact.Approval, error) {
	latest := time.Time{}
	ids := []string{}
	proofs := []string{}
	for _, item := range items {
		v, e := a.VerifyApproval(ctx, item)
		if e != nil || v.PlanDigest != digest || v.Environment != env {
			return artifact.Approval{}, approval.ErrDenied
		}
		ids = append(ids, v.Identity.ID)
		proofs = append(proofs, item.Proof)
		if v.ApprovedAt.After(latest) {
			latest = v.ApprovedAt
		}
	}
	sort.Strings(ids)
	sort.Strings(proofs)
	return artifact.Approval{Identity: strings.Join(ids, ","), ApprovedAt: latest.UTC(), ProofDigest: sha(strings.Join(proofs, "\x00"))}, nil
}
