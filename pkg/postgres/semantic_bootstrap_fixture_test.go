package postgres_test

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"os"
	"slices"
	"strings"
	"testing"
	"time"

	"autosql/pkg/bootstrap"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"

	"github.com/jackc/pgx/v5"
)

const semanticCellCommentedObjects = 248

var semanticCellRoles = map[string]bool{
	"autosql_semantic_owner":  true,
	"autosql_semantic_reader": true,
	"autosql_semantic_app":    true,
}

type semanticTable struct {
	name    string
	columns []string
}

var semanticTables = []semanticTable{
	{"organizations", []string{"id", "parent_id", "external_id", "name", "status", "metadata", "tags", "enabled", "region", "created_at", "updated_at", "archived_at", "version"}},
	{"users", []string{"id", "tenant_id", "external_id", "name", "status", "metadata", "tags", "enabled", "email", "created_at", "updated_at", "archived_at", "version"}},
	{"workspaces", []string{"id", "tenant_id", "external_id", "name", "status", "metadata", "tags", "enabled", "slug", "created_at", "updated_at", "archived_at", "version"}},
	{"applications", []string{"id", "tenant_id", "external_id", "name", "status", "metadata", "tags", "enabled", "repository", "created_at", "updated_at", "archived_at", "version"}},
	{"environments", []string{"id", "tenant_id", "external_id", "name", "status", "metadata", "tags", "enabled", "tier", "created_at", "updated_at", "archived_at", "version"}},
	{"deployments", []string{"id", "tenant_id", "external_id", "name", "status", "metadata", "tags", "enabled", "commit_sha", "created_at", "updated_at", "archived_at", "version"}},
	{"jobs", []string{"id", "tenant_id", "queue", "status", "lifecycle_state", "state_v2", "payload", "headers", "tags", "run_at", "local_start", "dbos_updated_at", "attempt_count", "worker_id", "created_at", "updated_at"}},
	{"job_attempts", []string{"id", "tenant_id", "external_id", "name", "status", "metadata", "tags", "enabled", "job_id", "created_at", "updated_at", "archived_at", "version"}},
	{"schedules", []string{"id", "tenant_id", "external_id", "name", "status", "metadata", "tags", "enabled", "cron_expression", "created_at", "updated_at", "archived_at", "version"}},
	{"webhooks", []string{"id", "tenant_id", "external_id", "name", "status", "metadata", "tags", "enabled", "endpoint", "created_at", "updated_at", "archived_at", "version"}},
	{"audit_events", []string{"id", "tenant_id", "external_id", "name", "status", "metadata", "tags", "enabled", "actor_id", "created_at", "updated_at", "archived_at", "version"}},
	{"api_keys", []string{"id", "tenant_id", "external_id", "name", "status", "metadata", "tags", "enabled", "key_digest", "created_at", "updated_at", "archived_at", "version"}},
	{"feature_flags", []string{"id", "tenant_id", "external_id", "name", "status", "metadata", "tags", "enabled", "rollout", "created_at", "updated_at", "archived_at", "version"}},
	{"schema_migrations", []string{"id", "tenant_id", "external_id", "name", "status", "metadata", "tags", "enabled", "checksum", "created_at", "updated_at", "archived_at", "version"}},
	{"outbox_messages", []string{"id", "tenant_id", "external_id", "name", "status", "metadata", "tags", "enabled", "topic", "created_at", "updated_at", "archived_at", "version"}},
	{"idempotency_keys", []string{"id", "tenant_id", "external_id", "name", "status", "metadata", "tags", "enabled", "request_hash", "created_at", "updated_at", "archived_at", "version"}},
}

