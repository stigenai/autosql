package migrate

import (
	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/guardrail"
	"autosql/pkg/plan"
	"autosql/pkg/plugin"
	"autosql/pkg/postgres"
	"autosql/pkg/safety"
	"autosql/pkg/schema"
	"autosql/pkg/simulate"
	"autosql/pkg/source"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

var ErrCheckpointPolicy = errors.New("checkpoint data omission policy denied")

type CheckpointRequest struct {
	GenerateRequest
	DataPolicy     string
	DeclaredReplay []string
	PolicyApproved bool
}

type CheckpointResult struct {
	Status, File, ArtifactFile, ManifestDigest, CoveredFrom, CoveredTo, SchemaFingerprint, DataPolicy string
	Statements                                                                                        int
}

type CheckpointVerification struct {
	Status            string `json:"status"`
	Checkpoints       int    `json:"checkpoints"`
	LatestVersion     string `json:"latest_version,omitempty"`
	SchemaFingerprint string `json:"schema_fingerprint,omitempty"`
}

// VerifyCheckpoints checks the immutable generation, artifact encoding and all
// checkpoint range/head/fingerprint/data-policy bindings without mutation.
func VerifyCheckpoints(directory string) (CheckpointVerification, error) {
	return VerifyCheckpointsTrusted(directory, nil)
}

func VerifyCheckpointsTrusted(directory string, verify func(artifact.Artifact) (artifact.VerifiedArtifact, error)) (CheckpointVerification, error) {
	s, err := LoadSnapshot(directory)
	if err != nil {
		return CheckpointVerification{}, err
	}
	out := CheckpointVerification{Status: "verified"}
	index := map[string]int{}
	for i, e := range s.Manifest.Entries {
		index[e.Version] = i
	}
	for i, e := range s.Manifest.Entries {
		if e.Kind != "checkpoint" {
			continue
		}
		from, fok := index[e.CoveredFrom]
		to, tok := index[e.CoveredTo]
		if !fok || !tok || from > to || to >= i {
			return CheckpointVerification{}, fmt.Errorf("%w: checkpoint covered range", ErrInvalid)
		}
		a, er := artifact.Parse(s.Files[e.ArtifactFile])
		if er != nil {
			return CheckpointVerification{}, fmt.Errorf("%w: checkpoint artifact", ErrInvalid)
		}
		if verify != nil {
			if _, er = verify(a); er != nil {
				return CheckpointVerification{}, fmt.Errorf("%w: checkpoint trust", ErrInvalid)
			}
		}
		md := a.Metadata
		if md["autosql.checkpoint.covered_from"] != e.CoveredFrom || md["autosql.checkpoint.covered_to"] != e.CoveredTo || md["autosql.checkpoint.head_chain"] != s.Manifest.Entries[to].ChainDigest || md["autosql.checkpoint.schema_fingerprint"] != e.SchemaFingerprint || md["autosql.checkpoint.data_policy"] != e.DataPolicy || a.Plan.ToFingerprint != e.SchemaFingerprint {
			return CheckpointVerification{}, fmt.Errorf("%w: checkpoint binding", ErrInvalid)
		}
		out.Checkpoints++
		out.LatestVersion = e.Version
		out.SchemaFingerprint = e.SchemaFingerprint
	}
	return out, nil
}

// CreateCheckpoint compacts schema history without claiming to preserve data.
// It publishes only after replay, canonical inspection, full rendering and a
// second empty-database simulation all agree on the exact schema fingerprint.
func (s GenerateService) CreateCheckpoint(ctx context.Context, r CheckpointRequest) (CheckpointResult, error) {
	var out CheckpointResult
	if err := validateGenerateRequest(r.GenerateRequest); err != nil {
		return out, err
	}
	if r.DataPolicy != "schema_only" && r.DataPolicy != "declared_replay" {
		return out, generationFailure("data_policy", ErrCheckpointPolicy)
	}
	if r.DataPolicy == "declared_replay" && (!r.PolicyApproved || len(r.DeclaredReplay) == 0) {
		return out, generationFailure("data_policy", ErrCheckpointPolicy)
	}
	if err := s.checkpoint(r.GenerateRequest, "snapshot"); err != nil {
		return out, err
	}
	snap, err := LoadSnapshot(r.Directory)
	if err != nil || len(snap.Manifest.Entries) == 0 {
		return out, generationFailure("snapshot", ErrGenerateConflict)
	}
	if err = linearHead(snap.Manifest); err != nil {
		return out, err
	}
	if err = validateCheckpointData(snap, r); err != nil {
		return out, err
	}
	if err = s.checkpoint(r.GenerateRequest, "replay"); err != nil {
		return out, err
	}
	workspace, err := replaySnapshot(ctx, snap, r.GenerateRequest)
	if err != nil {
		return out, generationFailure("replay", ErrGenerateStage)
	}
	defer workspace.Close()
	schemas, err := checkpointSchemas(ctx, workspace.URL)
	if err != nil || len(schemas) == 0 {
		return out, generationFailure("inspect", ErrGenerateStage)
	}
	inspected, err := postgres.InspectURL(ctx, workspace.URL, postgres.Options{Schemas: schemas})
	if err != nil {
		return out, generationFailure("inspect", ErrGenerateStage)
	}
	doc, err := postgres.New().Normalize(ctx, inspected)
	if err != nil {
		return out, generationFailure("inspect", ErrGenerateStage)
	}
	fingerprint, err := schema.SemanticFingerprint(doc)
	if err != nil {
		return out, generationFailure("inspect", ErrGenerateStage)
	}
	if err = s.checkpoint(r.GenerateRequest, "render"); err != nil {
		return out, err
	}
	statements, err := postgres.RenderDocument(ctx, doc, nil)
	if err != nil {
		return out, generationFailure("render", ErrGenerateStage)
	}
	if err = s.checkpoint(r.GenerateRequest, "simulate"); err != nil {
		return out, err
	}
	simulated, err := simulateCheckpoint(ctx, r.GenerateRequest, doc, statements)
	if err != nil || simulated != fingerprint {
		return out, generationFailure("simulate", ErrGenerateStage)
	}
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}, Extra: doc.Graph.Extra}, Annotations: doc.Annotations, Extra: doc.Extra}
	p, err := plan.Build(ctx, postgres.New(), empty, doc, plan.Options{})
	if err != nil || p.ToFingerprint != fingerprint {
		return out, generationFailure("plan", ErrGenerateStage)
	}
	replaySQL, err := declaredReplaySQL(snap, r.DeclaredReplay)
	if err != nil {
		return out, generationFailure("data_replay", ErrCheckpointPolicy)
	}
	p, err = plan.AppendReplay(p, replaySQL)
	if err != nil {
		return out, generationFailure("data_replay", ErrGenerateStage)
	}
	if r.DataPolicy == "declared_replay" {
		wantData, er := checkpointDataFingerprint(ctx, workspace.URL, schemas)
		if er != nil {
			return out, generationFailure("data_evidence", ErrGenerateStage)
		}
		gotSchema, gotData, er := simulateCheckpointPlan(ctx, r.GenerateRequest, doc, p.Statements(), schemas)
		if er != nil || gotSchema != fingerprint || gotData != wantData {
			return out, generationFailure("data_evidence", ErrGenerateStage)
		}
	}
	checks, err := generationChecks(p, executableStatements(p), r.PrecheckAssertions)
	if err != nil {
		return out, generationFailure("prechecks", ErrGenerateStage)
	}
	g := generationGuardrail(r.GenerateRequest)
	si := safety.Input{Changes: p.Changes, Statements: p.SafetyStatements(), Target: safety.Target{Engine: "postgresql", Version: r.PostgresVersion}}
	if err = s.checkpoint(r.GenerateRequest, "safety"); err != nil {
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
	if err = s.checkpoint(r.GenerateRequest, "policy"); err != nil {
		return out, err
	}
	violations, err := g.Policy.Evaluate(ctx, r.Policy, schemaPolicyResources(doc), migrationPolicyResources(p))
	if err != nil || len(violations) != 0 {
		return out, generationFailure("policy", ErrGenerateStage)
	}
	bindings, err := guardrail.BuildStatementBindings(p.Changes, si.Statements)
	if err != nil {
		return out, generationFailure("guardrail_bindings", ErrGenerateStage)
	}
	guardWorkspace, err := emptyCheckpointWorkspace(ctx, r.GenerateRequest)
	if err != nil {
		return out, generationFailure("guardrail_database", ErrGenerateStage)
	}
	defer guardWorkspace.Close()
	in := guardrail.Input{Changes: p.Changes, Safety: si, Policy: r.Policy, PolicyIdentity: r.PolicyIdentity, SchemaResources: schemaPolicyResources(doc), MigrationResources: migrationPolicyResources(p), Precheck: checks, Approval: approval.Request{Plan: approval.Plan{Environment: r.Environment, Author: r.Author, ExpiresAt: r.ExpiresAt}, Approvals: append([]approval.Approval(nil), r.Approvals...), RequestedBy: r.Requester}, StatementBindings: bindings, Database: replayDB{url: guardWorkspace.URL}}
	if err = s.checkpoint(r.GenerateRequest, "guardrail"); err != nil {
		return out, err
	}
	bundle, err := g.BundleDigest(in)
	if err != nil {
		return out, generationFailure("guardrail", ErrGenerateStage)
	}
	for i := range in.Approval.Approvals {
		in.Approval.Approvals[i].PlanDigest = bundle
		in.Approval.Approvals[i].Environment = r.Environment
	}
	in.Approval.Plan.Digest = bundle
	g.Approval.Authority = r.Authority
	g.Approval.Audit = r.ApprovalAudit
	if _, err = g.Apply(ctx, in); err != nil {
		return out, generationFailure("guardrail_approval_precheck", ErrGenerateStage)
	}
	approved, err := trustedArtifactApproval(ctx, r.Authority, in.Approval.Approvals, bundle, r.Environment)
	if err != nil {
		return out, generationFailure("approval_evidence", ErrGenerateStage)
	}
	coveredFrom, coveredTo := snap.Manifest.Entries[0].Version, snap.Manifest.Entries[len(snap.Manifest.Entries)-1].Version
	declared := append([]string(nil), r.DeclaredReplay...)
	sort.Strings(declared)
	metadata := cloneStrings(r.Metadata)
	metadata["autosql.checkpoint.covered_from"] = coveredFrom
	metadata["autosql.checkpoint.covered_to"] = coveredTo
	metadata["autosql.checkpoint.head_chain"] = snap.Manifest.Entries[len(snap.Manifest.Entries)-1].ChainDigest
	metadata["autosql.checkpoint.schema_fingerprint"] = fingerprint
	metadata["autosql.checkpoint.data_policy"] = r.DataPolicy
	metadata["autosql.checkpoint.declared_replay"] = strings.Join(declared, ",")
	metadata["autosql.migration.manifest"] = snap.Manifest.Digest
	metadata["autosql.migration.from"] = p.FromFingerprint
	metadata["autosql.migration.to"] = p.ToFingerprint
	if err = s.checkpoint(r.GenerateRequest, "sign"); err != nil {
		return out, err
	}
	a, err := artifact.NewGenerated(p, checks, r.CreatedAt.UTC(), r.ExpiresAt.UTC(), r.SourceRevision, r.Environment, r.DatabaseIdentity, bundle, approved, metadata, r.GeneratorKeyID, r.GeneratorPurpose, r.GeneratorPrivateKey)
	if err == nil {
		simConfig := sha(strings.Join([]string{r.ProductionIdentity, r.DevelopmentIdentity, p.FromFingerprint, p.ToFingerprint}, "\x00"))
		safetyConfig := shaJSON(si)
		policyConfig := shaJSON(struct {
			Policy                   any
			Identity, Checks, Bundle string
		}{r.Policy, r.PolicyIdentity, checks.Digest, bundle})
		atts := []artifact.ValidationAttestation{{Stage: "replay_simulation", Implementation: "autosql/pkg/migrate.GenerateService", Version: "1", ConfigDigest: simConfig, ResultDigest: fingerprint, At: r.CreatedAt.UTC(), ExpiresAt: r.ExpiresAt.UTC(), Simulation: &artifact.SimulationAttestation{TargetIdentity: r.ProductionIdentity, DevelopmentIdentity: r.DevelopmentIdentity, FromFingerprint: p.FromFingerprint, ToFingerprint: p.ToFingerprint, DatabaseVersion: fmt.Sprint(r.PostgresVersion), ConfigDigest: simConfig}}, {Stage: "safety", Implementation: "autosql/pkg/safety.Runner", Version: "1", ConfigDigest: safetyConfig, ResultDigest: shaJSON(diagnostics), At: r.CreatedAt.UTC(), ExpiresAt: r.ExpiresAt.UTC(), Safety: &artifact.SafetyAttestation{Analyzers: []string{"compatibility", "postgresql-operational"}, Threshold: string(safety.SeverityError), SuppressionsDigest: shaJSON([]safety.Diagnostic{}), DiagnosticsDigest: shaJSON(diagnostics), ConfigDigest: safetyConfig}}, {Stage: "policy_precheck_guardrail", Implementation: "autosql/pkg/guardrail.Guardrail", Version: "1", ConfigDigest: policyConfig, ResultDigest: bundle, At: r.CreatedAt.UTC(), ExpiresAt: r.ExpiresAt.UTC(), Policy: &artifact.PolicyAttestation{DocumentDigest: shaJSON(r.Policy), LimitsDigest: shaJSON(g.Policy.Limits), ResourcesDigest: shaJSON([]any{in.SchemaResources, in.MigrationResources}), ConfigDigest: policyConfig}, Precheck: &artifact.PrecheckGuardrailAttestation{ChecksDigest: checks.Digest, GuardrailDigest: bundle, ConfigDigest: policyConfig}}}
		err = a.SetValidationAttestations(atts)
	}
	if err == nil {
		err = a.Sign(r.SigningKeyID, r.SigningPrivateKey)
	}
	if err != nil {
		return out, generationFailure("sign", ErrGenerateStage)
	}
	raw, err := a.MarshalCanonical()
	if err != nil {
		return out, generationFailure("encode", ErrGenerateStage)
	}
	sql := renderCheckpointSQL(p, checks.Digest, bundle, r.DataPolicy, declared)
	name := fmt.Sprintf("V%s__%s.sql", r.Version, r.Label)
	files := snapshotFiles(snap)
	files = append(files, File{Name: name, SQL: sql, ArtifactName: name + ".artifact.json", Artifact: raw, Kind: "checkpoint", CoveredFrom: coveredFrom, CoveredTo: coveredTo, SchemaFingerprint: fingerprint, DataPolicy: r.DataPolicy})
	if err = s.checkpoint(r.GenerateRequest, "publish"); err != nil {
		return out, err
	}
	man, err := UpdateWithOps(r.Directory, UpdateRequest{Files: files, ManifestVersion: ManifestVersion, ExpectedManifestDigest: snap.Manifest.Digest}, s.Ops)
	if err != nil {
		return out, generationFailure("publish", ErrGenerateConflict)
	}
	return CheckpointResult{Status: "created", File: name, ArtifactFile: name + ".artifact.json", ManifestDigest: man.Digest, CoveredFrom: coveredFrom, CoveredTo: coveredTo, SchemaFingerprint: fingerprint, DataPolicy: r.DataPolicy, Statements: len(statements)}, nil
}

