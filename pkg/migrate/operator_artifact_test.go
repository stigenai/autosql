package migrate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/bootstrap"
	"autosql/pkg/plan"
	"autosql/pkg/policy"
	"autosql/pkg/postgres"
	"autosql/pkg/precheck"
	"autosql/pkg/safety"
	"autosql/pkg/schema"
	"autosql/pkg/simulate"
	"autosql/pkg/source"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestAutomationApprovalProviderBindsFreshProofToBundleAndEnvironment(t *testing.T) {
	_, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	created := time.Now().UTC().Truncate(time.Second)
	provider := AutomationApprovalProvider{KeyID: "ci-approval-2026-07", Identity: approval.Identity{ID: "github-actions", Roles: []string{"release-automation"}}, Actors: []approval.Identity{{ID: "schema-author"}, {ID: "release-requester"}}, PrivateKey: private, TTL: 10 * time.Minute}
	items, authority, err := provider.Issue(context.Background(), "sha256:"+strings.Repeat("a", 64), "production", created, created.Add(time.Hour))
	if err != nil || len(items) != 1 {
		t.Fatalf("issue items=%d err=%v", len(items), err)
	}
	verified, err := authority.VerifyApproval(context.Background(), items[0])
	if err != nil {
		t.Fatal(err)
	}
	if verified.Identity.ID != "github-actions" || verified.PlanDigest != "sha256:"+strings.Repeat("a", 64) || verified.Environment != "production" || !verified.ApprovedAt.Equal(created) || !verified.ExpiresAt.Equal(created.Add(10*time.Minute)) {
		t.Fatalf("unexpected claims: %+v", verified)
	}
	if actor, err := authority.ResolveActor(context.Background(), "schema-author"); err != nil || actor.ID != "schema-author" {
		t.Fatalf("author was not trusted: %+v %v", actor, err)
	}
	for name, mutate := range map[string]func(approval.Approval) approval.Approval{
		"signature": func(item approval.Approval) approval.Approval { item.Proof += "x"; return item },
		"proof":     func(item approval.Approval) approval.Approval { item.Proof = "invalid"; return item },
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := authority.VerifyApproval(context.Background(), mutate(items[0])); err == nil {
				t.Fatal("tampered automation approval verified")
			}
		})
	}
}

func TestGenerateErrorPreservesSafeSQLStateCause(t *testing.T) {
	cause := simulate.RedactedCause(&pgconn.PgError{Code: "42P06", Message: "schema public already exists", Detail: "seeded secret"})
	err := generationFailureCause("guardrail_approval_precheck", ErrGenerateStage, cause)
	var postgresError *simulate.PostgresError
	if !errors.Is(err, ErrGenerateStage) || !errors.As(err, &postgresError) || postgresError.SQLState() != "42P06" || strings.Contains(err.Error(), "seeded secret") {
		t.Fatalf("generation error=%v", err)
	}
}

