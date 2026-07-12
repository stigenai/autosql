package migrate

import (
	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/plan"
	"autosql/pkg/plugin"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/simulate"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/url"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5"
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
	checks, err := generationChecks(p, executableStatements(p), r.PrecheckAssertions)
	if err != nil {
		return out, generationFailure("prechecks", ErrGenerateStage)
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
	approvals := append([]approval.Approval(nil), r.Approvals...)
	for i := range approvals {
		approvals[i].PlanDigest, approvals[i].Environment = p.Digest, r.Environment
	}
	approved, err := trustedArtifactApproval(ctx, r.Authority, approvals, p.Digest, r.Environment)
	if err != nil {
		return out, generationFailure("approval_evidence", ErrGenerateStage)
	}
	if err = s.checkpoint(r.GenerateRequest, "sign"); err != nil {
		return out, err
	}
	a, err := artifact.NewGenerated(p, checks, r.CreatedAt.UTC(), r.ExpiresAt.UTC(), r.SourceRevision, r.Environment, r.DatabaseIdentity, p.Digest, approved, metadata, r.GeneratorKeyID, r.GeneratorPurpose, r.GeneratorPrivateKey)
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
	sql := renderCheckpointSQL(statements, p.Digest, checks.Digest, r.DataPolicy, declared)
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
	rows, err := c.Query(ctx, `select n.nspname from pg_namespace n where n.nspname <> 'information_schema' and n.nspname !~ '^pg_' and exists(select 1 from pg_class c where c.relnamespace=n.oid and c.relkind in ('r','p','v','m','S','f')) order by n.nspname`)
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
		u := strings.ToUpper(string(s.Files[e.File]))
		data := strings.Contains(u, "INSERT ") || strings.Contains(u, "COPY ") || strings.Contains(u, "UPDATE ") || strings.Contains(u, "DELETE ")
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

func renderCheckpointSQL(ss []plugin.Statement, planDigest, checks, policy string, replay []string) []byte {
	var b strings.Builder
	fmt.Fprintf(&b, "-- autosql:transaction=required\n-- autosql:plan-digest=%s\n-- autosql:check-digest=%s\n-- autosql:bundle-digest=%s\n-- autosql:check-bundle-digest=%s\n-- autosql-checkpoint-data-policy: %s\n-- autosql-checkpoint-declared-replay: %s\n", planDigest, directiveDigest(checks), planDigest, planDigest, policy, strings.Join(replay, ","))
	for _, s := range ss {
		if s.Kind == plugin.StatementExecutable {
			b.WriteString(strings.TrimSpace(s.SQL))
			b.WriteString(";\n")
		}
	}
	return []byte(b.String())
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
