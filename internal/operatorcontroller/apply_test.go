package operatorcontroller

import (
	"context"
	"crypto/ed25519"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/bootstrap"
	"autosql/pkg/guardrail"
	"autosql/pkg/operator"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/precheck"
	"autosql/pkg/schema"
	"autosql/pkg/source"
	"github.com/jackc/pgx/v5"
)

// This test is opt-in because it requires a reachable PostgreSQL instance.
// It exercises the same source-to-plan check used immediately before the
// signed artifact mutation boundary.
func TestDeclarativePlanVerificationAgainstPostgres(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("AUTOSQL_OPERATOR_PG_URL"))
	if url == "" {
		t.Skip("AUTOSQL_OPERATOR_PG_URL is not set")
	}
	ctx := context.Background()
	const schemaName = "autosql_operator_plan_test"
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "drop schema if exists "+pgx.Identifier{schemaName}.Sanitize()+" cascade"); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, "drop schema if exists "+pgx.Identifier{schemaName}.Sanitize()+" cascade")

	desiredSQL := "create schema " + schemaName + "; create table " + schemaName + ".orders (id bigint);"
	desired, err := source.LoadContext(ctx, source.Input{URI: "operator:inline", Format: source.FormatSQL, Data: []byte(desiredSQL)})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{schemaName}})
	if err != nil {
		t.Fatal(err)
	}
	current, err = postgres.New().Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	resource := operatorResourceForPlanTest(desiredSQL, url)
	if err := verifyDeclarativePlan(ctx, resource.ResolvedSource, resource.ResolvedDatabaseURL, artifact.Artifact{Plan: p}, nil, false); err != nil {
		t.Fatal(err)
	}
	if err := verifyDeclarativePlan(ctx, "create schema "+schemaName+";", url, artifact.Artifact{Plan: p}, nil, false); err == nil {
		t.Fatal("mismatched declarative source was accepted")
	}
}

func TestArtifactApplyAdoptsWithoutCallingMutationService(t *testing.T) {
	digest := "sha256:" + strings.Repeat("a", 64)
	parsed := artifact.Artifact{Digest: digest, DatabaseIdentity: "orders", TargetEnvironment: "production", Plan: plan.Plan{Digest: "sha256:" + strings.Repeat("b", 64)}}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, digest+".json"), []byte("adoption artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_OPERATOR_ARTIFACT_DIR", directory)
	oldParse, oldVerify, oldAdopt := parseOperatorArtifact, verifyProductionOperatorArtifact, verifyOperatorAdoptionResource
	t.Cleanup(func() {
		parseOperatorArtifact, verifyProductionOperatorArtifact, verifyOperatorAdoptionResource = oldParse, oldVerify, oldAdopt
	})
	parseOperatorArtifact = func([]byte) (artifact.Artifact, error) { return parsed, nil }
	verifyProductionOperatorArtifact = func(databaseURL string, candidate artifact.Artifact) (artifact.Artifact, error) {
		if databaseURL != "postgres://runtime-target" || candidate.Digest != digest {
			t.Fatalf("verification binding lost: url=%q artifact=%+v", databaseURL, candidate)
		}
		return candidate, nil
	}
	verifyOperatorAdoptionResource = func(_ context.Context, resource operator.Resource, candidate artifact.Artifact) (verifiedAdoption, error) {
		if resource.Spec.AdoptionPolicy != operator.AdoptIfEquivalent || candidate.Digest != digest {
			t.Fatalf("adoption input=%+v artifact=%+v", resource.Spec, candidate)
		}
		return verifiedAdoption{SourceDigest: "sha256:" + strings.Repeat("c", 64)}, nil
	}
	resource := operator.Resource{Spec: operator.Spec{Kind: operator.Declarative, AdoptionPolicy: operator.AdoptIfEquivalent, Source: operator.Source{Format: "hcl", Inline: `schema "app" {}`}}, ResolvedSource: `schema "app" {}`, ResolvedDatabaseURL: "postgres://runtime-target"}
	outcome, err := ArtifactApply(context.Background(), resource, digest)
	if err != nil || outcome.Status != "adopted" || outcome.AppliedSteps != 0 || outcome.SourceDigest == "" || outcome.PlanDigest != parsed.Plan.Digest {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}

	verifyOperatorAdoptionResource = func(context.Context, operator.Resource, artifact.Artifact) (verifiedAdoption, error) {
		return verifiedAdoption{}, errors.New("live database does not match desired schema; adoption refused")
	}
	outcome, err = ArtifactApply(context.Background(), resource, digest)
	if err == nil || outcome.Status != "failed" || outcome.AppliedSteps != 0 {
		t.Fatalf("mismatch outcome=%+v err=%v", outcome, err)
	}
}