func TestGenerationPlanMutationPreservesPostgresCause(t *testing.T) {
	developmentURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if developmentURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	identity, err := simulate.ResolvePostgresIdentity(ctx, developmentURL)
	if err != nil {
		t.Fatal(err)
	}
	workspace, err := (simulate.PostgresFactory{NamePrefix: "autosql_sim_error_capture"}).CreateWorkspace(ctx, simulate.Config{DevelopmentURL: developmentURL, DevelopmentIdentity: identity, ProductionIdentity: "operator-bootstrap/error-capture", CleanupTimeout: 15 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	defer workspace.Cleanup(context.Background())
	missingRole := "autosql_missing_owner_" + fmt.Sprintf("%x", sha256.Sum256([]byte(t.Name()+time.Now().String())))[:12]
	mutation := &generationPlanMutation{url: workspace.URL(), plan: plan.Plan{
		Steps:  []plan.Step{{ID: "owner-error", Kind: plan.StepExecutable, SQL: "CREATE SCHEMA owner_error AUTHORIZATION " + pgx.Identifier{missingRole}.Sanitize()}},
		Phases: []plan.Phase{{ID: "transactional", Transaction: plan.TransactionRequired, StepIDs: []string{"owner-error"}}},
	}}
	_, err = mutation.ApplyAuthorized(ctx, precheck.Plan{ID: "owner-error", ChangeDigest: "sha256:owner-error"})
	var postgresError *simulate.PostgresError
	if err == nil || !errors.As(mutation.cause, &postgresError) || postgresError.SQLState() != "42704" || !strings.Contains(mutation.cause.Error(), missingRole) {
		t.Fatalf("mutation err=%v cause=%v", err, mutation.cause)
	}
}

func TestOperatorSimulationFactoryMatchesBootstrapTargetPrecondition(t *testing.T) {
	external := bootstrap.DatabaseTarget{Mode: bootstrap.ExternalDatabase, Owner: "app"}
	desired := ownerRoleFixture()
	if factory := operatorSimulationFactory(&external, desired); !factory.DropPublicSchema || strings.Join(factory.RequiredRoles, ",") != "app,routine_owner" {
		t.Fatal("external bootstrap simulation retained the default public schema")
	}
	managed := bootstrap.DatabaseTarget{Mode: bootstrap.ManagedDatabase}
	if factory := operatorSimulationFactory(&managed, desired); factory.DropPublicSchema {
		t.Fatal("managed bootstrap simulation removed the intrinsic public schema")
	}
	if factory := operatorSimulationFactory(nil, desired); factory.DropPublicSchema {
		t.Fatal("ordinary transition simulation removed the default public schema")
	}
}

func TestExternalBootstrapArtifactReplaysPublicSchemaAndTemporaryOwnerWithoutProductionURL(t *testing.T) {
	developmentURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if developmentURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	suffix := fmt.Sprintf("%x", sha256.Sum256([]byte(t.Name()+time.Now().String())))
	role := "autosql_bootstrap_owner_" + suffix[:12]
	database := "autosql_bootstrap_external"
	routine := `CREATE OR REPLACE FUNCTION public.identity(value integer)
 RETURNS integer
 LANGUAGE sql
 IMMUTABLE
 SET search_path TO 'pg_catalog', 'public'
AS $function$ SELECT value $function$`
	routineDigest := sha256.Sum256([]byte(routine))
	hcl := fmt.Sprintf(`database %q {
  mode = "external"
  endpoint = { host = "production.invalid", port = 5432, tls_mode = "require" }
  maintenance_database = "postgres"
  owner = %q
  encoding = "UTF8"
  locale_provider = "libc"
  collation = "C"
  character_type = "C"
  template = "template0"
  tablespace = "pg_default"
  connection_limit = -1
  allow_connections = true
}
schema "public" {}
resource "function" "identity(value integer)" {
  schema = "public"
  parent = schema_id("public")
  spec_json = jsonencode({
    name = "identity"
    identity_arguments = "value integer"
    arguments = "value integer"
    result = "integer"
    returns_set = false
    language = "sql"
    volatility = "i"
    strict = false
    security_definer = false
    leakproof = false
    parallel = "u"
    cost = 100
    rows = 0
    configuration = ["search_path=pg_catalog, public"]
    owner = %q
    definition = %q
    body_digest = "sha256:%x"
  })
  deps_json = jsonencode([contains(schema_id("public"))])
}
`, database, role, role, routine, routineDigest)
	desired, err := source.LoadContext(ctx, source.Input{URI: "bootstrap-owner.hcl", Format: source.FormatHCLSource, Data: []byte(hcl)})
	if err != nil {
		t.Fatal(err)
	}
	target, err := singleDatabaseTarget(desired)
	if err != nil {
		t.Fatal(err)
	}
	request := generationFixture(t, t.TempDir(), developmentURL, developmentURL)
	request.Desired = desired
	request.DatabaseIdentity = database
	request.ProductionIdentity = "operator-bootstrap/" + database
	admin, err := pgx.Connect(ctx, developmentURL)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close(context.Background())
	defer func() {
		_, _ = admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize())
	}()
	_, _ = admin.Exec(ctx, "DROP ROLE IF EXISTS "+pgx.Identifier{role}.Sanitize())
	result, err := (GenerateService{}).BuildOperatorArtifact(ctx, OperatorArtifactRequest{Generation: request, Desired: desired, BootstrapTarget: &target, Render: map[string]string{"postgres_version": "16"}})
	if err != nil {
		t.Fatal(err)
	}
	if result.Artifact.Digest == "" || result.Artifact.Plan.Digest == "" {
		t.Fatalf("incomplete artifact: %+v", result.Artifact)
	}
	var roleExists bool
	if err = admin.QueryRow(ctx, `select exists(select 1 from pg_roles where rolname=$1)`, role).Scan(&roleExists); err != nil || roleExists {
		t.Fatalf("temporary owner role cleanup exists=%v err=%v", roleExists, err)
	}
}

func ownerRoleFixture() schema.Document {
	return schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{
		{ID: "role/managed_owner", Kind: schema.KindRole, Name: schema.Name{Name: "managed_owner"}, Spec: []byte(`{}`)},
		{ID: "function/app/f()", Kind: schema.KindFunction, Name: schema.Name{Schema: "app", Name: "f()"}, Spec: []byte(`{"owner":"routine_owner"}`)},
		{ID: "table/app/t", Kind: schema.KindTable, Name: schema.Name{Schema: "app", Name: "t"}, Spec: []byte(`{"owner":"managed_owner"}`)},
	}}}
}