func semanticCellBootstrapSQL(namespace string) string {
	var sql strings.Builder
	fmt.Fprintf(&sql, `
create role autosql_semantic_owner;
create role autosql_semantic_reader;
create role autosql_semantic_app;
create schema %[1]s authorization autosql_semantic_owner;
create extension hstore with schema %[1]s;
create extension pgcrypto with schema %[1]s;
set role autosql_semantic_owner;
create type %[1]s.job_status as enum ('pending','running','succeeded','failed','cancelled');
create function %[1]s.lifecycle_state_to_v2(value text) returns text language sql immutable strict parallel safe as $$
  select case value when 'queued' then 'pending' when 'executing' then 'running' when 'complete' then 'succeeded' when 'error' then 'failed' else 'pending' end
$$;
create function %[1]s.set_updated_at() returns trigger language plpgsql as $$
begin
  new.updated_at := statement_timestamp();
  return new;
end
$$;
create function %[1]s.reject_audit_mutation() returns trigger language plpgsql as $$
begin
  raise exception 'audit events are immutable';
end
$$;
create function %[1]s.claim_job(worker uuid) returns uuid language plpgsql volatile as $$
declare claimed uuid;
begin
  update %[1]s.jobs set worker_id=worker,lifecycle_state='executing',updated_at=statement_timestamp()
  where id=(select id from %[1]s.jobs where lifecycle_state='queued' and run_at<=now() order by run_at for update skip locked limit 1)
  returning id into claimed;
  return claimed;
end
$$;
create procedure %[1]s.prune_audit_events(retain interval) language plpgsql as $$
begin
  delete from %[1]s.audit_events where created_at < statement_timestamp() - retain;
end
$$;
`, namespace)

	for _, table := range semanticTables {
		if table.name == "jobs" {
			fmt.Fprintf(&sql, `create table %[1]s.jobs(
id uuid primary key default gen_random_uuid(),
tenant_id uuid not null references %[1]s.organizations(id) on delete cascade,
queue character varying(255) not null default 'default'::text,
status %[1]s.job_status not null default 'pending'::%[1]s.job_status,
lifecycle_state text not null default 'queued'::text,
state_v2 text generated always as (%[1]s.lifecycle_state_to_v2(lifecycle_state)) stored,
payload jsonb not null default '{}'::jsonb,
headers jsonb not null default '[]'::jsonb,
tags text[] not null default '{}'::text[],
run_at timestamptz not null default CURRENT_TIMESTAMP,
local_start time without time zone not null default '09:30:00'::time without time zone,
dbos_updated_at bigint not null default (extract(epoch from now())*1000)::bigint,
attempt_count integer not null default 0 check(attempt_count>=0),
worker_id uuid default '00000000-0000-0000-0000-000000000000'::uuid,
created_at timestamptz not null default now(),
updated_at timestamptz not null default now(),
constraint jobs_queue_external_unique unique(tenant_id,queue,id));`, namespace)
			continue
		}
		if table.name == "organizations" {
			fmt.Fprintf(&sql, `create table %[1]s.organizations(
id uuid primary key,parent_id uuid references %[1]s.organizations(id),external_id character varying(255) not null unique,
name character varying(255) not null,status character varying(63) not null,metadata jsonb not null,
tags text[] not null,enabled boolean not null,region character varying(63) not null,
created_at timestamptz not null,updated_at timestamptz not null,archived_at timestamptz,version bigint not null check(version>0));`, namespace)
			continue
		}
		extra := table.columns[8]
		extraType := "character varying(255)"
		if extra == "rollout" {
			extraType = "integer check (rollout between 0 and 100)"
		}
		fmt.Fprintf(&sql, `create table %[1]s.%[2]s(
id uuid primary key,tenant_id uuid not null references %[1]s.organizations(id) on delete cascade,
external_id character varying(255) not null,name character varying(255) not null,status character varying(63) not null,
metadata jsonb not null,tags text[] not null,enabled boolean not null,
%[3]s %[4]s,created_at timestamptz not null,updated_at timestamptz not null,archived_at timestamptz,
version bigint not null check(version>0),constraint %[2]s_tenant_external_unique unique(tenant_id,external_id));`, namespace, table.name, extra, extraType)
	}

	fmt.Fprintf(&sql, `
create index jobs_ready_idx on %[1]s.jobs(run_at,id) where lifecycle_state='queued';
create index jobs_tenant_status_idx on %[1]s.jobs(tenant_id,status) include(queue,attempt_count);
create index deployments_commit_idx on %[1]s.deployments(tenant_id,commit_sha);
create index audit_events_actor_idx on %[1]s.audit_events(tenant_id,actor_id,created_at desc);
create index api_keys_digest_idx on %[1]s.api_keys(key_digest) where enabled;
create index outbox_topic_idx on %[1]s.outbox_messages(topic,created_at) where status='active';
create trigger jobs_updated_at before update on %[1]s.jobs for each row execute function %[1]s.set_updated_at();
create trigger deployments_updated_at before update on %[1]s.deployments for each row execute function %[1]s.set_updated_at();
create trigger audit_events_immutable before update or delete on %[1]s.audit_events for each row execute function %[1]s.reject_audit_mutation();
alter table %[1]s.jobs enable row level security;
alter table %[1]s.jobs force row level security;
create policy jobs_reader on %[1]s.jobs for select to autosql_semantic_reader using (tenant_id=current_setting('app.tenant_id',true)::uuid);
create policy jobs_app on %[1]s.jobs for all to autosql_semantic_app using (tenant_id=current_setting('app.tenant_id',true)::uuid) with check (tenant_id=current_setting('app.tenant_id',true)::uuid);
alter table %[1]s.api_keys enable row level security;
alter table %[1]s.api_keys force row level security;
create policy api_keys_reader on %[1]s.api_keys for select to autosql_semantic_reader using (tenant_id=current_setting('app.tenant_id',true)::uuid);
create policy api_keys_app on %[1]s.api_keys for all to autosql_semantic_app using (tenant_id=current_setting('app.tenant_id',true)::uuid) with check (tenant_id=current_setting('app.tenant_id',true)::uuid);
reset role;
grant usage on schema %[1]s to autosql_semantic_reader,autosql_semantic_app;
grant select on all tables in schema %[1]s to autosql_semantic_reader;
grant select,insert,update,delete on all tables in schema %[1]s to autosql_semantic_app;
grant autosql_semantic_reader to autosql_semantic_app;
alter default privileges for role autosql_semantic_owner in schema %[1]s grant select on tables to autosql_semantic_reader;
`, namespace)

	comments := 0
	comment := func(statement string) {
		if comments < semanticCellCommentedObjects {
			sql.WriteString(statement)
			sql.WriteByte(';')
			comments++
		}
	}
	comment(fmt.Sprintf("comment on schema %s is 'Cell application namespace and ownership boundary'", namespace))
	comment(fmt.Sprintf("comment on type %s.job_status is 'Durable lifecycle state for queued work'", namespace))
	for _, table := range semanticTables {
		comment(fmt.Sprintf("comment on table %s.%s is 'Managed %s business records'", namespace, table.name, strings.ReplaceAll(table.name, "_", " ")))
		for _, column := range table.columns {
			comment(fmt.Sprintf("comment on column %s.%s.%s is '%s field for %s records'", namespace, table.name, column, strings.ReplaceAll(column, "_", " "), strings.ReplaceAll(table.name, "_", " ")))
		}
	}
	for _, table := range semanticTables {
		comment(fmt.Sprintf("comment on constraint %s_pkey on %s.%s is 'Stable primary identity for %s records'", table.name, namespace, table.name, strings.ReplaceAll(table.name, "_", " ")))
	}
	for _, item := range []struct{ kind, name, signature, text string }{
		{"function", "lifecycle_state_to_v2", "(text)", "Canonical mapping used by stored generated state"},
		{"function", "set_updated_at", "()", "Shared timestamp trigger implementation"},
		{"function", "reject_audit_mutation", "()", "Audit immutability enforcement"},
		{"function", "claim_job", "(uuid)", "Concurrent worker claim with skip locked"},
		{"procedure", "prune_audit_events", "(interval)", "Retention procedure for immutable audit history"},
	} {
		comment(fmt.Sprintf("comment on %s %s.%s%s is '%s'", item.kind, namespace, item.name, item.signature, item.text))
	}
	if comments != semanticCellCommentedObjects {
		panic(fmt.Sprintf("semantic comment fixture has %d comments, want %d", comments, semanticCellCommentedObjects))
	}
	return sql.String()
}

