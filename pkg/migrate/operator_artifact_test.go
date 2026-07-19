package migrate

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/bootstrap"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/precheck"
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