func TestOperatorBootstrapArtifactRenderBindsCompleteInventory(t *testing.T) {
	inventory := postgres.BootstrapAuthorizationInventory{
		Routines: []postgres.BootstrapRoutineAuthorization{
			{SourceDigest: "sha256:b", UnsafeLanguageAuthorizationRequired: true},
			{SourceDigest: "sha256:a", PrivilegedRoutineAuthorizationRequired: true, TransactionControlAuthorizationRequired: true},
		},
		Extensions: []postgres.BootstrapExtensionAuthorization{
			{Name: "pgcrypto", Version: "1.3", Schema: "app"},
			{Name: "hstore", Version: "1.8", Schema: "app", UntrustedExtensionAuthorizationRequired: true},
		},
	}
	render := operatorBootstrapArtifactRender(inventory, map[string]string{"postgres_version": "16", "concurrent_indexes": "true"})
	want := map[string]string{
		"postgres_version": "16", "concurrent_indexes": "true",
		"reviewed_routine_digests": "sha256:a,sha256:b", "extension_allowlist": "hstore,pgcrypto",
		"extension_version.hstore": "1.8", "extension_schemas.hstore": "app",
		"extension_version.pgcrypto": "1.3", "extension_schemas.pgcrypto": "app",
		"allow_unsafe_routine_languages": "true", "allow_privileged_routines": "true",
		"allow_transaction_control_procedures": "true", "allow_untrusted_extensions": "true",
	}
	if len(render) != len(want) {
		t.Fatalf("render=%v want=%v", render, want)
	}
	for key, value := range want {
		if render[key] != value {
			t.Fatalf("render[%q]=%q want %q", key, render[key], value)
		}
	}
}