func TestRequireNoOpAdoptionPlan(t *testing.T) {
	fingerprint := "sha256:" + strings.Repeat("d", 64)
	valid := plan.Plan{FromFingerprint: fingerprint, ToFingerprint: fingerprint}
	if err := requireNoOpAdoptionPlan(valid); err != nil {
		t.Fatalf("valid no-op rejected: %v", err)
	}
	for name, mutate := range map[string]func(*plan.Plan){
		"different fingerprint": func(p *plan.Plan) { p.ToFingerprint = "sha256:" + strings.Repeat("e", 64) },
		"change": func(p *plan.Plan) {
			p.Changes.Changes = []schema.Change{{ID: "change"}}
		},
		"step":   func(p *plan.Plan) { p.Steps = []plan.Step{{ID: "step"}} },
		"phase":  func(p *plan.Plan) { p.Phases = []plan.Phase{{ID: "phase"}} },
		"replay": func(p *plan.Plan) { p.Replay = []string{"side-effect"} },
	} {
		t.Run(name, func(t *testing.T) {
			candidate := valid
			mutate(&candidate)
			if err := requireNoOpAdoptionPlan(candidate); err == nil {
				t.Fatal("non-no-op adoption plan accepted")
			}
		})
	}
}

func TestAdoptionEquivalenceAgainstPostgres(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("AUTOSQL_OPERATOR_PG_URL"))
	if url == "" {
		t.Skip("AUTOSQL_OPERATOR_PG_URL is not set")
	}
	ctx := context.Background()
	const schemaName = "autosql_operator_adopt_test"
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	quoted := pgx.Identifier{schemaName}.Sanitize()
	if _, err = conn.Exec(ctx, "drop schema if exists "+quoted+" cascade; create schema "+quoted+"; create table "+quoted+".orders (id bigint)"); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), "drop schema if exists "+quoted+" cascade")
	desiredSQL := "create schema " + schemaName + "; create table " + schemaName + ".orders (id bigint);"
	desired, err := source.LoadContext(ctx, source.Input{URI: "operator:inline", Format: source.FormatSQL, Data: []byte(desiredSQL)})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := plan.Build(ctx, postgres.New(), desired, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	resource := operator.Resource{Spec: operator.Spec{Kind: operator.Declarative, AdoptionPolicy: operator.AdoptIfEquivalent, Source: operator.Source{Format: "sql", Inline: desiredSQL}}, ResolvedSource: desiredSQL, ResolvedDatabaseURL: url}
	artifact := artifact.Artifact{Plan: approved, DatabaseIdentity: "adoption-test", TargetEnvironment: "test"}
	if _, err = verifyAdoptionResource(ctx, resource, artifact); err != nil {
		t.Fatalf("equivalent database rejected: %v", err)
	}
	if _, err = conn.Exec(ctx, "alter table "+quoted+".orders add column drifted text"); err != nil {
		t.Fatal(err)
	}
	if _, err = verifyAdoptionResource(ctx, resource, artifact); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("drifted database adoption error=%v", err)
	}
}

func TestAdoptionEquivalenceWithMaterializedViewAgainstPostgres(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("AUTOSQL_OPERATOR_PG_URL"))
	if url == "" {
		t.Skip("AUTOSQL_OPERATOR_PG_URL is not set")
	}
	ctx := context.Background()
	const schemaName = "autosql_operator_adopt_matview_test"
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	quoted := pgx.Identifier{schemaName}.Sanitize()
	if _, err = conn.Exec(ctx, "drop schema if exists "+quoted+" cascade; create schema "+quoted+"; create table "+quoted+".orders (id bigint); create materialized view "+quoted+".order_ids as select id from "+quoted+".orders"); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), "drop schema if exists "+quoted+" cascade")
	desiredSQL := "create schema " + schemaName + "; create table " + schemaName + ".orders (id bigint); create materialized view " + schemaName + ".order_ids as select id from " + schemaName + ".orders;"
	desired, err := source.LoadContext(ctx, source.Input{URI: "operator:inline", Format: source.FormatSQL, Data: []byte(desiredSQL)})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	approved, err := plan.Build(ctx, postgres.New(), desired, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	resource := operator.Resource{Spec: operator.Spec{Kind: operator.Declarative, AdoptionPolicy: operator.AdoptIfEquivalent, Source: operator.Source{Format: "sql", Inline: desiredSQL}}, ResolvedSource: desiredSQL, ResolvedDatabaseURL: url}
	artifact := artifact.Artifact{Plan: approved, DatabaseIdentity: "adoption-matview-test", TargetEnvironment: "test"}
	if _, err = verifyAdoptionResource(ctx, resource, artifact); err != nil {
		t.Fatalf("equivalent database with materialized view rejected: %v", err)
	}
}