func semanticCellFixture(t testing.TB, ctx context.Context, rawURL, namespace string) (schema.Document, *pgx.ConnConfig, string, int, func()) {
	t.Helper()
	conn, err := pgx.Connect(ctx, rawURL)
	if err != nil {
		t.Fatal(err)
	}
	cleanupSemanticCell(t, ctx, conn, namespace)
	if _, err := conn.Exec(ctx, semanticCellBootstrapSQL(namespace)); err != nil {
		conn.Close(context.Background())
		t.Fatal(err)
	}
	inspected, err := postgres.InspectConn(ctx, conn, postgres.Options{Schemas: []string{namespace}, Advanced: true})
	if err != nil {
		conn.Close(context.Background())
		t.Fatal(err)
	}
	desired := filterSemanticCell(t, inspected, namespace)
	assertSemanticCell(t, desired, namespace)
	var owner string
	var serverVersion int
	if err := conn.QueryRow(ctx, `select current_user,current_setting('server_version_num')::integer`).Scan(&owner, &serverVersion); err != nil {
		conn.Close(context.Background())
		t.Fatal(err)
	}
	cleanupSemanticCell(t, ctx, conn, namespace)
	config, err := pgx.ParseConfig(rawURL)
	if err != nil {
		conn.Close(context.Background())
		t.Fatal(err)
	}
	cleanup := func() {
		cleanupSemanticCell(t, context.Background(), conn, namespace)
		conn.Close(context.Background())
	}
	return desired, config, owner, serverVersion / 10000, cleanup
}