func TestOperatorAdoptionArtifactRenderBindsExactSafeProvisioningInputs(t *testing.T) {
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{
		{Kind: schema.KindFunction, Name: schema.Name{Schema: "app", Name: "normalize(text)"}, Spec: []byte(`{"body_digest":"sha256:b"}`)},
		{Kind: schema.KindProcedure, Name: schema.Name{Schema: "audit", Name: "record()"}, Spec: []byte(`{"body_digest":"sha256:a"}`)},
		{Kind: schema.KindExtension, Name: schema.Name{Schema: "app", Name: "pg_trgm"}, Spec: []byte(`{"version":"1.6"}`)},
		{Kind: schema.KindExtension, Name: schema.Name{Schema: "audit", Name: "btree_gist"}, Spec: []byte(`{"version":"1.7"}`)},
	}}}
	render := operatorAdoptionArtifactRender(desired, map[string]string{
		"postgres_version": "17", "reviewed_routine_digests": "sha256:stale", "extension_allowlist": "stale",
	})
	want := map[string]string{
		"postgres_version":          "17",
		"reviewed_routine_digests":  "sha256:a,sha256:b",
		"extension_allowlist":       "btree_gist,pg_trgm",
		"extension_version.pg_trgm": "1.6", "extension_schemas.pg_trgm": "app",
		"extension_version.btree_gist": "1.7", "extension_schemas.btree_gist": "audit",
	}
	if len(render) != len(want) {
		t.Fatalf("render=%v want=%v", render, want)
	}
	for key, value := range want {
		if render[key] != value {
			t.Fatalf("render[%q]=%q want %q", key, render[key], value)
		}
	}
	for _, forbidden := range []string{"allow_unsafe_routine_languages", "allow_privileged_routines", "allow_untrusted_extensions"} {
		if render[forbidden] != "" {
			t.Fatalf("adoption implicitly enabled %s", forbidden)
		}
	}
}