func TestVerifyBootstrapAuthorizationUsesSignedPlanBoundToken(t *testing.T) {
	ctx := context.Background()
	namespace := schema.Resource{Kind: schema.KindSchema, Name: schema.Name{Name: "app"}, Spec: []byte(`{}`)}
	namespace.ID = schema.StableID(namespace.Kind, namespace.Name)
	extension := schema.Resource{Kind: schema.KindExtension, Name: schema.Name{Schema: "app", Name: "pgcrypto", Parent: namespace.ID}, Spec: []byte(`{"version":"1.3","relocatable":true,"trusted":true,"superuser":false,"requires":[]}`), Dependencies: []schema.Dependency{{Target: namespace.ID, Type: schema.DependencyContains}}}
	extension.ID = schema.StableID(extension.Kind, extension.Name)
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{namespace, extension}}}
	desired.Normalize()
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: "db.internal", Port: 5432, TLSMode: "verify-full"}, MaintenanceDatabase: "postgres", Name: "cell", Owner: "postgres", Encoding: "UTF8", Template: "template0", Tablespace: "pg_default", ConnectionLimit: -1, AllowConnections: true}
	inventory, err := postgres.PrepareBootstrapAuthorizationInventory(ctx, target, desired, postgres.BootstrapAuthorizationInventoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	manifest, err := postgres.NewBootstrapAuthorizationManifest(inventory, now, now, now.Add(time.Hour), "security", "dba", "bootstrap-authorization")
	if err == nil {
		err = manifest.Sign("key-1", private)
	}
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := manifest.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	verified, err := postgres.VerifyBootstrapAuthorizationManifest(manifest, inventory, postgres.BootstrapAuthorizationVerifyPolicy{Now: func() time.Time { return now.Add(time.Minute) }, Keys: map[string]artifact.KeyRecord{"key-1": {PublicKey: public, Issuer: "security", Identity: "dba", Purpose: "bootstrap-authorization", Status: "active", NotBefore: manifest.NotBefore, NotAfter: manifest.ExpiresAt}}, Issuer: "security", Signer: "dba", Purpose: "bootstrap-authorization"})
	if err != nil {
		t.Fatal(err)
	}
	whole, err := postgres.PlanDatabaseBootstrapAuthorized(ctx, target, desired, plan.Options{}, verified)
	if err != nil {
		t.Fatal(err)
	}
	ref := &operator.BootstrapAuthorizationRef{ManifestSecretRef: operator.SecretKeyRef{Name: "auth", Key: "manifest"}, PublicKeySecretRef: operator.SecretKeyRef{Name: "auth", Key: "public"}, Issuer: "security", Signer: "dba", Purpose: "bootstrap-authorization"}
	resource := operator.Resource{Spec: operator.Spec{DatabaseTarget: &target, BootstrapAuthorization: ref}, ResolvedAuthorizationManifest: manifestBytes, ResolvedAuthorizationPublicKey: public}
	if _, err := verifyBootstrapAuthorization(ctx, resource, desired, artifact.Artifact{Plan: whole.SchemaPlan}); err != nil {
		t.Fatalf("accepted manifest rejected: %v", err)
	}

	missing := resource
	missing.Spec.BootstrapAuthorization = nil
	if _, err := verifyBootstrapAuthorization(ctx, missing, desired, artifact.Artifact{Plan: whole.SchemaPlan}); !authorizationStateIs(err, operator.AuthorizationMissing) {
		t.Fatalf("missing state=%v", err)
	}
	invalid := resource
	invalid.ResolvedAuthorizationManifest = []byte("not a manifest")
	if _, err := verifyBootstrapAuthorization(ctx, invalid, desired, artifact.Artifact{Plan: whole.SchemaPlan}); !authorizationStateIs(err, operator.AuthorizationInvalid) {
		t.Fatalf("invalid state=%v", err)
	}
	staleDesired := desired
	extra := schema.Resource{Kind: schema.KindSchema, Name: schema.Name{Name: "later"}, Spec: []byte(`{}`)}
	extra.ID = schema.StableID(extra.Kind, extra.Name)
	staleDesired.Graph.Resources = append(append([]schema.Resource(nil), desired.Graph.Resources...), extra)
	staleDesired.Normalize()
	if _, err := verifyBootstrapAuthorization(ctx, resource, staleDesired, artifact.Artifact{Plan: whole.SchemaPlan}); !authorizationStateIs(err, operator.AuthorizationStale) {
		t.Fatalf("stale state=%v", err)
	}
}

func authorizationStateIs(err error, state operator.AuthorizationState) bool {
	var authorizationErr *operator.AuthorizationError
	return errors.As(err, &authorizationErr) && authorizationErr.State == state
}

func operatorResourceForPlanTest(sql, url string) operator.Resource {
	return operator.Resource{ResolvedSource: sql, ResolvedDatabaseURL: url}
}