func checkpointSchemas(ctx context.Context, databaseURL string) ([]string, error) {
	c, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return nil, err
	}
	defer c.Close(context.Background())
	rows, err := c.Query(ctx, `select n.nspname from pg_namespace n where n.nspname <> 'information_schema' and n.nspname !~ '^pg_' and (n.nspname <> 'public' or exists(select 1 from pg_class c where c.relnamespace=n.oid and c.relkind in ('r','p','v','m','S','f'))) order by n.nspname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var n string
		if err = rows.Scan(&n); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func validateCheckpointData(s Snapshot, r CheckpointRequest) error {
	declared := map[string]bool{}
	for _, v := range r.DeclaredReplay {
		if declared[v] || r.DataPolicy != "declared_replay" {
			return generationFailure("data_policy", ErrCheckpointPolicy)
		}
		declared[v] = true
	}
	known := map[string]bool{}
	for _, e := range s.Manifest.Entries {
		known[e.Version] = true
		data, classifyErr := checkpointSideEffects(e.File, s.Files[e.File])
		if classifyErr != nil {
			data = true
		}
		if data && (r.DataPolicy != "declared_replay" || !r.PolicyApproved || !declared[e.Version]) {
			return generationFailure("data_policy", ErrCheckpointPolicy)
		}
	}
	for v := range declared {
		if !known[v] {
			return generationFailure("data_policy", ErrCheckpointPolicy)
		}
	}
	return nil
}

func checkpointSideEffects(name string, raw []byte) (bool, error) {
	parts, err := source.SplitSQL(name, string(raw))
	if err != nil {
		return true, err
	}
	for _, part := range parts {
		encoded, err := pg_query.ParseToJSON(part.SQL)
		if err != nil {
			return true, err
		}
		var tree any
		if json.Unmarshal([]byte(encoded), &tree) != nil {
			return true, ErrInvalid
		}
		if astSideEffect(tree) {
			return true, nil
		}
	}
	return false, nil
}

func astSideEffect(v any) bool {
	switch x := v.(type) {
	case []any:
		for _, item := range x {
			if astSideEffect(item) {
				return true
			}
		}
	case map[string]any:
		for k, item := range x {
			switch k {
			case "InsertStmt", "UpdateStmt", "DeleteStmt", "MergeStmt", "CopyStmt", "TruncateStmt", "DoStmt", "CallStmt", "CreateFunctionStmt", "AlterFunctionStmt", "VariableSetStmt", "FuncCall":
				return true
			}
			if astSideEffect(item) {
				return true
			}
		}
	}
	return false
}

func renderCheckpointSQL(p plan.Plan, checks, bundle, policy string, replay []string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "-- autosql:transaction=required\n-- autosql:plan-digest=%s\n-- autosql:check-digest=%s\n-- autosql:bundle-digest=%s\n-- autosql:check-bundle-digest=%s\n-- autosql-checkpoint-data-policy: %s\n-- autosql-checkpoint-declared-replay: %s\n", p.Digest, directiveDigest(checks), bundle, bundle, policy, strings.Join(replay, ","))
	for _, q := range executableStatements(p) {
		b.WriteString(strings.TrimSpace(q))
		b.WriteString(";\n")
	}
	return []byte(b.String())
}

func declaredReplaySQL(s Snapshot, versions []string) ([]string, error) {
	wanted := map[string]bool{}
	for _, v := range versions {
		wanted[v] = true
	}
	var out []string
	for _, e := range s.Manifest.Entries {
		if !wanted[e.Version] {
			continue
		}
		parts, err := source.SplitSQL(e.File, string(s.Files[e.File]))
		if err != nil {
			return nil, err
		}
		for _, part := range parts {
			side, err := checkpointSideEffects(e.File, []byte(part.SQL))
			if err != nil || !side {
				continue
			}
			out = append(out, strings.TrimSuffix(strings.TrimSpace(part.SQL), ";"))
		}
	}
	return out, nil
}

func simulateCheckpoint(ctx context.Context, r GenerateRequest, want schema.Document, statements []plugin.Statement) (fp string, err error) {
	actual, err := simulate.ResolvePostgresIdentity(ctx, r.DevelopmentURL)
	if err != nil || actual != r.DevelopmentIdentity || actual == r.ProductionIdentity {
		return "", errors.New("development identity mismatch")
	}
	u, err := url.Parse(r.DevelopmentURL)
	if err != nil {
		return "", err
	}
	admin, err := pgx.Connect(ctx, r.DevelopmentURL)
	if err != nil {
		return "", err
	}
	defer admin.Close(context.Background())
	random := make([]byte, 12)
	if _, err = rand.Read(random); err != nil {
		return "", err
	}
	name := "autosql_checkpoint_sim_" + hex.EncodeToString(random)
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return "", err
	}
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)")
	}()
	du := *u
	du.Path = "/" + name
	conn, err := pgx.Connect(ctx, du.String())
	if err != nil {
		return "", err
	}
	for _, s := range statements {
		if s.Kind == plugin.StatementExecutable {
			if _, err = conn.Exec(ctx, s.SQL); err != nil {
				conn.Close(context.Background())
				return "", err
			}
		}
	}
	conn.Close(context.Background())
	var schemas []string
	for _, x := range want.Graph.Resources {
		if x.Kind == schema.KindSchema {
			schemas = append(schemas, x.Name.Name)
		}
	}
	sort.Strings(schemas)
	doc, err := postgres.InspectURL(ctx, du.String(), postgres.Options{Schemas: schemas})
	if err != nil {
		return "", err
	}
	doc, err = postgres.New().Normalize(ctx, doc)
	if err != nil {
		return "", err
	}
	return schema.SemanticFingerprint(doc)
}

func emptyCheckpointWorkspace(ctx context.Context, r GenerateRequest) (replayWorkspace, error) {
	var out replayWorkspace
	actual, err := simulate.ResolvePostgresIdentity(ctx, r.DevelopmentURL)
	if err != nil || actual != r.DevelopmentIdentity || actual == r.ProductionIdentity {
		return out, errors.New("development identity mismatch")
	}
	u, err := url.Parse(r.DevelopmentURL)
	if err != nil {
		return out, err
	}
	admin, err := pgx.Connect(ctx, r.DevelopmentURL)
	if err != nil {
		return out, err
	}
	defer admin.Close(context.Background())
	random := make([]byte, 12)
	if _, err = rand.Read(random); err != nil {
		return out, err
	}
	name := "autosql_checkpoint_guard_" + hex.EncodeToString(random)
	if _, err = admin.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()); err != nil {
		return out, err
	}
	du := *u
	du.Path = "/" + name
	return replayWorkspace{URL: du.String(), adminURL: r.DevelopmentURL, name: name}, nil
}

func simulateCheckpointPlan(ctx context.Context, r GenerateRequest, want schema.Document, statements []plugin.Statement, schemas []string) (string, string, error) {
	w, err := emptyCheckpointWorkspace(ctx, r)
	if err != nil {
		return "", "", err
	}
	defer w.Close()
	c, err := pgx.Connect(ctx, w.URL)
	if err != nil {
		return "", "", err
	}
	for _, s := range statements {
		if s.Kind == plugin.StatementExecutable {
			if _, err = c.Exec(ctx, s.SQL); err != nil {
				c.Close(context.Background())
				return "", "", err
			}
		}
	}
	c.Close(context.Background())
	doc, err := postgres.InspectURL(ctx, w.URL, postgres.Options{Schemas: schemas})
	if err != nil {
		return "", "", err
	}
	doc, err = postgres.New().Normalize(ctx, doc)
	if err != nil {
		return "", "", err
	}
	fp, err := schema.SemanticFingerprint(doc)
	if err != nil {
		return "", "", err
	}
	data, err := checkpointDataFingerprint(ctx, w.URL, schemas)
	return fp, data, err
}

func checkpointDataFingerprint(ctx context.Context, databaseURL string, schemas []string) (string, error) {
	c, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return "", err
	}
	defer c.Close(context.Background())
	var material strings.Builder
	for _, ns := range schemas {
		rows, er := c.Query(ctx, `select tablename from pg_tables where schemaname=$1 order by tablename`, ns)
		if er != nil {
			return "", er
		}
		var tables []string
		for rows.Next() {
			var n string
			if er = rows.Scan(&n); er != nil {
				rows.Close()
				return "", er
			}
			tables = append(tables, n)
		}
		rows.Close()
		for _, table := range tables {
			q := `select coalesce(jsonb_agg(to_jsonb(t) order by to_jsonb(t)::text),'[]'::jsonb)::text from ` + pgx.Identifier{ns, table}.Sanitize() + ` t`
			var value string
			if er = c.QueryRow(ctx, q).Scan(&value); er != nil {
				return "", er
			}
			material.WriteString(ns)
			material.WriteByte(0)
			material.WriteString(table)
			material.WriteByte(0)
			material.WriteString(value)
			material.WriteByte(0)
		}
	}
	return sha(material.String()), nil
}