func filterSemanticCell(t testing.TB, input schema.Document, namespace string) schema.Document {
	t.Helper()
	keep := map[string]bool{}
	for _, resource := range input.Graph.Resources {
		if resource.Name.Schema == namespace || resource.Kind == schema.KindSchema && resource.Name.Name == namespace || resource.Kind == schema.KindRole && semanticCellRoles[resource.Name.Name] {
			keep[resource.ID] = true
			continue
		}
		values := map[string]any{}
		_ = json.Unmarshal(resource.Spec, &values)
		switch resource.Kind {
		case schema.KindGrant:
			grantee, _ := values["grantee"].(string)
			keep[resource.ID] = semanticCellRoles[grantee] && (values["schema"] == namespace || strings.Contains(fmt.Sprint(values["object"]), namespace))
		case schema.KindMembership:
			keep[resource.ID] = semanticCellRoles[fmt.Sprint(values["parent"])] && semanticCellRoles[fmt.Sprint(values["member"])]
		case schema.KindDefaultPrivilege:
			keep[resource.ID] = values["schema"] == namespace
		}
	}
	out := schema.Document{Version: input.Version, Annotations: input.Annotations}
	for _, resource := range input.Graph.Resources {
		if !keep[resource.ID] {
			continue
		}
		if resource.Kind == schema.KindExtension {
			values := map[string]any{}
			if json.Unmarshal(resource.Spec, &values) == nil {
				delete(values, "owner")
				resource.Spec, _ = json.Marshal(values)
			}
			resource.Annotations = maps.Clone(resource.Annotations)
			delete(resource.Annotations, "comment")
		}
		resource.Dependencies = slices.DeleteFunc(append([]schema.Dependency(nil), resource.Dependencies...), func(dependency schema.Dependency) bool {
			return !keep[dependency.Target] || resource.Kind == schema.KindDefaultPrivilege && dependency.Type != schema.DependencyReferences || resource.Kind == schema.KindExtension && dependency.Type == schema.DependencyOwns
		})
		out.Graph.Resources = append(out.Graph.Resources, resource)
	}
	out, err := postgres.New().Normalize(context.Background(), out)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

func assertSemanticCell(t testing.TB, document schema.Document, namespace string) {
	t.Helper()
	comments := 0
	kinds := map[schema.Kind]int{}
	extensions, roles := map[string]bool{}, map[string]bool{}
	sentinels := map[string]bool{}
	lifecycleFunctions := map[string]bool{}
	resourceKinds := map[string]schema.Kind{}
	jobsTableID, lifecycleStateColumnID := "", ""
	for _, resource := range document.Graph.Resources {
		resourceKinds[resource.ID] = resource.Kind
		if resource.Kind == schema.KindTable && resource.Name.Schema == namespace && resource.Name.Name == "jobs" {
			jobsTableID = resource.ID
		}
		if resource.Kind == schema.KindColumn && resource.Name.Schema == namespace && resource.Name.Name == "lifecycle_state" {
			lifecycleStateColumnID = resource.ID
		}
		if resource.Kind == schema.KindFunction && strings.Contains(resource.Name.String(), "lifecycle_state_to_v2") {
			lifecycleFunctions[resource.ID] = true
			values := map[string]any{}
			_ = json.Unmarshal(resource.Spec, &values)
			if strings.Contains(fmt.Sprint(values["definition"]), "case value") && fmt.Sprint(values["body_digest"]) != "" {
				sentinels["lifecycle_function_body"] = true
			}
		}
	}
	for _, resource := range document.Graph.Resources {
		kinds[resource.Kind]++
		if resource.Annotations["comment"] != "" {
			comments++
		}
		values := map[string]any{}
		_ = json.Unmarshal(resource.Spec, &values)
		switch resource.Kind {
		case schema.KindExtension:
			extensions[resource.Name.Name] = true
		case schema.KindRole:
			roles[resource.Name.Name] = true
		case schema.KindColumn:
			defaultExpr := fmt.Sprint(values["default"])
			typ := fmt.Sprint(values["type"])
			generated := fmt.Sprint(values["generated"])
			rawSpec := string(resource.Spec)
			if strings.Contains(strings.ToLower(rawSpec), "extract(epoch from current_timestamp)") && strings.Contains(rawSpec, "OPERATOR(pg_catalog.*)") && strings.Contains(rawSpec, "1000") && strings.Contains(rawSpec, "bigint") {
				sentinels["dbos_arithmetic"] = true
			}
			if strings.Contains(typ, "character varying(255)") {
				sentinels["parameterized_type"] = true
			}
			for name, fragment := range map[string]string{"enum_default": "job_status", "array_default": "text[]", "time_default": "time without time zone", "cast_string_default": "::text"} {
				if strings.Contains(defaultExpr, fragment) {
					sentinels[name] = true
				}
			}
			switch resource.Name.Name {
			case "payload":
				sentinels["json_empty_object"] = defaultExpr == `'{}'::jsonb` || defaultExpr == `'{}'::pg_catalog.jsonb`
			case "headers":
				sentinels["json_empty_array"] = defaultExpr == `'[]'::jsonb` || defaultExpr == `'[]'::pg_catalog.jsonb`
			case "id":
				if resource.Name.Parent == jobsTableID {
					sentinels["uuid_generated"] = defaultExpr == "gen_random_uuid()" || defaultExpr == "pg_catalog.gen_random_uuid()"
				}
			case "worker_id":
				sentinels["uuid_fixed_cast"] = defaultExpr == `'00000000-0000-0000-0000-000000000000'::uuid` || defaultExpr == `'00000000-0000-0000-0000-000000000000'::pg_catalog.uuid`
			}
			if generated == "s" && strings.Contains(defaultExpr, "lifecycle_state_to_v2") {
				sentinels["generated_stored"] = true
				expected := map[string]bool{
					jobsTableID + "\x00" + string(schema.DependencyContains):              true,
					lifecycleStateColumnID + "\x00" + string(schema.DependencyReferences): true,
				}
				for functionID := range lifecycleFunctions {
					expected[functionID+"\x00"+string(schema.DependencyReferences)] = true
				}
				actual := map[string]bool{}
				for _, dependency := range resource.Dependencies {
					actual[dependency.Target+"\x00"+string(dependency.Type)] = true
				}
				if len(lifecycleFunctions) == 1 && jobsTableID != "" && lifecycleStateColumnID != "" && len(resource.Dependencies) == len(expected) && maps.Equal(actual, expected) {
					sentinels["generated_exact_dependency_set"] = true
				}
				functionReferences := 0
				for _, dependency := range resource.Dependencies {
					if dependency.Type == schema.DependencyReferences && resourceKinds[dependency.Target] == schema.KindFunction {
						functionReferences++
					}
				}
				if functionReferences == 1 {
					sentinels["generated_single_function_reference"] = true
				}
			}
		case schema.KindFunction, schema.KindProcedure:
			definition := fmt.Sprint(values["definition"])
			if fmt.Sprint(values["body_digest"]) != "" && (strings.Contains(definition, "skip locked") || strings.Contains(definition, "audit events are immutable") || strings.Contains(definition, "case value") || strings.Contains(definition, "delete from")) {
				sentinels["real_routine_bodies"] = true
			}
		}
	}
	if comments != semanticCellCommentedObjects {
		t.Fatalf("semantic comments=%d want=%d", comments, semanticCellCommentedObjects)
	}
	for name := range semanticCellRoles {
		if !roles[name] {
			t.Errorf("semantic role %s missing", name)
		}
	}
	for _, name := range []string{"hstore", "pgcrypto"} {
		if !extensions[name] {
			t.Errorf("semantic extension %s missing", name)
		}
	}
	for _, name := range []string{"dbos_arithmetic", "parameterized_type", "enum_default", "json_empty_object", "json_empty_array", "array_default", "uuid_generated", "uuid_fixed_cast", "time_default", "cast_string_default", "generated_stored", "lifecycle_function_body", "generated_exact_dependency_set", "generated_single_function_reference", "real_routine_bodies"} {
		if !sentinels[name] {
			for _, resource := range document.Graph.Resources {
				if resource.Kind == schema.KindColumn && (resource.Name.Name == "dbos_updated_at" || resource.Name.Name == "state_v2" || resource.Name.Name == "payload" || resource.Name.Name == "headers" || resource.Name.Name == "id" || resource.Name.Name == "worker_id") {
					t.Logf("missing sentinel %s column=%s spec=%s deps=%+v", name, resource.Name.String(), resource.Spec, resource.Dependencies)
				}
			}
			t.Errorf("semantic sentinel %s missing", name)
		}
	}
	for kind, minimum := range map[schema.Kind]int{schema.KindTable: 16, schema.KindPrimaryKey: 16, schema.KindForeignKey: 15, schema.KindCheckConstraint: 10, schema.KindUniqueConstraint: 10, schema.KindIndex: 6, schema.KindFunction: 4, schema.KindProcedure: 1, schema.KindTrigger: 3, schema.KindPolicy: 4, schema.KindGrant: 3, schema.KindMembership: 1, schema.KindDefaultPrivilege: 1} {
		if kinds[kind] < minimum {
			t.Errorf("semantic kind %s=%d want >=%d", kind, kinds[kind], minimum)
		}
	}
	owned := false
	for _, resource := range document.Graph.Resources {
		if resource.Name.Schema != namespace {
			continue
		}
		values := map[string]any{}
		_ = json.Unmarshal(resource.Spec, &values)
		if values["owner"] == "autosql_semantic_owner" {
			owned = true
			break
		}
	}
	if !owned {
		t.Error("semantic ownership handoff is missing")
	}
}

func assertSemanticAuthorization(t testing.TB, inventory postgres.BootstrapAuthorizationInventory) {
	t.Helper()
	if len(inventory.Routines) < 5 || len(inventory.Extensions) != 2 {
		t.Fatalf("semantic authorization routines=%d extensions=%d", len(inventory.Routines), len(inventory.Extensions))
	}
	for _, routine := range inventory.Routines {
		if !routine.DigestReviewRequired || routine.SourceDigest == "" || routine.ResourceID == "" || routine.Signature == "" {
			t.Fatalf("semantic routine authorization incomplete: %+v", routine)
		}
	}
}

func assertSemanticPlan(t testing.TB, whole bootstrap.Plan) {
	t.Helper()
	if err := whole.Validate(); err != nil {
		t.Fatal(err)
	}
	stages := map[bootstrap.ExecutionStage]bool{}
	for _, step := range whole.Steps {
		stages[step.Stage] = true
	}
	for _, stage := range []bootstrap.ExecutionStage{bootstrap.StageDatabaseTarget, bootstrap.StageRoles, bootstrap.StageNamespaces, bootstrap.StageExtensions, bootstrap.StageRoutines, bootstrap.StageStorage, bootstrap.StageConstraints, bootstrap.StageIndexes, bootstrap.StageBehavior, bootstrap.StageAccess} {
		if !stages[stage] {
			t.Errorf("semantic plan missing stage %s", stage)
		}
	}
}

func cleanupSemanticCell(t testing.TB, ctx context.Context, conn *pgx.Conn, namespace string) {
	t.Helper()
	_, err := conn.Exec(ctx, fmt.Sprintf(`reset role; drop extension if exists hstore cascade; drop extension if exists pgcrypto cascade; drop schema if exists %s cascade`, namespace))
	if err != nil {
		t.Fatal(err)
	}
	for _, role := range []string{"autosql_semantic_app", "autosql_semantic_reader", "autosql_semantic_owner"} {
		var exists bool
		if err := conn.QueryRow(ctx, `select exists(select 1 from pg_roles where rolname=$1)`, role).Scan(&exists); err != nil {
			t.Fatal(err)
		}
		if exists {
			if _, err := conn.Exec(ctx, "drop owned by "+pgx.Identifier{role}.Sanitize()+" cascade; drop role "+pgx.Identifier{role}.Sanitize()); err != nil {
				t.Fatal(err)
			}
		}
	}
}

func TestSemanticCellSignedDirectBootstrapInterruptionResume(t *testing.T) {
	rawURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if rawURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Minute)
	defer cancel()
	const namespace = "autosql_semantic_direct"
	desired, config, owner, major, cleanup := semanticCellFixture(t, ctx, rawURL, namespace)
	defer cleanup()
	target := completeCellExecutionTarget(config, owner, "autosql_semantic_direct")
	defer postgres.DropDatabaseURL(context.Background(), rawURL, target.Name, true)
	render := map[string]string{"postgres_version": fmt.Sprint(major), "concurrent_indexes": "true"}
	inventory, err := postgres.PrepareBootstrapAuthorizationInventory(ctx, target, desired, postgres.BootstrapAuthorizationInventoryOptions{Render: render})
	if err != nil {
		t.Fatal(err)
	}
	assertSemanticAuthorization(t, inventory)
	authorization := signCompleteBootstrapAuthorization(t, inventory)
	whole, err := postgres.PlanDatabaseBootstrapAuthorized(ctx, target, desired, plan.Options{Render: render}, authorization.Verified)
	if err != nil {
		t.Fatal(err)
	}
	assertSemanticPlan(t, whole)
	interrupted := map[string]bool{}
	var result postgres.BootstrapExecutionResult
	for attempt := 0; attempt <= len(whole.Phases); attempt++ {
		result, err = postgres.ExecuteDatabaseBootstrapURL(ctx, rawURL, whole, postgres.BootstrapExecutionHooks{BeforePhase: func(_ context.Context, phase bootstrap.BootstrapPhase) error {
			if !interrupted[phase.ID] {
				interrupted[phase.ID] = true
				return fmt.Errorf("semantic interruption detail")
			}
			return nil
		}})
		if err == nil {
			break
		}
		if strings.Contains(err.Error(), "semantic interruption detail") {
			t.Fatalf("executor leaked interruption detail: %v", err)
		}
	}
	if err != nil || !result.Completed || !result.Resumed {
		if err != nil {
			message := err.Error()
			if start := strings.Index(message, "first "); start >= 0 {
				fragment := message[start+len("first "):]
				if bracket := strings.Index(fragment, "["); bracket > 0 {
					id := fragment[:bracket]
					for _, resource := range desired.Graph.Resources {
						if resource.ID == id {
							t.Logf("first postcondition mismatch resource=%s %s spec=%s", resource.Kind, resource.Name.String(), resource.Spec)
						}
					}
					targetConfig := config.Copy()
					targetConfig.Database = target.Name
					if diagnosticConn, connectErr := pgx.ConnectConfig(ctx, targetConfig); connectErr == nil {
						if diagnostic, inspectErr := postgres.InspectConn(ctx, diagnosticConn, postgres.Options{Schemas: []string{namespace}, Advanced: true}); inspectErr == nil {
							for _, resource := range diagnostic.Graph.Resources {
								if resource.ID == id {
									t.Logf("actual postcondition resource=%s %s spec=%s", resource.Kind, resource.Name.String(), resource.Spec)
								}
							}
						}
						diagnosticConn.Close(context.Background())
					}
				}
			}
		}
		t.Fatalf("semantic direct bootstrap=%+v err=%v", result, err)
	}
	assertSemanticTargetZeroChange(t, ctx, config, target, desired, namespace, major, "direct")
}