func TestDeclarativeSourceFormatSelectionIsExplicitAndBackwardCompatible(t *testing.T) {
	ctx := context.Background()
	hcl := `schema "app" {}`
	if document, err := loadOperatorDeclarativeSource(ctx, hcl, ""); err != nil || len(document.Graph.Resources) == 0 {
		t.Fatalf("schema-only legacy HCL rejected: resources=%d err=%v", len(document.Graph.Resources), err)
	}
	if _, err := loadOperatorDeclarativeSource(ctx, hcl, "hcl"); err != nil {
		t.Fatalf("explicit HCL rejected: %v", err)
	}
	sql := "create schema app; create table app.notes (body text default 'database phrase'); -- database comment"
	if document, err := loadOperatorDeclarativeSource(ctx, sql, ""); err != nil || len(document.Graph.Resources) < 2 {
		t.Fatalf("SQL containing database phrase was misclassified: resources=%d err=%v", len(document.Graph.Resources), err)
	}
	if _, err := loadOperatorDeclarativeSource(ctx, sql, "sql"); err != nil {
		t.Fatalf("explicit SQL rejected: %v", err)
	}
	for name, test := range map[string]struct{ raw, format, want string }{
		"hcl declared sql": {hcl, "sql", "declared sql"},
		"sql declared hcl": {sql, "hcl", "declared hcl"},
		"unknown format":   {sql, "json", "must be sql or hcl"},
		"ambiguous empty":  {"", "", "ambiguous"},
		"invalid both":     {"not valid {{{", "", "invalid"},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := loadOperatorDeclarativeSource(ctx, test.raw, test.format); err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want=%q", err, test.want)
			}
		})
	}
}

func TestArtifactApplyExecutesExactVerifiedBootstrapPlanThroughMaintenanceURL(t *testing.T) {
	ctx := context.Background()
	namespace := schema.Resource{Kind: schema.KindSchema, Name: schema.Name{Name: "app"}, Spec: []byte(`{}`)}
	namespace.ID = schema.StableID(namespace.Kind, namespace.Name)
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{namespace}}}
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: "db.internal", Port: 5432, TLSMode: "verify-full"}, MaintenanceDatabase: "postgres", Name: "cell", Owner: "postgres", Encoding: "UTF8", Template: "template0", Tablespace: "pg_default", ConnectionLimit: -1, AllowConnections: true}
	whole, err := postgres.PlanDatabaseBootstrap(ctx, target, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	parsed := artifact.Artifact{Digest: digest, Plan: whole.SchemaPlan, DatabaseIdentity: target.Name}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, digest+".json"), []byte("production artifact bytes"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_OPERATOR_ARTIFACT_DIR", directory)
	originalParse, originalVerifySource, originalVerifyArtifact, originalExecute := parseOperatorArtifact, verifyOperatorDeclarativeResource, verifyProductionOperatorArtifact, executeOperatorDatabaseBootstrapURL
	defer func() {
		parseOperatorArtifact, verifyOperatorDeclarativeResource, verifyProductionOperatorArtifact, executeOperatorDatabaseBootstrapURL = originalParse, originalVerifySource, originalVerifyArtifact, originalExecute
	}()
	parseOperatorArtifact = func(raw []byte) (artifact.Artifact, error) {
		if string(raw) != "production artifact bytes" {
			t.Fatal("unexpected artifact bytes")
		}
		return parsed, nil
	}
	verifyOperatorDeclarativeResource = func(context.Context, operator.Resource, artifact.Artifact) (verifiedBootstrapPlan, error) {
		return verifiedBootstrapPlan{Plan: whole, SourceDigest: "sha256:" + strings.Repeat("b", 64), ExpiresAt: time.Now().Add(time.Hour), Authorized: true}, nil
	}
	verifyProductionOperatorArtifact = func(databaseURL string, candidate artifact.Artifact) (artifact.Artifact, error) {
		if databaseURL != "postgres://target-runtime" || candidate.Digest != digest {
			t.Fatal("production artifact verification binding missing")
		}
		return candidate, nil
	}
	executed := false
	executeOperatorDatabaseBootstrapURL = func(_ context.Context, maintenanceURL string, candidate bootstrap.Plan, _ postgres.BootstrapExecutionHooks) (postgres.BootstrapExecutionResult, error) {
		executed = maintenanceURL == "postgres://maintenance-runtime" && candidate.Digest == whole.Digest && candidate.Target == whole.Target
		return postgres.BootstrapExecutionResult{PlanDigest: candidate.Digest, CreatedDatabase: true, Completed: true, AppliedSteps: len(candidate.Steps)}, nil
	}
	resource := operator.Resource{Spec: operator.Spec{Kind: operator.Declarative, Source: operator.Source{Inline: "database HCL"}, DatabaseTarget: &target, BootstrapAuthorization: &operator.BootstrapAuthorizationRef{}}, ResolvedSource: "database HCL", ResolvedDatabaseURL: "postgres://target-runtime", ResolvedMaintenanceDatabaseURL: "postgres://maintenance-runtime"}
	outcome, err := ArtifactApply(ctx, resource, digest)
	if err != nil || !executed || outcome.AuthorizationState != operator.AuthorizationAccepted || outcome.PlanDigest != whole.Digest || outcome.AppliedSteps != len(whole.Steps) {
		t.Fatalf("outcome=%+v executed=%v err=%v", outcome, executed, err)
	}
	executed = false
	verifyProductionOperatorArtifact = func(string, artifact.Artifact) (artifact.Artifact, error) {
		return artifact.Artifact{}, errors.New("signature rejected")
	}
	if _, err := ArtifactApply(ctx, resource, digest); err == nil || executed {
		t.Fatalf("artifact verification failure reached bootstrap executor: executed=%v err=%v", executed, err)
	}
}