func TestOperatorAdoptionArtifactRoundTripsComplexHCL(t *testing.T) {
	developmentURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if developmentURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, developmentURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	fixture := `
drop extension if exists pg_trgm cascade;
drop extension if exists btree_gist cascade;
drop schema if exists autosql_adopt_app cascade;
drop schema if exists autosql_adopt_audit cascade;
create schema autosql_adopt_app;
create schema autosql_adopt_audit;
create extension pg_trgm with schema autosql_adopt_app;
create extension btree_gist with schema autosql_adopt_app;
create table autosql_adopt_app.widgets(id bigint primary key, label text not null);
create index widgets_label_idx on autosql_adopt_app.widgets(label);
create function autosql_adopt_app.normalize_label(value text) returns text language sql immutable as $$ select lower(value) $$;
create table autosql_adopt_audit.events(
  id bigint primary key,
  widget_id bigint references autosql_adopt_app.widgets(id)
);
create index events_widget_idx on autosql_adopt_audit.events(widget_id);
create materialized view autosql_adopt_audit.widget_counts as
select w.id, count(e.id)::bigint as event_count
from autosql_adopt_app.widgets w
left join autosql_adopt_audit.events e on e.widget_id = w.id
group by w.id;
`
	if _, err = conn.Exec(ctx, fixture); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop extension if exists pg_trgm cascade; drop extension if exists btree_gist cascade; drop schema if exists autosql_adopt_app cascade; drop schema if exists autosql_adopt_audit cascade`)
	desired, err := postgres.InspectURL(ctx, developmentURL, postgres.Options{Schemas: []string{"autosql_adopt_app", "autosql_adopt_audit"}})
	if err != nil {
		t.Fatal(err)
	}
	hcl, err := source.FormatHCL(desired)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadContext(ctx, source.Input{URI: "issue-59.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	request := generationFixture(t, t.TempDir(), developmentURL, developmentURL)
	request.Desired = desired
	request.ProductionIdentity = "operator-adopt/issue-59"
	request.DatabaseIdentity = "issue-59"
	workspace, err := operatorSimulationFactory(nil, desired).CreateWorkspace(ctx, simulate.Config{
		DevelopmentURL:      developmentURL,
		DevelopmentIdentity: request.DevelopmentIdentity,
		ProductionIdentity:  request.ProductionIdentity,
		CleanupTimeout:      15 * time.Second,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = materializeOperatorCurrent(ctx, workspace.URL(), desired, plan.Options{Render: operatorAdoptionArtifactRender(desired, map[string]string{"postgres_version": "17"})}); err != nil {
		_ = workspace.Cleanup(context.Background())
		t.Fatalf("materialize complex adoption schema: %v", err)
	}
	if err = workspace.Cleanup(context.Background()); err != nil {
		t.Fatal(err)
	}
	result, err := (GenerateService{}).BuildOperatorArtifact(ctx, OperatorArtifactRequest{
		Generation: request, Current: desired, Desired: desired, Adopt: true,
		Render: map[string]string{"postgres_version": "17"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.Artifact.Plan.FromFingerprint != result.Artifact.Plan.ToFingerprint || len(result.Artifact.Plan.Changes.Changes) != 0 || len(result.Artifact.Plan.Steps) != 0 {
		t.Fatalf("adoption artifact was not a no-op: %+v", result.Artifact.Plan)
	}
}

// TestOperatorTransitionArtifactAuthorizesExtensionsFromCurrentSchema covers the
// transition path (neither BootstrapTarget nor Adopt), which is how an artifact
// is published for a database that AutoSQL already manages.
//
// Bootstrap mode never materializes anything (current is empty) and adopt mode
// authorizes from desired, so transition mode was the only path that reached the
// replay renderer with an empty extension_allowlist. Materializing the current
// schema then failed with "extension <name> is not present in extension_allowlist"
// for any database using an extension -- which is every real one -- making
// transition artifacts unpublishable.
func TestOperatorTransitionArtifactAuthorizesExtensionsFromCurrentSchema(t *testing.T) {
	developmentURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if developmentURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 120*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, developmentURL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())

	reset := `drop extension if exists btree_gist cascade; drop schema if exists autosql_transition_app cascade;`
	if _, err = conn.Exec(ctx, reset+`
create schema autosql_transition_app;
create extension btree_gist with schema autosql_transition_app;
create table autosql_transition_app.widgets(id bigint primary key, label text not null);
`); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), reset)

	inspect := func() schema.Document {
		t.Helper()
		document, inspectErr := postgres.InspectURL(ctx, developmentURL, postgres.Options{Schemas: []string{"autosql_transition_app"}})
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		document, inspectErr = postgres.New().Normalize(ctx, document)
		if inspectErr != nil {
			t.Fatal(inspectErr)
		}
		return document
	}

	// current: the live schema, extension included.
	current := inspect()
	// desired: current plus one table. Purely additive, so the safety analyzer
	// raises no AUTOSQL001 "object is dropped" error and the run reaches the
	// materialize step this test is about.
	if _, err = conn.Exec(ctx, `create table autosql_transition_app.gadgets(id bigint primary key)`); err != nil {
		t.Fatal(err)
	}
	desired := inspect()

	request := generationFixture(t, t.TempDir(), developmentURL, developmentURL)
	request.Desired = desired
	request.PostgresVersion = 17
	// The fixture resolves both identities from one URL; validateGenerateRequest
	// rejects a request whose development and production identities are equal.
	request.ProductionIdentity = "operator-transition/extension-fixture"
	request.DatabaseIdentity = "extension-fixture"

	if _, err = (GenerateService{}).BuildOperatorArtifact(ctx, OperatorArtifactRequest{
		Generation: request, Current: current, Desired: desired,
		Render: map[string]string{"postgres_version": "17"},
	}); err != nil {
		t.Fatalf("transition artifact for a schema with an extension: %v", err)
	}
}

func TestGenerationGuardrailAppliesSafetySuppressions(t *testing.T) {
	dropped := &schema.Resource{Kind: schema.KindTable, Name: schema.Name{Schema: "public", Name: "dead_table"}, Spec: []byte(`{}`)}
	dropped.ID = schema.StableID(dropped.Kind, dropped.Name)
	changes := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "drop-dead", Operation: schema.OperationDrop, ResourceID: dropped.ID, Before: dropped}}}
	input := safety.Input{Changes: changes, Target: safety.Target{Engine: "postgresql", Version: 17}}

	dropDiagnostic := func(ds []safety.Diagnostic) *safety.Diagnostic {
		t.Helper()
		for i := range ds {
			if ds[i].Rule == safety.RuleDropObject {
				return &ds[i]
			}
		}
		t.Fatalf("no AUTOSQL001 diagnostic in %#v", ds)
		return nil
	}

	// Without a suppression the drop is an error-severity diagnostic that
	// fails artifact generation closed.
	unsuppressed, err := generationGuardrail(GenerateRequest{Environment: "production"}).Safety.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if d := dropDiagnostic(unsuppressed); d.Suppressed != nil || d.Severity != safety.SeverityError {
		t.Fatalf("expected an unsuppressed error-severity AUTOSQL001, got %#v", d)
	}

	suppression := safety.Suppression{Rule: safety.RuleDropObject, ObjectID: dropped.ID, Reason: "approved by the schema-destructive gate"}
	g := generationGuardrail(GenerateRequest{Environment: "production", SafetySuppressions: []safety.Suppression{suppression}})
	if len(g.Safety.Suppressions) != 1 || g.Safety.Suppressions[0] != suppression {
		t.Fatalf("guardrail did not carry the request suppressions: %#v", g.Safety.Suppressions)
	}
	suppressed, err := g.Safety.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if d := dropDiagnostic(suppressed); d.Suppressed == nil || d.Suppressed.Reason != suppression.Reason {
		t.Fatalf("expected the drop diagnostic suppressed with its reason, got %#v", d)
	}

	// A suppression naming a different object must not leak onto this drop.
	other := generationGuardrail(GenerateRequest{Environment: "production", SafetySuppressions: []safety.Suppression{{Rule: safety.RuleDropObject, ObjectID: "table:000000000000000000000000", Reason: "unrelated"}}})
	diags, err := other.Safety.Run(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	if d := dropDiagnostic(diags); d.Suppressed != nil {
		t.Fatalf("suppression leaked across object IDs: %#v", d)
	}
}

func TestValidateGenerateRequestValidatesSafetySuppressions(t *testing.T) {
	doc, err := source.LoadContext(context.Background(), source.Input{URI: "desired.sql", Format: source.FormatSQL, Data: []byte("CREATE SCHEMA app; CREATE TABLE app.widgets (id bigint);")})
	if err != nil {
		t.Fatal(err)
	}
	_, generator, _ := ed25519.GenerateKey(rand.Reader)
	_, signer, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC()
	r := GenerateRequest{Directory: t.TempDir(), Version: "1", Label: "create_widgets", Format: "sql", Desired: doc, DevelopmentURL: "postgres://example.invalid/dev", DevelopmentIdentity: "dev", ProductionIdentity: "prod", Environment: "test", DatabaseIdentity: "db", SourceRevision: "test-revision", Author: "author", Requester: "requester", PostgresVersion: 16, Policy: policy.Document{Version: policy.LanguageVersion, Rules: []policy.Rule{{Name: "allow", Target: "all", Assert: policy.Expression{Eq: []any{true, true}}, Message: "allowed"}}}, PolicyIdentity: "test-policy/v1", ApprovalPolicy: approval.Policy{Environments: map[string]approval.EnvironmentPolicy{"test": {Allowed: true}}}, Authority: generationTestAuthority{at: now, expires: now.Add(time.Hour)}, ApprovalAudit: &approval.Chain{Sink: &approval.FileSink{Path: filepath.Join(t.TempDir(), "approval.audit")}}, Approvals: []approval.Approval{{Approver: "reviewer", ApprovedAt: now, ExpiresAt: now.Add(time.Hour), Proof: "trusted-proof"}}, CreatedAt: now, ExpiresAt: now.Add(time.Hour), GeneratorKeyID: "generator", GeneratorPurpose: "migration-generator", SigningKeyID: "release", GeneratorPrivateKey: generator, SigningPrivateKey: signer}
	if err := validateGenerateRequest(r); err != nil {
		t.Fatalf("baseline request without suppressions: %v", err)
	}
	r.SafetySuppressions = []safety.Suppression{{Rule: safety.RuleDropObject, ObjectID: "table:abc", Reason: "approved destructive change"}}
	if err := validateGenerateRequest(r); err != nil {
		t.Fatalf("valid suppression rejected: %v", err)
	}
	r.SafetySuppressions = []safety.Suppression{{Rule: safety.RuleDropObject, ObjectID: "table:abc"}}
	if err := validateGenerateRequest(r); err == nil {
		t.Fatal("a suppression without a reason was accepted")
	}
}
