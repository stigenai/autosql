package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"autosql/pkg/bootstrap"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/source"

	"github.com/jackc/pgx/v5"
)

type completeBootstrapManifest struct {
	Version            int                 `json:"version"`
	Namespace          string              `json:"namespace"`
	NamespaceOwner     string              `json:"namespace_owner"`
	Counts             map[schema.Kind]int `json:"counts"`
	CellRoutines       int                 `json:"cell_routines"`
	RepositoryRoutines int                 `json:"repository_routines"`
	RLSTables          int                 `json:"rls_tables"`
	ExplicitGrants     int                 `json:"explicit_grants"`
	Fingerprint        string              `json:"fingerprint"`
}

func TestCanonicalCompleteBootstrapInventoryManifest(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	// This is intentionally a single end-to-end budget: the proof builds the
	// 1,007-resource fixture, plans it twice, exercises interruption recovery
	// before every phase, and inspects the result. Hosted matrix runners are
	// substantially slower than local development machines.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	const namespace = "autosql_canonical_cell"
	cleanupCompleteBootstrapInventory(t, ctx, conn, namespace)
	defer cleanupCompleteBootstrapInventory(t, context.Background(), conn, namespace)
	if _, err := conn.Exec(ctx, completeBootstrapInventorySQL(namespace)); err != nil {
		t.Fatal(err)
	}

	inspected, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}, Advanced: true})
	if err != nil {
		t.Fatal(err)
	}
	desired := filterCompleteBootstrapInventory(t, inspected, namespace)
	target := bootstrap.DatabaseTarget{
		Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: "db.internal", TLSMode: "verify-full"},
		MaintenanceDatabase: "postgres", Name: "canonical_cell", Owner: "postgres", ConnectionLimit: -1, AllowConnections: true,
	}
	completeHCL := desired
	completeHCL.Graph.Resources = append([]schema.Resource(nil), desired.Graph.Resources...)
	targetSpec, err := json.Marshal(target)
	if err != nil {
		t.Fatal(err)
	}
	completeHCL.Graph.Resources = append(completeHCL.Graph.Resources, schema.Resource{
		ID: schema.StableID(schema.KindDatabase, schema.Name{Name: target.Name}), Kind: schema.KindDatabase,
		Name: schema.Name{Name: target.Name}, Spec: targetSpec,
	})
	completeHCL.Normalize()
	hcl, err := source.FormatHCL(completeHCL)
	if err != nil {
		t.Fatal(err)
	}
	if output := os.Getenv("AUTOSQL_COMPLETE_HCL_OUTPUT"); output != "" {
		if err := os.WriteFile(output, hcl, 0o600); err != nil {
			t.Fatal(err)
		}
	}
	reloaded, err := source.LoadContext(ctx, source.Input{URI: "complete-bootstrap.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	var reloadedSchema schema.Document
	reloadedSchema.Version = reloaded.Version
	reloadedSchema.Annotations = reloaded.Annotations
	for _, resource := range reloaded.Graph.Resources {
		if resource.Kind == schema.KindDatabase {
			declared, err := postgres.DatabaseTargetFromResource(resource)
			if err != nil || declared != target.Normalize() {
				t.Fatalf("complete HCL target=%+v err=%v", declared, err)
			}
			continue
		}
		reloadedSchema.Graph.Resources = append(reloadedSchema.Graph.Resources, resource)
	}
	reloadedSchema, err = postgres.New().Normalize(ctx, reloadedSchema)
	if err != nil {
		t.Fatal(err)
	}
	if changes, diffErr := schema.Diff(desired, reloadedSchema, schema.DiffOptions{}); diffErr != nil || len(changes.Changes) != 0 {
		t.Fatalf("complete HCL schema round trip changes=%+v err=%v", changes.Changes, diffErr)
	}
	assertFingerprint(t, reloadedSchema, desired)

	manifest := manifestForCompleteBootstrap(t, desired, namespace)
	assertCompleteBootstrapManifest(t, manifest)
	first, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	second, err := json.Marshal(manifestForCompleteBootstrap(t, reloadedSchema, namespace))
	if err != nil || !slices.Equal(first, second) {
		t.Fatalf("manifest is not deterministic: err=%v\n%s\n%s", err, first, second)
	}

	options := reviewedRoutineRenderOptions(desired)
	options["extension_allowlist"] = "hstore,pgcrypto"
	for _, resource := range desired.Graph.Resources {
		if resource.Kind != schema.KindExtension {
			continue
		}
		values := map[string]any{}
		_ = json.Unmarshal(resource.Spec, &values)
		version, _ := values["version"].(string)
		options["extension_version."+resource.Name.Name] = version
		options["extension_schemas."+resource.Name.Name] = resource.Name.Schema
	}
	var serverVersion int
	if err := conn.QueryRow(ctx, `select current_setting('server_version_num')::integer`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	options["postgres_version"] = fmt.Sprintf("%d", serverVersion/10000)
	report, err := postgres.PreflightProvisioning(ctx, desired, options)
	if err != nil || !report.Supported {
		t.Fatalf("complete inventory preflight=%+v err=%v", report.Diagnostics, err)
	}
	options["concurrent_indexes"] = "true"
	firstPlan, err := postgres.PlanDatabaseBootstrap(ctx, target, reloaded, plan.Options{Render: options})
	if err != nil {
		t.Fatal(err)
	}
	secondPlan, err := postgres.PlanDatabaseBootstrap(ctx, target, reloaded, plan.Options{Render: options})
	if err != nil {
		t.Fatal(err)
	}
	firstPlanJSON, err := firstPlan.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	secondPlanJSON, err := secondPlan.MarshalCanonical()
	if err != nil || !slices.Equal(firstPlanJSON, secondPlanJSON) {
		t.Fatalf("complete bootstrap plan is not byte deterministic: err=%v", err)
	}
	assertCompleteBootstrapScaleBudget(t, desired, hcl, firstPlan, firstPlanJSON)
	assertCompleteBootstrapPlan(t, firstPlan)

	// Prove the actual empty-database executor against the same complete graph.
	// Remove the source fixture first because roles are cluster-wide and must be
	// created by the bootstrap rather than inherited from the inspection source.
	cleanupCompleteBootstrapInventory(t, ctx, conn, namespace)
	config, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	var owner string
	if err := conn.QueryRow(ctx, `select current_user`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	executionTarget := bootstrap.DatabaseTarget{
		Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: config.Host, Port: uint16(config.Port), TLSMode: "disable"},
		MaintenanceDatabase: config.Database, Name: fmt.Sprintf("autosql_complete_bootstrap_%d", time.Now().UnixNano()), Owner: owner,
		Encoding: "UTF8", Template: "template0", Tablespace: "pg_default", ConnectionLimit: -1, AllowConnections: true,
	}
	defer postgres.DropDatabaseURL(context.Background(), url, executionTarget.Name, true)
	executionPlan, err := postgres.PlanDatabaseBootstrap(ctx, executionTarget, desired, plan.Options{Render: options})
	if err != nil {
		t.Fatal(err)
	}
	interrupted := map[string]bool{}
	var execution postgres.BootstrapExecutionResult
	for attempt := 0; attempt <= len(executionPlan.Phases); attempt++ {
		execution, err = postgres.ExecuteDatabaseBootstrapURL(ctx, url, executionPlan, postgres.BootstrapExecutionHooks{BeforePhase: func(_ context.Context, phase bootstrap.BootstrapPhase) error {
			if !interrupted[phase.ID] {
				interrupted[phase.ID] = true
				return fmt.Errorf("seeded executor hook detail")
			}
			return nil
		}})
		if err == nil {
			break
		}
		if strings.Contains(err.Error(), "seeded executor") {
			t.Fatalf("bootstrap error leaked hook detail: %v", err)
		}
	}
	if err != nil || !execution.Completed || !execution.Resumed {
		if len(execution.Events) > 0 {
			failedPhase := execution.Events[len(execution.Events)-1].PhaseID
			for _, phase := range executionPlan.Phases {
				if phase.ID == failedPhase {
					t.Logf("failed bootstrap phase stage=%s transaction=%s steps=%v", phase.Stage, phase.Transaction, phase.StepIDs)
				}
			}
		}
		t.Fatalf("complete empty-database bootstrap=%+v err=%v", execution, err)
	}
	wantInterruptions := len(executionPlan.Phases) - 1 // database preparation is not a schema phase hook
	if len(interrupted) != wantInterruptions {
		t.Fatalf("interrupted phases=%d want=%d", len(interrupted), wantInterruptions)
	}
	targetConfig := config.Copy()
	targetConfig.Database = executionTarget.Name
	targetConnection, err := pgx.ConnectConfig(ctx, targetConfig)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := postgres.InspectConn(ctx, targetConnection, postgres.Options{Schemas: []string{namespace}, Advanced: true})
	targetConnection.Close(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	actual = filterCompleteBootstrapInventory(t, actual, namespace)
	assertCompleteBootstrapManifest(t, manifestForCompleteBootstrap(t, actual, namespace))
	assertFingerprint(t, actual, desired)
	noopPlan, err := plan.Build(ctx, postgres.New(), actual, desired, plan.Options{Render: options})
	if err != nil || len(noopPlan.Changes.Changes) != 0 || len(noopPlan.Steps) != 0 {
		t.Fatalf("complete bootstrap second plan changes=%d steps=%d err=%v", len(noopPlan.Changes.Changes), len(noopPlan.Steps), err)
	}
	if err := postgres.AbortDatabaseBootstrapURL(ctx, url, executionPlan, true); err != nil {
		t.Fatal(err)
	}
	cleanupCompleteBootstrapInventory(t, ctx, conn, namespace)
}

func assertCompleteBootstrapPlan(t *testing.T, whole bootstrap.Plan) {
	t.Helper()
	if err := whole.Validate(); err != nil {
		t.Fatal(err)
	}
	if len(whole.Steps) == 0 || whole.Steps[0].Action != bootstrap.ActionPrepareDatabase || whole.Steps[0].Transaction != plan.TransactionProhibited {
		t.Fatalf("missing out-of-transaction database preparation: %+v", whole.Steps)
	}
	seen := map[string]int{}
	stages := map[bootstrap.ExecutionStage]bool{}
	accessStart, nonAccessEnd, prohibitedIndexes := len(whole.Steps), -1, 0
	for position, step := range whole.Steps {
		if _, duplicate := seen[step.ID]; duplicate {
			t.Fatalf("duplicate bootstrap step %s", step.ID)
		}
		for _, dependency := range step.DependsOn {
			dependencyPosition, exists := seen[dependency]
			if !exists || dependencyPosition >= position {
				t.Fatalf("step %s has missing/forward dependency %s", step.ID, dependency)
			}
		}
		seen[step.ID] = position
		stages[step.Stage] = true
		if step.Stage == bootstrap.StageAccess {
			accessStart = min(accessStart, position)
		} else {
			nonAccessEnd = max(nonAccessEnd, position)
		}
		if step.Stage == bootstrap.StageIndexes && step.Transaction == plan.TransactionProhibited {
			prohibitedIndexes++
		}
	}
	for _, required := range []bootstrap.ExecutionStage{bootstrap.StageDatabaseTarget, bootstrap.StageRoles, bootstrap.StageNamespaces, bootstrap.StageExtensions, bootstrap.StageRoutines, bootstrap.StageStorage, bootstrap.StageConstraints, bootstrap.StageIndexes, bootstrap.StageBehavior, bootstrap.StageAccess} {
		if !stages[required] {
			t.Errorf("complete bootstrap plan lacks %s stage", required)
		}
	}
	if accessStart <= nonAccessEnd {
		t.Fatalf("access handoff is not final: access starts at %d, non-access ends at %d", accessStart, nonAccessEnd)
	}
	if prohibitedIndexes != 315 {
		t.Fatalf("non-transactional indexes=%d want 315", prohibitedIndexes)
	}
}

func completeBootstrapInventorySQL(namespace string) string {
	var fixture strings.Builder
	fmt.Fprintf(&fixture, `create role autosql_cell_reader; create role autosql_cell_app; create role autosql_cell_owner; create schema %s authorization autosql_cell_owner; create extension hstore with schema %s; create extension pgcrypto with schema %s; create type %s.cell_composite as (label text, rank integer);`, namespace, namespace, namespace, namespace)
	for index := 0; index < 69; index++ {
		fmt.Fprintf(&fixture, `create table %s.t%02d(id bigint not null,parent_id bigint,tenant_id text not null,value integer not null,unique_value bigint not null,constraint t%02d_pkey primary key(id)`, namespace, index, index)
		if index < 27 {
			fmt.Fprintf(&fixture, `,constraint t%02d_unique unique(unique_value)`, index)
		}
		if index < 45 {
			fmt.Fprintf(&fixture, `,constraint t%02d_check check(value >= 0)`, index)
		}
		fixture.WriteString(`);`)
		if index < 7 {
			fmt.Fprintf(&fixture, `alter table %s.t%02d enable row level security; alter table %s.t%02d force row level security; create policy t%02d_reader on %s.t%02d for select to autosql_cell_reader using (tenant_id=current_setting('app.tenant_id',true)); create policy t%02d_app on %s.t%02d for all to autosql_cell_app using (tenant_id=current_setting('app.tenant_id',true)) with check (tenant_id=current_setting('app.tenant_id',true));`, namespace, index, namespace, index, index, namespace, index, index, namespace, index)
		}
	}
	for index := 1; index <= 56; index++ {
		fmt.Fprintf(&fixture, `alter table %s.t%02d add constraint t%02d_parent_fkey foreign key(parent_id) references %s.t00(id) on delete restrict;`, namespace, index, index, namespace)
	}
	for index := 0; index < 315; index++ {
		fmt.Fprintf(&fixture, `create index cell_idx_%03d on %s.t%02d(value);`, index, namespace, index%69)
	}
	for index := 0; index < 15; index++ {
		fmt.Fprintf(&fixture, `create function %s.cell_fn_%02d(input integer) returns integer language sql immutable strict parallel safe as $$ select input + %d $$;`, namespace, index, index)
	}
	for index := 0; index < 6; index++ {
		fmt.Fprintf(&fixture, `create function %s.repo_trigger_fn_%02d() returns trigger language plpgsql as $$ begin return new; end $$; create trigger repo_trigger_%02d before insert or update on %s.t00 for each row execute function %s.repo_trigger_fn_%02d();`, namespace, index, index, namespace, namespace, index)
	}
	for index := 0; index < 18; index++ {
		fmt.Fprintf(&fixture, `create function %s.repo_fn_%02d(input integer) returns integer language sql stable as $$ select input + %d $$;`, namespace, index, index)
	}
	for index := 0; index < 8; index++ {
		fmt.Fprintf(&fixture, `create procedure %s.repo_proc_%02d(IN input integer, INOUT result integer) language plpgsql as $$ begin result := input + %d; end $$;`, namespace, index, index)
	}
	fmt.Fprintf(&fixture, `grant usage on schema %s to autosql_cell_reader; grant select on table %s.t00 to autosql_cell_reader; grant insert on table %s.t00 to autosql_cell_app; grant autosql_cell_reader to autosql_cell_app; alter default privileges in schema %s grant select on tables to autosql_cell_reader; comment on schema %s is 'anonymized complete bootstrap inventory'; comment on table %s.t00 is 'representative commented table';`, namespace, namespace, namespace, namespace, namespace, namespace)
	return fixture.String()
}

func filterCompleteBootstrapInventory(t testing.TB, input schema.Document, namespace string) schema.Document {
	t.Helper()
	allowedRoles := map[string]bool{"autosql_cell_reader": true, "autosql_cell_app": true, "autosql_cell_owner": true}
	keep := map[string]bool{}
	for _, resource := range input.Graph.Resources {
		securityResource := resource.Kind == schema.KindGrant || resource.Kind == schema.KindMembership || resource.Kind == schema.KindDefaultPrivilege
		if !securityResource && (resource.Name.Schema == namespace || resource.Kind == schema.KindSchema && resource.Name.Name == namespace) || resource.Kind == schema.KindRole && allowedRoles[resource.Name.Name] {
			keep[resource.ID] = true
			continue
		}
		values := map[string]any{}
		_ = json.Unmarshal(resource.Spec, &values)
		switch resource.Kind {
		case schema.KindGrant:
			grantee, _ := values["grantee"].(string)
			if allowedRoles[grantee] {
				keep[resource.ID] = true
			}
		case schema.KindMembership:
			parent, _ := values["parent"].(string)
			member, _ := values["member"].(string)
			if allowedRoles[parent] && allowedRoles[member] {
				keep[resource.ID] = true
			}
		case schema.KindDefaultPrivilege:
			if values["schema"] == namespace {
				keep[resource.ID] = true
			}
		}
	}
	out := schema.Document{Version: input.Version, Annotations: input.Annotations}
	for _, original := range input.Graph.Resources {
		if !keep[original.ID] {
			continue
		}
		resource := original
		values := map[string]any{}
		if resource.Kind != schema.KindRole && resource.Kind != schema.KindDefaultPrivilege && json.Unmarshal(resource.Spec, &values) == nil {
			keepOwner := resource.Kind == schema.KindSchema && resource.Name.Name == namespace
			if !keepOwner {
				delete(values, "owner")
			}
			resource.Spec, _ = json.Marshal(values)
		}
		keepOwnerDependency := resource.Kind == schema.KindSchema && resource.Name.Name == namespace
		resource.Dependencies = slices.DeleteFunc(append([]schema.Dependency(nil), resource.Dependencies...), func(dependency schema.Dependency) bool {
			return !keep[dependency.Target] || dependency.Type == schema.DependencyOwns && !keepOwnerDependency
		})
		out.Graph.Resources = append(out.Graph.Resources, resource)
	}
	out, err := postgres.New().Normalize(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func manifestForCompleteBootstrap(t *testing.T, document schema.Document, namespace string) completeBootstrapManifest {
	t.Helper()
	manifest := completeBootstrapManifest{Version: 1, Namespace: namespace, Counts: map[schema.Kind]int{}}
	for _, resource := range document.Graph.Resources {
		manifest.Counts[resource.Kind]++
		if resource.Kind == schema.KindFunction || resource.Kind == schema.KindProcedure {
			if strings.HasPrefix(resource.Name.Name, "cell_fn_") {
				manifest.CellRoutines++
			} else if strings.HasPrefix(resource.Name.Name, "repo_") {
				manifest.RepositoryRoutines++
			}
		}
		if resource.Kind == schema.KindTable {
			values := map[string]any{}
			_ = json.Unmarshal(resource.Spec, &values)
			if values["row_security"] == true {
				manifest.RLSTables++
			}
		}
		if resource.Kind == schema.KindSchema && resource.Name.Name == namespace {
			values := map[string]any{}
			_ = json.Unmarshal(resource.Spec, &values)
			manifest.NamespaceOwner, _ = values["owner"].(string)
		}
		if resource.Kind == schema.KindGrant {
			values := map[string]any{}
			_ = json.Unmarshal(resource.Spec, &values)
			if values["grantee"] != "autosql_cell_owner" {
				manifest.ExplicitGrants++
			}
		}
	}
	fingerprint, err := schema.SemanticFingerprint(document)
	if err != nil {
		t.Fatal(err)
	}
	manifest.Fingerprint = fingerprint
	return manifest
}

func assertCompleteBootstrapManifest(t *testing.T, manifest completeBootstrapManifest) {
	t.Helper()
	want := map[schema.Kind]int{
		schema.KindIndex: 315, schema.KindPrimaryKey: 69, schema.KindForeignKey: 56,
		schema.KindCheckConstraint: 45, schema.KindUniqueConstraint: 27,
		schema.KindTrigger: 6, schema.KindPolicy: 14, schema.KindExtension: 2,
		schema.KindGrant: 5, schema.KindComposite: 1,
	}
	for kind, count := range want {
		if manifest.Counts[kind] != count {
			t.Errorf("%s count=%d want=%d", kind, manifest.Counts[kind], count)
		}
	}
	if manifest.CellRoutines != 15 || manifest.RepositoryRoutines != 32 || manifest.RLSTables != 7 || manifest.ExplicitGrants != 3 || manifest.NamespaceOwner != "autosql_cell_owner" || manifest.Fingerprint == "" {
		t.Fatalf("manifest=%+v", manifest)
	}
}

func cleanupCompleteBootstrapInventory(t testing.TB, ctx context.Context, conn *pgx.Conn, namespace string) {
	t.Helper()
	_, err := conn.Exec(ctx, fmt.Sprintf(`drop extension if exists hstore cascade; drop extension if exists pgcrypto cascade; drop schema if exists %s cascade; drop role if exists autosql_cell_app; drop role if exists autosql_cell_reader; drop role if exists autosql_cell_owner`, namespace))
	if err != nil {
		t.Fatal(err)
	}
}

func assertCompleteBootstrapScaleBudget(t testing.TB, desired schema.Document, hcl []byte, whole bootstrap.Plan, encoded []byte) {
	t.Helper()
	const (
		maxResources = 1_250
		maxHCLBytes  = 4 << 20
		maxPlanBytes = 8 << 20
		maxSteps     = 2_000
		maxPhases    = 1_000
		maxSQLBytes  = 4 << 20
		maxStepDeps  = 1_200
	)
	locks := map[plan.LockLevel]int{}
	transactions := map[plan.TransactionMode]int{}
	sqlBytes, maxDependencies, scanSteps := 0, 0, 0
	for _, step := range whole.SchemaPlan.Steps {
		locks[step.Lock]++
		transactions[step.Transaction]++
		sqlBytes += len(step.SQL)
		maxDependencies = max(maxDependencies, len(step.DependsOn))
		if step.Impact.Scans {
			scanSteps++
		}
	}
	t.Logf("complete_bootstrap_scale resources=%d hcl_bytes=%d plan_bytes=%d sql_bytes=%d steps=%d phases=%d max_step_dependencies=%d locks=%v transactions=%v scan_steps=%d", len(desired.Graph.Resources), len(hcl), len(encoded), sqlBytes, len(whole.Steps), len(whole.Phases), maxDependencies, locks, transactions, scanSteps)
	if len(desired.Graph.Resources) > maxResources || len(hcl) > maxHCLBytes || len(encoded) > maxPlanBytes || len(whole.Steps) > maxSteps || len(whole.Phases) > maxPhases || sqlBytes > maxSQLBytes || maxDependencies > maxStepDeps {
		t.Fatalf("complete bootstrap exceeded structural scale budget")
	}
	if transactions[plan.TransactionProhibited] < 315 || locks[plan.LockShare] < 315 || scanSteps < 315 {
		t.Fatalf("online-index lock exposure was not represented: locks=%v transactions=%v scans=%d", locks, transactions, scanSteps)
	}
}

func BenchmarkCompleteBootstrapPipeline(b *testing.B) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		b.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		b.Fatal(err)
	}
	defer conn.Close(ctx)
	const namespace = "autosql_benchmark_cell"
	cleanupCompleteBootstrapInventory(b, ctx, conn, namespace)
	defer cleanupCompleteBootstrapInventory(b, ctx, conn, namespace)
	if _, err := conn.Exec(ctx, completeBootstrapInventorySQL(namespace)); err != nil {
		b.Fatal(err)
	}
	inspected, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}, Advanced: true})
	if err != nil {
		b.Fatal(err)
	}
	desired := filterCompleteBootstrapInventory(b, inspected, namespace)
	hcl, err := source.FormatHCL(desired)
	if err != nil {
		b.Fatal(err)
	}
	options := reviewedRoutineRenderOptions(desired)
	options["extension_allowlist"] = "hstore,pgcrypto"
	options["concurrent_indexes"] = "true"
	for _, resource := range desired.Graph.Resources {
		if resource.Kind != schema.KindExtension {
			continue
		}
		values := map[string]any{}
		_ = json.Unmarshal(resource.Spec, &values)
		options["extension_version."+resource.Name.Name], _ = values["version"].(string)
		options["extension_schemas."+resource.Name.Name] = resource.Name.Schema
	}
	var serverVersion int
	if err := conn.QueryRow(ctx, `select current_setting('server_version_num')::integer`).Scan(&serverVersion); err != nil {
		b.Fatal(err)
	}
	options["postgres_version"] = fmt.Sprintf("%d", serverVersion/10000)
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: "db.internal", TLSMode: "verify-full"}, MaintenanceDatabase: "postgres", Name: "benchmark_cell", Owner: "postgres", ConnectionLimit: -1, AllowConnections: true}
	whole, err := postgres.PlanDatabaseBootstrap(ctx, target, desired, plan.Options{Render: options})
	if err != nil {
		b.Fatal(err)
	}

	b.Run("inspect", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{namespace}, Advanced: true}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("normalize", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := postgres.New().Normalize(ctx, desired); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("hcl_format_load", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			formatted, err := source.FormatHCL(desired)
			if err == nil {
				_, err = source.LoadContext(ctx, source.Input{URI: "benchmark.hcl", Format: source.FormatHCLSource, Data: formatted})
			}
			if err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("preflight_diff_render_schedule", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			report, err := postgres.PreflightProvisioning(ctx, desired, options)
			if err != nil || !report.Supported {
				b.Fatalf("preflight supported=%t err=%v", report.Supported, err)
			}
			if _, err := postgres.PlanDatabaseBootstrap(ctx, target, desired, plan.Options{Render: options}); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.Run("plan_serialize", func(b *testing.B) {
		b.ReportAllocs()
		for range b.N {
			if _, err := whole.MarshalCanonical(); err != nil {
				b.Fatal(err)
			}
		}
	})
	b.SetBytes(int64(len(hcl)))
}