func TestArtifactApplyBootstrapRequiresGeneratedNoEditsArtifactBeforeCredentialReadOrMutation(t *testing.T) {
	ctx := context.Background()
	namespace := schema.Resource{Kind: schema.KindSchema, Name: schema.Name{Name: "app"}, Spec: []byte(`{}`)}
	namespace.ID = schema.StableID(namespace.Kind, namespace.Name)
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{namespace}}}
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: "db.internal", Port: 5432, TLSMode: "verify-full"}, MaintenanceDatabase: "postgres", Name: "cell", Owner: "postgres", Encoding: "UTF8", Template: "template0", Tablespace: "pg_default", ConnectionLimit: -1, AllowConnections: true}
	whole, err := postgres.PlanDatabaseBootstrap(ctx, target, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	changeDigest, err := guardrail.ChangeDigest(whole.SchemaPlan.Changes)
	if err != nil {
		t.Fatal(err)
	}
	statements := make([]string, 0, len(whole.SchemaPlan.Steps))
	for _, step := range whole.SchemaPlan.Steps {
		if step.Kind == plan.StepExecutable {
			statements = append(statements, step.SQL)
		}
	}
	checks := precheck.Plan{ID: "operator-no-edits-checks", ChangeDigest: changeDigest, Statements: statements}
	checks.Digest, err = precheck.Digest(checks)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	releasePublic, releasePrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	generatorPublic, generatorPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	guardrailDigest := "sha256:" + strings.Repeat("9", 64)
	newGenerated := func() artifact.Artifact {
		a, createErr := artifact.NewGenerated(whole.SchemaPlan, checks, now, now.Add(time.Hour), "source", "production", target.Name, guardrailDigest, artifact.Approval{Identity: "release", ApprovedAt: now}, map[string]string{}, "generator-key", "migration-generator", generatorPrivate)
		if createErr == nil {
			createErr = a.Sign("release-key", releasePrivate)
		}
		if createErr != nil {
			t.Fatal(createErr)
		}
		return a
	}
	generated := newGenerated()
	unattested, err := artifact.New(whole.SchemaPlan, checks, now, now.Add(time.Hour), "source", "production", target.Name, guardrailDigest, artifact.Approval{Identity: "release", ApprovedAt: now}, map[string]string{})
	if err == nil {
		err = unattested.Sign("release-key", releasePrivate)
	}
	if err != nil {
		t.Fatal(err)
	}
	edited := newGenerated()
	edited.MarkEditedOrigin("review-editor")
	if err := edited.ResetAuthorization(); err == nil {
		err = edited.Sign("release-key", releasePrivate)
	}
	if err != nil {
		t.Fatal(err)
	}
	missingOriginAttestation := newGenerated()
	missingOriginAttestation.Origin.Signature = ""
	if err := missingOriginAttestation.ResetAuthorization(); err == nil {
		err = missingOriginAttestation.Sign("release-key", releasePrivate)
	}
	if err != nil {
		t.Fatal(err)
	}

	directory := t.TempDir()
	config := map[string]any{
		// The override supplied by the operator is authoritative. This missing
		// reference is a tripwire proving verification does not read a second
		// configured database credential before applying the no-edits policy.
		"DatabaseURL": "env://AUTOSQL_OPERATOR_MUST_NOT_READ", "Environment": "production", "DatabaseIdentity": target.Name, "SourceRevision": "source",
		"KeyID": "release-key", "PublicKey": base64.RawStdEncoding.EncodeToString(releasePublic), "Issuer": "release-issuer", "Signer": "release-signer", "Author": "author", "Requester": "requester",
		"ApprovalAuditPath": filepath.Join(directory, "approval.jsonl"), "LifecycleAuditPath": filepath.Join(directory, "lifecycle.jsonl"), "ArtifactDirectory": directory,
		"PostgresVersion": 18, "Schemas": []string{"app"}, "ExpectedPlanDigest": whole.SchemaPlan.Digest, "ExpectedChecksDigest": checks.Digest, "ExpectedGuardrailDigest": guardrailDigest,
		"ExpectedApprovalIdentity": "release", "KeyStatus": "active", "KeyPurpose": "plan-artifact", "KeyNotBefore": now.Add(-time.Hour), "KeyNotAfter": now.Add(2 * time.Hour), "NoEdits": false,
		"GeneratorKeyID": "generator-key", "GeneratorPublicKey": base64.RawStdEncoding.EncodeToString(generatorPublic), "GeneratorPurpose": "migration-generator",
	}
	configBytes, err := json.Marshal(config)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "apply-config.json")
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_APPLY_CONFIG", configPath)
	t.Setenv("AUTOSQL_OPERATOR_ARTIFACT_DIR", directory)
	t.Setenv("AUTOSQL_OPERATOR_MUST_NOT_READ", "")
	originalVerifySource, originalExecute := verifyOperatorDeclarativeResource, executeOperatorDatabaseBootstrapURL
	defer func() {
		verifyOperatorDeclarativeResource, executeOperatorDatabaseBootstrapURL = originalVerifySource, originalExecute
	}()
	verifyOperatorDeclarativeResource = func(context.Context, operator.Resource, artifact.Artifact) (verifiedBootstrapPlan, error) {
		return verifiedBootstrapPlan{Plan: whole, SourceDigest: "sha256:" + strings.Repeat("b", 64), Authorized: true}, nil
	}
	executed := false
	executeOperatorDatabaseBootstrapURL = func(context.Context, string, bootstrap.Plan, postgres.BootstrapExecutionHooks) (postgres.BootstrapExecutionResult, error) {
		executed = true
		return postgres.BootstrapExecutionResult{Completed: true}, nil
	}
	resource := operator.Resource{Spec: operator.Spec{Kind: operator.Declarative, Source: operator.Source{Inline: "database HCL"}, DatabaseTarget: &target, BootstrapAuthorization: &operator.BootstrapAuthorizationRef{}}, ResolvedSource: "database HCL", ResolvedDatabaseURL: "postgres://operator-resolved-target", ResolvedMaintenanceDatabaseURL: "postgres://operator-resolved-maintenance"}

	writeArtifact := func(a artifact.Artifact) string {
		raw, marshalErr := a.MarshalCanonical()
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		if writeErr := os.WriteFile(filepath.Join(directory, a.Digest+".json"), raw, 0o600); writeErr != nil {
			t.Fatal(writeErr)
		}
		return a.Digest
	}
	for name, candidate := range map[string]artifact.Artifact{"unattested artifact.New": unattested, "edited origin": edited, "missing generator attestation": missingOriginAttestation} {
		t.Run(name, func(t *testing.T) {
			executed = false
			if _, applyErr := ArtifactApply(ctx, resource, writeArtifact(candidate)); applyErr == nil || executed {
				t.Fatalf("mandatory no-edits policy bypass: executed=%v err=%v", executed, applyErr)
			}
		})
	}
	t.Run("missing provenance", func(t *testing.T) {
		executed = false
		raw, marshalErr := generated.MarshalCanonical()
		if marshalErr != nil {
			t.Fatal(marshalErr)
		}
		var object map[string]any
		if err := json.Unmarshal(raw, &object); err != nil {
			t.Fatal(err)
		}
		delete(object, "origin")
		raw, err = json.Marshal(object)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(directory, generated.Digest+".json"), raw, 0o600); err != nil {
			t.Fatal(err)
		}
		if _, applyErr := ArtifactApply(ctx, resource, generated.Digest); applyErr == nil || executed {
			t.Fatalf("missing provenance reached mutation: executed=%v err=%v", executed, applyErr)
		}
	})
	t.Run("valid generated artifact despite config bypass attempt", func(t *testing.T) {
		executed = false
		digest := writeArtifact(generated)
		verified, verifyErr := verifyOperatorReleaseArtifact(digest)
		if verifyErr != nil {
			t.Fatal(verifyErr)
		}
		resource.VerifiedReleaseArtifact = verified
		if err := os.Remove(filepath.Join(directory, digest+".json")); err != nil {
			t.Fatal(err)
		}
		if _, applyErr := ArtifactApply(ctx, resource, digest); applyErr != nil || !executed {
			t.Fatalf("generated artifact: executed=%v err=%v", executed, applyErr)
		}
	})
}