func assertSemanticTargetZeroChange(t *testing.T, ctx context.Context, config *pgx.ConnConfig, target bootstrap.DatabaseTarget, desired schema.Document, namespace string, major int, path string) {
	t.Helper()
	targetConfig := config.Copy()
	targetConfig.Database = target.Name
	conn, err := pgx.ConnectConfig(ctx, targetConfig)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := postgres.InspectConn(ctx, conn, postgres.Options{Schemas: []string{namespace}, Advanced: true})
	conn.Close(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	actual = filterSemanticCell(t, actual, namespace)
	assertSemanticCell(t, actual, namespace)
	assertFingerprint(t, actual, desired)
	render := map[string]string{"postgres_version": fmt.Sprint(major), "concurrent_indexes": "true"}
	next, err := plan.Build(ctx, postgres.New(), actual, desired, plan.Options{Render: render})
	if err != nil || len(next.Changes.Changes) != 0 || len(next.Steps) != 0 {
		t.Fatalf("%s semantic next changes=%d steps=%d err=%v", path, len(next.Changes.Changes), len(next.Steps), err)
	}
	adopt, err := plan.Build(ctx, postgres.New(), actual, actual, plan.Options{Render: render})
	if err != nil || len(adopt.Changes.Changes) != 0 || len(adopt.Steps) != 0 {
		t.Fatalf("%s semantic adopt changes=%d steps=%d err=%v", path, len(adopt.Changes.Changes), len(adopt.Steps), err)
	}
	t.Logf("semantic_cell_%s resources=%d comments=%d next=0/0 adopt=0/0", path, len(actual.Graph.Resources), semanticCellCommentedObjects)
}