func TestArtifactApplyRejectsCrossTargetArtifactBeforeRuntimeResolution(t *testing.T) {
	ctx := context.Background()
	namespace := schema.Resource{Kind: schema.KindSchema, Name: schema.Name{Name: "app"}, Spec: []byte(`{}`)}
	namespace.ID = schema.StableID(namespace.Kind, namespace.Name)
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{namespace}}}
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: "db.internal"}, MaintenanceDatabase: "postgres", Name: "cell-b", Owner: "postgres", ConnectionLimit: -1, AllowConnections: true}
	whole, err := postgres.PlanDatabaseBootstrap(ctx, target, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("c", 64)
	parsed := artifact.Artifact{Digest: digest, Plan: whole.SchemaPlan, DatabaseIdentity: "cell-a"}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, digest+".json"), []byte("cross-target artifact"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_OPERATOR_ARTIFACT_DIR", directory)
	originalParse, originalVerifySource, originalVerifyArtifact, originalExecute := parseOperatorArtifact, verifyOperatorDeclarativeResource, verifyProductionOperatorArtifact, executeOperatorDatabaseBootstrapURL
	defer func() {
		parseOperatorArtifact, verifyOperatorDeclarativeResource, verifyProductionOperatorArtifact, executeOperatorDatabaseBootstrapURL = originalParse, originalVerifySource, originalVerifyArtifact, originalExecute
	}()
	parseOperatorArtifact = func([]byte) (artifact.Artifact, error) { return parsed, nil }
	verifyOperatorDeclarativeResource = func(context.Context, operator.Resource, artifact.Artifact) (verifiedBootstrapPlan, error) {
		return verifiedBootstrapPlan{Plan: whole}, nil
	}
	verified, executed := false, false
	verifyProductionOperatorArtifact = func(string, artifact.Artifact) (artifact.Artifact, error) {
		verified = true
		return parsed, nil
	}
	executeOperatorDatabaseBootstrapURL = func(context.Context, string, bootstrap.Plan, postgres.BootstrapExecutionHooks) (postgres.BootstrapExecutionResult, error) {
		executed = true
		return postgres.BootstrapExecutionResult{}, nil
	}
	resource := operator.Resource{Spec: operator.Spec{Kind: operator.Declarative, Source: operator.Source{Inline: "database HCL"}, DatabaseTarget: &target}, ResolvedSource: "database HCL"}
	if _, err := ArtifactApply(ctx, resource, digest); err == nil || !strings.Contains(err.Error(), "database identity") {
		t.Fatalf("cross-target artifact error=%v", err)
	}
	if verified || executed {
		t.Fatalf("cross-target artifact reached runtime verification or mutation: verified=%v executed=%v", verified, executed)
	}
	for _, identity := range []string{"cell-b ", " cell-b", "CELL-B", "cell\u2010b"} {
		if artifactIdentityMatchesBootstrapTarget(identity, target) {
			t.Fatalf("non-exact identity %q aliased target after normalization", identity)
		}
	}
	if !artifactIdentityMatchesBootstrapTarget("cell-b", target) {
		t.Fatal("exact canonical identity rejected")
	}
}

func TestArtifactApplyCreatesFreshDatabaseAgainstPostgresWithRealSignatures(t *testing.T) {
	maintenanceURL := strings.TrimSpace(os.Getenv("AUTOSQL_TEST_POSTGRES_URL"))
	if maintenanceURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	config, err := pgx.ParseConfig(maintenanceURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	var owner string
	if err := conn.QueryRow(ctx, `select current_user`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	database := fmt.Sprintf("autosql_operator_artifact_apply_%d", time.Now().UnixNano())
	defer postgres.DropDatabaseURL(context.Background(), maintenanceURL, database, true)
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: config.Host, Port: uint16(config.Port), TLSMode: "disable"}, MaintenanceDatabase: config.Database, Name: database, Owner: owner, Encoding: "UTF8", Template: "template0", Tablespace: "pg_default", ConnectionLimit: -1, AllowConnections: true}
	namespace := schema.Resource{Kind: schema.KindSchema, Name: schema.Name{Name: "operator_live"}, Spec: []byte(`{}`)}
	namespace.ID = schema.StableID(namespace.Kind, namespace.Name)
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{namespace}}}
	hcl, err := source.FormatHCL(desired)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadContext(ctx, source.Input{URI: "operator:inline", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	inventory, err := postgres.PrepareBootstrapAuthorizationInventory(ctx, target, desired, postgres.BootstrapAuthorizationInventoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	manifestPublic, manifestPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Second)
	manifest, err := postgres.NewBootstrapAuthorizationManifest(inventory, now, now.Add(-time.Minute), now.Add(time.Hour), "security", "dba", "bootstrap-authorization")
	if err == nil {
		err = manifest.Sign("bootstrap-key", manifestPrivate)
	}
	if err != nil {
		t.Fatal(err)
	}
	manifestBytes, err := manifest.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	verifiedManifest, err := postgres.VerifyBootstrapAuthorizationManifest(manifest, inventory, postgres.BootstrapAuthorizationVerifyPolicy{
		Now:    func() time.Time { return now },
		Keys:   map[string]artifact.KeyRecord{"bootstrap-key": {PublicKey: manifestPublic, Issuer: "security", Identity: "dba", Purpose: "bootstrap-authorization", Status: "active", NotBefore: manifest.NotBefore, NotAfter: manifest.ExpiresAt}},
		Issuer: "security", Signer: "dba", Purpose: "bootstrap-authorization",
	})
	if err != nil {
		t.Fatal(err)
	}
	whole, err := postgres.PlanDatabaseBootstrapAuthorized(ctx, target, desired, plan.Options{}, verifiedManifest)
	if err != nil {
		t.Fatal(err)
	}
	changeDigest, err := guardrail.ChangeDigest(whole.SchemaPlan.Changes)
	if err != nil {
		t.Fatal(err)
	}
	statements := make([]string, 0, len(whole.SchemaPlan.Steps))
	for _, step := range whole.SchemaPlan.Steps {
		if step.Kind == plan.StepExecutable {
			statements = append(statements, step.SQL)
		}
	}
	checks := precheck.Plan{ID: "operator-real-signed-checks", ChangeDigest: changeDigest, Statements: statements}
	checks.Digest, err = precheck.Digest(checks)
	if err != nil {
		t.Fatal(err)
	}
	releasePublic, releasePrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	generatorPublic, generatorPrivate, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	guardrailDigest := "sha256:" + strings.Repeat("9", 64)
	parsed, err := artifact.NewGenerated(whole.SchemaPlan, checks, now, now.Add(time.Hour), "operator-live-source", "operator-live", database, guardrailDigest, artifact.Approval{Identity: "release", ApprovedAt: now}, map[string]string{}, "generator-key", "migration-generator", generatorPrivate)
	if err == nil {
		err = parsed.Sign("release-key", releasePrivate)
	}
	if err != nil {
		t.Fatal(err)
	}
	artifactBytes, err := parsed.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	directory := t.TempDir()
	if err := os.WriteFile(filepath.Join(directory, parsed.Digest+".json"), artifactBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_OPERATOR_ARTIFACT_DIR", directory)
	t.Setenv("AUTOSQL_OPERATOR_REAL_SIGNED_DB", maintenanceURL)
	targetConfig := *config
	targetConfig.Database = database
	applyConfig := map[string]any{
		"DatabaseURL": "env://AUTOSQL_OPERATOR_REAL_SIGNED_DB", "Environment": "operator-live", "DatabaseIdentity": database, "SourceRevision": "operator-live-source",
		"KeyID": "release-key", "PublicKey": base64.RawStdEncoding.EncodeToString(releasePublic), "Issuer": "release-issuer", "Signer": "release-signer", "Author": "author", "Requester": "requester",
		"ApprovalAuditPath": filepath.Join(directory, "approval.jsonl"), "LifecycleAuditPath": filepath.Join(directory, "lifecycle.jsonl"), "ArtifactDirectory": directory,
		"PostgresVersion": 18, "Schemas": []string{"operator_live"}, "ExpectedPlanDigest": whole.SchemaPlan.Digest, "ExpectedChecksDigest": checks.Digest, "ExpectedGuardrailDigest": guardrailDigest,
		"ExpectedApprovalIdentity": "release", "KeyStatus": "active", "KeyPurpose": "plan-artifact", "KeyNotBefore": now.Add(-time.Hour), "KeyNotAfter": now.Add(2 * time.Hour), "NoEdits": false,
		"GeneratorKeyID": "generator-key", "GeneratorPublicKey": base64.RawStdEncoding.EncodeToString(generatorPublic), "GeneratorPurpose": "migration-generator",
	}
	configBytes, err := json.Marshal(applyConfig)
	if err != nil {
		t.Fatal(err)
	}
	configPath := filepath.Join(directory, "apply-config.json")
	if err := os.WriteFile(configPath, configBytes, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_APPLY_CONFIG", configPath)
	authorization := &operator.BootstrapAuthorizationRef{ManifestSecretRef: operator.SecretKeyRef{Name: "auth", Key: "manifest"}, PublicKeySecretRef: operator.SecretKeyRef{Name: "auth", Key: "public"}, Issuer: "security", Signer: "dba", Purpose: "bootstrap-authorization"}
	resource := operator.Resource{Spec: operator.Spec{Kind: operator.Declarative, Source: operator.Source{Format: "hcl", Inline: string(hcl)}, DatabaseTarget: &target, BootstrapAuthorization: authorization}, ResolvedSource: string(hcl), ResolvedDatabaseURL: targetConfig.ConnString(), ResolvedMaintenanceDatabaseURL: maintenanceURL, ResolvedAuthorizationManifest: manifestBytes, ResolvedAuthorizationPublicKey: manifestPublic}
	outcome, err := ArtifactApply(ctx, resource, parsed.Digest)
	if err != nil || outcome.PlanDigest != whole.Digest || outcome.AppliedSteps == 0 {
		t.Fatalf("outcome=%+v err=%v", outcome, err)
	}
	config.Database = database
	targetConn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer targetConn.Close(ctx)
	var exists bool
	if err := targetConn.QueryRow(ctx, `select exists(select 1 from pg_namespace where nspname='operator_live')`).Scan(&exists); err != nil || !exists {
		t.Fatalf("fresh database was not bootstrapped: exists=%v err=%v", exists, err)
	}
}
