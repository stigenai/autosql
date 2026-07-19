package postgres

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"autosql/pkg/artifact"
	"autosql/pkg/bootstrap"
	"autosql/pkg/plan"
	"autosql/pkg/schema"
	"autosql/pkg/source"

	"github.com/jackc/pgx/v5"
)

func TestWholeDatabaseBootstrapExecutionResumeAndReconcile(t *testing.T) {
	maintenanceURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if maintenanceURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	target := bootstrapExecutionTarget(t, ctx, maintenanceURL, "autosql_bootstrap_exec")
	defer DropDatabaseURL(context.Background(), maintenanceURL, target.Name, true)
	_ = DropDatabaseURL(ctx, maintenanceURL, target.Name, true)

	desired := bootstrapExecutionDocument(t, "bootstrap_app")
	whole, err := PlanDatabaseBootstrap(ctx, target, desired, plan.Options{Render: map[string]string{"concurrent_indexes": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	failedOnline := false
	first, err := ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, whole, BootstrapExecutionHooks{AfterStep: func(_ context.Context, step bootstrap.BootstrapStep) error {
		if step.Stage == bootstrap.StageIndexes && !failedOnline {
			failedOnline = true
			return errors.New("seeded-secret-routine-body")
		}
		return nil
	}})
	if !errors.Is(err, ErrBootstrapReconcile) || first.PendingStep == "" || strings.Contains(err.Error(), "seeded-secret") {
		t.Fatalf("online interruption err=%v result=%+v", err, first)
	}
	raw, _ := json.Marshal(first)
	if strings.Contains(string(raw), "seeded-secret") || strings.Contains(string(raw), "CREATE INDEX") {
		t.Fatalf("execution output leaked SQL or hook details: %s", raw)
	}
	diagnosis, err := DiagnoseDatabaseBootstrapURL(ctx, maintenanceURL, whole)
	if err != nil || diagnosis.PendingStep != first.PendingStep || diagnosis.RecoveryGuidance == "" {
		t.Fatalf("diagnosis=%+v err=%v", diagnosis, err)
	}
	resumed, err := ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, whole, BootstrapExecutionHooks{})
	if err != nil || !resumed.Resumed || !resumed.Completed {
		t.Fatalf("resume=%+v err=%v", resumed, err)
	}
	noOp, err := ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, whole, BootstrapExecutionHooks{})
	if err != nil || !noOp.Resumed || !noOp.Completed || noOp.AppliedSteps != 0 {
		t.Fatalf("completed rerun=%+v err=%v", noOp, err)
	}
	config, _ := pgx.ParseConfig(maintenanceURL)
	config.Database = target.Name
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	inspected, err := InspectConn(ctx, conn, Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range inspected.Graph.Resources {
		if resource.Name.Schema == "autosql_internal" || resource.Kind == schema.KindSchema && resource.Name.Name == "autosql_internal" {
			t.Fatalf("bootstrap ledger escaped into managed graph: %+v", resource)
		}
	}
	current, err := New().Normalize(ctx, inspected)
	if err != nil {
		t.Fatal(err)
	}
	updated := current
	updated.Graph.Resources = append([]schema.Resource(nil), current.Graph.Resources...)
	var table, id schema.Resource
	for _, resource := range updated.Graph.Resources {
		if resource.Kind == schema.KindTable && resource.Name.Schema == "bootstrap_app" && resource.Name.Name == "items" {
			table = resource
		}
	}
	for _, resource := range updated.Graph.Resources {
		if resource.Kind == schema.KindColumn && resource.Name.Parent == table.ID && resource.Name.Name == "id" {
			id = resource
		}
	}
	maintenanceIndex := renderResource(schema.KindIndex, schema.Name{Schema: "bootstrap_app", Name: "items_id_maintenance_idx", Parent: table.ID}, `{"definition":"CREATE INDEX items_id_maintenance_idx ON bootstrap_app.items USING btree (id)","method":"btree","unique":false,"valid":true,"ready":true,"columns":["id"]}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: id.ID, Type: schema.DependencyReferences})
	updated.Graph.Resources = append(updated.Graph.Resources, maintenanceIndex)
	updated, err = New().Normalize(ctx, updated)
	if err != nil {
		t.Fatal(err)
	}
	maintenancePlan, err := PlanDatabaseTransition(ctx, target, current, updated, plan.Options{Render: map[string]string{"concurrent_indexes": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	interruptedMaintenance := false
	maintenanceResult, err := ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, maintenancePlan, BootstrapExecutionHooks{AfterStep: func(_ context.Context, step bootstrap.BootstrapStep) error {
		if step.Stage == bootstrap.StageIndexes && !interruptedMaintenance {
			interruptedMaintenance = true
			return errors.New("injected maintenance interruption")
		}
		return nil
	}})
	if !errors.Is(err, ErrBootstrapReconcile) || maintenanceResult.PendingStep == "" {
		t.Fatalf("maintenance interruption result=%+v err=%v", maintenanceResult, err)
	}
	if _, err := conn.Exec(ctx, `drop index bootstrap_app.items_id_maintenance_idx`); err != nil {
		t.Fatal(err)
	}
	if _, err := conn.Exec(ctx, `create index items_id_maintenance_idx on bootstrap_app.items(value)`); err != nil {
		t.Fatal(err)
	}
	if mismatched, mismatchErr := ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, maintenancePlan, BootstrapExecutionHooks{}); !errors.Is(mismatchErr, ErrBootstrapReconcile) || mismatched.PendingStep != maintenanceResult.PendingStep {
		t.Fatalf("mismatched concurrent remnant result=%+v err=%v", mismatched, mismatchErr)
	}
	if _, err := conn.Exec(ctx, `drop index bootstrap_app.items_id_maintenance_idx`); err != nil {
		t.Fatal(err)
	}
	maintenanceResult, err = ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, maintenancePlan, BootstrapExecutionHooks{})
	if err != nil || !maintenanceResult.Completed || !maintenanceResult.Resumed {
		t.Fatalf("maintenance resume=%+v err=%v", maintenanceResult, err)
	}
}

func TestManagedBootstrapExecutesIntoIntrinsicPublicSchema(t *testing.T) {
	maintenanceURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if maintenanceURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	target := bootstrapExecutionTarget(t, ctx, maintenanceURL, "autosql_bootstrap_public")
	defer DropDatabaseURL(context.Background(), maintenanceURL, target.Name, true)
	_ = DropDatabaseURL(ctx, maintenanceURL, target.Name, true)

	desired := bootstrapExecutionDocument(t, "public")
	whole, err := PlanDatabaseBootstrap(ctx, target, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range whole.SchemaPlan.Steps {
		if strings.Contains(step.SQL, `CREATE SCHEMA "public"`) {
			t.Fatal("plan attempts to recreate PostgreSQL's intrinsic public schema")
		}
	}
	result, err := ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, whole, BootstrapExecutionHooks{})
	if err != nil || !result.Completed {
		t.Fatalf("public-schema bootstrap result=%+v err=%v", result, err)
	}

	config, err := pgx.ParseConfig(maintenanceURL)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = target.Name
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var exists bool
	if err := conn.QueryRow(ctx, `select to_regclass('public.items') is not null`).Scan(&exists); err != nil || !exists {
		t.Fatalf("public.items exists=%t err=%v", exists, err)
	}
}

func TestManagedBootstrapPreservesRoutineLineComments(t *testing.T) {
	maintenanceURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if maintenanceURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	target := bootstrapExecutionTarget(t, ctx, maintenanceURL, "autosql_bootstrap_routine_lines")
	defer DropDatabaseURL(context.Background(), maintenanceURL, target.Name, true)
	_ = DropDatabaseURL(ctx, maintenanceURL, target.Name, true)

	namespace := renderResource(schema.KindSchema, schema.Name{Name: "global"}, `{}`)
	definition := "CREATE OR REPLACE FUNCTION global.record_assignment_history(value integer)\n" +
		" RETURNS integer\n" +
		" LANGUAGE plpgsql\n" +
		"AS $function$\n" +
		"BEGIN\n" +
		"  -- On UPDATE, preserve the previous assignment.\n" +
		"  RETURN value + 1;\n" +
		"END;\n" +
		"$function$"
	routineSpec, err := json.Marshal(map[string]any{
		"name": "record_assignment_history", "identity_arguments": "value integer", "arguments": "value integer",
		"result": "integer", "returns_set": false, "language": "plpgsql", "volatility": "v", "strict": false,
		"security_definer": false, "leakproof": false, "parallel": "u", "cost": 100.0, "rows": 0.0,
		"configuration": []string{}, "owner": target.Owner, "definition": definition,
	})
	if err != nil {
		t.Fatal(err)
	}
	routine := renderResource(schema.KindFunction, schema.Name{Schema: "global", Name: "record_assignment_history(value integer)", Parent: namespace.ID}, string(routineSpec), schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	desired, err := New().Normalize(ctx, schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{namespace, routine}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range desired.Graph.Resources {
		if resource.Kind == schema.KindFunction {
			routine = resource
		}
	}
	digest := stringValue(spec(routine), "body_digest")
	whole, err := PlanDatabaseBootstrap(ctx, target, desired, plan.Options{Render: map[string]string{"reviewed_routine_digests": digest}})
	if err != nil {
		t.Fatal(err)
	}
	foundRoutine := false
	for _, step := range whole.SchemaPlan.Steps {
		if !strings.Contains(step.SQL, "CREATE OR REPLACE FUNCTION") || !strings.Contains(step.SQL, "record_assignment_history") {
			continue
		}
		foundRoutine = true
		if !strings.Contains(step.SQL, "$function$\nBEGIN\n  -- On UPDATE") || strings.Contains(step.SQL, "BEGIN -- On UPDATE") {
			t.Fatalf("bootstrap plan changed routine line semantics:\n%s", step.SQL)
		}
	}
	if !foundRoutine {
		t.Fatal("bootstrap plan omitted reviewed routine")
	}
	result, err := ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, whole, BootstrapExecutionHooks{})
	if err != nil || !result.Completed {
		t.Fatalf("routine bootstrap result=%+v err=%v", result, err)
	}

	config, err := pgx.ParseConfig(maintenanceURL)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = target.Name
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var got int
	if err := conn.QueryRow(ctx, `select global.record_assignment_history(41)`).Scan(&got); err != nil || got != 42 {
		t.Fatalf("executed commented routine result=%d err=%v", got, err)
	}
	var installedSource string
	if err := conn.QueryRow(ctx, `select pg_get_functiondef('global.record_assignment_history(integer)'::regprocedure)`).Scan(&installedSource); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(installedSource, "$function$\nBEGIN\n  -- On UPDATE") {
		t.Fatalf("installed routine lost source lines:\n%s", installedSource)
	}
	inspected, err := InspectConn(ctx, conn, Options{Schemas: []string{"global"}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := New().Normalize(ctx, inspected)
	if err != nil {
		t.Fatal(err)
	}
	noOp, err := plan.Build(ctx, New(), current, desired, plan.Options{Render: map[string]string{"reviewed_routine_digests": digest}})
	if err != nil {
		t.Fatal(err)
	}
	if len(noOp.Changes.Changes) != 0 || len(noOp.Steps) != 0 {
		t.Fatalf("reinspected routine did not converge: changes=%+v steps=%+v", noOp.Changes.Changes, noOp.Steps)
	}
}

func TestManagedBootstrapExecutesEnumDefaultOutsideSearchPath(t *testing.T) {
	maintenanceURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if maintenanceURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	target := bootstrapExecutionTarget(t, ctx, maintenanceURL, "autosql_bootstrap_enum_default")
	defer DropDatabaseURL(context.Background(), maintenanceURL, target.Name, true)
	_ = DropDatabaseURL(ctx, maintenanceURL, target.Name, true)

	namespace := renderResource(schema.KindSchema, schema.Name{Name: "global"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "global", Name: "cells", Parent: namespace.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	statusType := renderResource(schema.KindEnum, schema.Name{Schema: "global", Name: "cell_status", Parent: namespace.ID}, `{"values":["provisioning","active","draining","offline"]}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	id := renderResource(schema.KindColumn, schema.Name{Schema: "global", Name: "id", Parent: table.ID}, `{"type":"bigint","not_null":true,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	status := renderResource(schema.KindColumn, schema.Name{Schema: "global", Name: "status", Parent: table.ID}, `{"type":"global.cell_status","default":"'provisioning'::cell_status","not_null":true,"ordinal":2}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: statusType.ID, Type: schema.DependencyUses})
	desired, err := New().Normalize(ctx, schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{status, id, table, statusType, namespace}}})
	if err != nil {
		t.Fatal(err)
	}
	whole, err := PlanDatabaseBootstrap(ctx, target, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	typePosition, columnPosition := -1, -1
	for position, step := range whole.SchemaPlan.Steps {
		if strings.Contains(step.SQL, `CREATE TYPE "global"."cell_status"`) {
			typePosition = position
		}
		if strings.Contains(step.SQL, `ADD COLUMN "status"`) {
			columnPosition = position
			if !strings.Contains(step.SQL, `'provisioning'::global.cell_status`) && !strings.Contains(step.SQL, `'provisioning'::"global"."cell_status"`) {
				t.Fatalf("enum default is not schema-bound: %s", step.SQL)
			}
		}
	}
	if typePosition < 0 || columnPosition < 0 || typePosition >= columnPosition {
		t.Fatalf("enum type position=%d column position=%d", typePosition, columnPosition)
	}
	result, err := ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, whole, BootstrapExecutionHooks{})
	if err != nil || !result.Completed {
		t.Fatalf("enum bootstrap result=%+v err=%v", result, err)
	}

	config, err := pgx.ParseConfig(maintenanceURL)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = target.Name
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var got string
	if err := conn.QueryRow(ctx, `insert into global.cells(id) values (1) returning status::text`).Scan(&got); err != nil || got != "provisioning" {
		t.Fatalf("schema-bound enum default=%q err=%v", got, err)
	}
	inspected, err := InspectConn(ctx, conn, Options{Schemas: []string{"global"}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := New().Normalize(ctx, inspected)
	if err != nil {
		t.Fatal(err)
	}
	noOp, err := plan.Build(ctx, New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(noOp.Changes.Changes) != 0 || len(noOp.Steps) != 0 {
		t.Fatalf("reinspected enum bootstrap did not converge: changes=%+v steps=%+v", noOp.Changes.Changes, noOp.Steps)
	}
}

func TestManagedBootstrapExecutesSchemaBoundIndexPredicateOutsideSearchPath(t *testing.T) {
	maintenanceURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if maintenanceURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	namespace := renderResource(schema.KindSchema, schema.Name{Name: "global"}, `{}`)
	cellType := renderResource(schema.KindEnum, schema.Name{Schema: "global", Name: "cell_type", Parent: namespace.ID}, `{"values":["shared","dedicated"]}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	cellStatus := renderResource(schema.KindEnum, schema.Name{Schema: "global", Name: "cell_status", Parent: namespace.ID}, `{"values":["active","inactive"]}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	table := renderResource(schema.KindTable, schema.Name{Schema: "global", Name: "cells", Parent: namespace.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	cellTypeColumn := renderResource(schema.KindColumn, schema.Name{Schema: "global", Name: "type", Parent: table.ID}, `{"type":"global.cell_type","not_null":true,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: cellType.ID, Type: schema.DependencyUses})
	statusColumn := renderResource(schema.KindColumn, schema.Name{Schema: "global", Name: "status", Parent: table.ID}, `{"type":"global.cell_status","not_null":true,"ordinal":2}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: cellStatus.ID, Type: schema.DependencyUses})
	index := renderResource(schema.KindIndex, schema.Name{Schema: "global", Name: "idx_cells_available_shared", Parent: table.ID}, `{"definition":"CREATE INDEX idx_cells_available_shared ON global.cells USING btree (type) WHERE ((type = 'shared'::cell_type) AND (status = 'active'::cell_status))","method":"btree","unique":false,"valid":true,"ready":true,"columns":["type"]}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: cellTypeColumn.ID, Type: schema.DependencyReferences})
	desired, err := New().Normalize(ctx, schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{index, statusColumn, cellTypeColumn, table, cellStatus, cellType, namespace}}})
	if err != nil {
		t.Fatal(err)
	}

	for _, concurrent := range []bool{false, true} {
		t.Run(fmt.Sprintf("concurrent_%v", concurrent), func(t *testing.T) {
			target := bootstrapExecutionTarget(t, ctx, maintenanceURL, fmt.Sprintf("autosql_bootstrap_index_cast_%v", concurrent))
			defer DropDatabaseURL(context.Background(), maintenanceURL, target.Name, true)
			_ = DropDatabaseURL(ctx, maintenanceURL, target.Name, true)
			render := map[string]string{}
			if concurrent {
				render["concurrent_indexes"] = "true"
			}
			whole, planErr := PlanDatabaseBootstrap(ctx, target, desired, plan.Options{Render: render})
			if planErr != nil {
				t.Fatal(planErr)
			}
			result, executeErr := ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, whole, BootstrapExecutionHooks{})
			if executeErr != nil || !result.Completed {
				t.Fatalf("schema-bound index bootstrap result=%+v err=%v", result, executeErr)
			}
			config, parseErr := pgx.ParseConfig(maintenanceURL)
			if parseErr != nil {
				t.Fatal(parseErr)
			}
			config.Database = target.Name
			conn, connectErr := pgx.ConnectConfig(ctx, config)
			if connectErr != nil {
				t.Fatal(connectErr)
			}
			defer conn.Close(context.Background())
			inspected, inspectErr := InspectConn(ctx, conn, Options{Schemas: []string{"global"}})
			if inspectErr != nil {
				t.Fatal(inspectErr)
			}
			current, normalizeErr := New().Normalize(ctx, inspected)
			if normalizeErr != nil {
				t.Fatal(normalizeErr)
			}
			noOp, buildErr := plan.Build(ctx, New(), current, desired, plan.Options{Render: render})
			if buildErr != nil {
				t.Fatal(buildErr)
			}
			if len(noOp.Changes.Changes) != 0 || len(noOp.Steps) != 0 {
				t.Fatalf("reinspected index bootstrap did not converge: changes=%+v steps=%+v", noOp.Changes.Changes, noOp.Steps)
			}
		})
	}
}

func TestManagedBootstrapExecutesSchemaBoundForeignKeyOutsideSearchPath(t *testing.T) {
	maintenanceURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if maintenanceURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	target := bootstrapExecutionTarget(t, ctx, maintenanceURL, "autosql_bootstrap_fk_reference")
	defer DropDatabaseURL(context.Background(), maintenanceURL, target.Name, true)
	_ = DropDatabaseURL(ctx, maintenanceURL, target.Name, true)

	namespace := renderResource(schema.KindSchema, schema.Name{Name: "global"}, `{}`)
	channels := renderResource(schema.KindTable, schema.Name{Schema: "global", Name: "channels", Parent: namespace.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	channelID := renderResource(schema.KindColumn, schema.Name{Schema: "global", Name: "id", Parent: channels.ID}, `{"type":"bigint","not_null":true,"ordinal":1}`, schema.Dependency{Target: channels.ID, Type: schema.DependencyContains})
	channelsPK := renderResource(schema.KindPrimaryKey, schema.Name{Schema: "global", Name: "channels_pkey", Parent: channels.ID}, `{"definition":"PRIMARY KEY (id)","columns":["id"],"deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: channels.ID, Type: schema.DependencyContains}, schema.Dependency{Target: channelID.ID, Type: schema.DependencyReferences})
	subscriptions := renderResource(schema.KindTable, schema.Name{Schema: "global", Name: "instance_subscriptions", Parent: namespace.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	subscriptionChannelID := renderResource(schema.KindColumn, schema.Name{Schema: "global", Name: "channel_id", Parent: subscriptions.ID}, `{"type":"bigint","not_null":true,"ordinal":1}`, schema.Dependency{Target: subscriptions.ID, Type: schema.DependencyContains})
	foreignKey := renderResource(schema.KindForeignKey, schema.Name{Schema: "global", Name: "instance_subscriptions_channel_id_fkey", Parent: subscriptions.ID}, `{"definition":"FOREIGN KEY (channel_id) REFERENCES channels(id)","columns":["channel_id"],"referenced_columns":["id"],"deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: subscriptions.ID, Type: schema.DependencyContains}, schema.Dependency{Target: subscriptionChannelID.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: channels.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: channelID.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: channelsPK.ID, Type: schema.DependencyReferences})
	desired, err := New().Normalize(ctx, schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{foreignKey, subscriptionChannelID, subscriptions, channelsPK, channelID, channels, namespace}}})
	if err != nil {
		t.Fatal(err)
	}
	hcl, err := source.FormatHCL(desired)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = source.LoadContext(ctx, source.Input{URI: "schema-bound-fk.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	for _, resource := range desired.Graph.Resources {
		if resource.ID == foreignKey.ID && !strings.Contains(stringValue(spec(resource), "definition"), "REFERENCES global.channels") {
			t.Fatalf("foreign key target is not schema-bound: %s", stringValue(spec(resource), "definition"))
		}
	}
	whole, err := PlanDatabaseBootstrap(ctx, target, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, whole, BootstrapExecutionHooks{})
	if err != nil || !result.Completed {
		t.Fatalf("schema-bound foreign key bootstrap result=%+v err=%v", result, err)
	}
	config, err := pgx.ParseConfig(maintenanceURL)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = target.Name
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, `insert into global.channels(id) values(1); insert into global.instance_subscriptions(channel_id) values(1)`); err != nil {
		t.Fatal(err)
	}
	inspected, err := InspectConn(ctx, conn, Options{Schemas: []string{"global"}})
	if err != nil {
		t.Fatal(err)
	}
	current, err := New().Normalize(ctx, inspected)
	if err != nil {
		t.Fatal(err)
	}
	noOp, err := plan.Build(ctx, New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(noOp.Changes.Changes) != 0 || len(noOp.Steps) != 0 {
		t.Fatalf("reinspected foreign key bootstrap did not converge: changes=%+v steps=%+v", noOp.Changes.Changes, noOp.Steps)
	}
}

func TestWholeDatabaseBootstrapTransactionalFailureRollsBackAndIdentityIsBound(t *testing.T) {
	maintenanceURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if maintenanceURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	target := bootstrapExecutionTarget(t, ctx, maintenanceURL, "autosql_bootstrap_tx")
	defer DropDatabaseURL(context.Background(), maintenanceURL, target.Name, true)
	_ = DropDatabaseURL(ctx, maintenanceURL, target.Name, true)
	desired := bootstrapExecutionDocument(t, "tx_app")
	whole, err := PlanDatabaseBootstrap(ctx, target, desired, plan.Options{Render: map[string]string{"concurrent_indexes": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	failed := false
	_, err = ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, whole, BootstrapExecutionHooks{AfterStep: func(_ context.Context, step bootstrap.BootstrapStep) error {
		if step.Transaction == plan.TransactionRequired && step.Stage == bootstrap.StageNamespaces && !failed {
			failed = true
			return errors.New("injected")
		}
		return nil
	}})
	if err == nil || errors.Is(err, ErrBootstrapReconcile) {
		t.Fatalf("transaction interruption err=%v", err)
	}
	config, _ := pgx.ParseConfig(maintenanceURL)
	config.Database = target.Name
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	var schemaExists bool
	if err := conn.QueryRow(ctx, `select exists(select 1 from pg_namespace where nspname='tx_app')`).Scan(&schemaExists); err != nil {
		t.Fatal(err)
	}
	conn.Close(ctx)
	if schemaExists {
		t.Fatal("transactional schema survived injected phase rollback")
	}
	if result, err := ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, whole, BootstrapExecutionHooks{}); err != nil || !result.Completed || !result.Resumed {
		t.Fatalf("transaction resume=%+v err=%v", result, err)
	}

	changed := desired
	changed.Graph.Resources = append([]schema.Resource(nil), desired.Graph.Resources...)
	for index := range changed.Graph.Resources {
		if changed.Graph.Resources[index].Kind == schema.KindTable {
			changed.Graph.Resources[index].Annotations = map[string]string{"comment": "different plan"}
		}
	}
	changed, err = New().Normalize(ctx, changed)
	if err != nil {
		t.Fatal(err)
	}
	other, err := PlanDatabaseBootstrap(ctx, target, changed, plan.Options{Render: map[string]string{"concurrent_indexes": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, other, BootstrapExecutionHooks{}); !errors.Is(err, ErrBootstrapIdentity) {
		t.Fatalf("identity mismatch err=%v", err)
	}
}

func TestWholeDatabaseBootstrapRejectsUntrackedManagedCollision(t *testing.T) {
	maintenanceURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if maintenanceURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	target := bootstrapExecutionTarget(t, ctx, maintenanceURL, "autosql_bootstrap_collision")
	defer DropDatabaseURL(context.Background(), maintenanceURL, target.Name, true)
	_ = DropDatabaseURL(ctx, maintenanceURL, target.Name, true)
	maintenance, err := pgx.Connect(ctx, maintenanceURL)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.Exec(ctx, "create database "+pgx.Identifier{target.Name}.Sanitize()); err != nil {
		t.Fatal(err)
	}
	maintenance.Close(ctx)
	whole, err := PlanDatabaseBootstrap(ctx, target, bootstrapExecutionDocument(t, "collision_app"), plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, whole, BootstrapExecutionHooks{})
	if !errors.Is(err, ErrBootstrapCollision) || result.RecoveryGuidance == "" {
		t.Fatalf("collision result=%+v err=%v", result, err)
	}
}

func TestWholeDatabaseBootstrapExtensionReadinessFailsBeforeTargetMutation(t *testing.T) {
	maintenanceURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if maintenanceURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	target := bootstrapExecutionTarget(t, ctx, maintenanceURL, "autosql_extension_preflight")
	defer DropDatabaseURL(context.Background(), maintenanceURL, target.Name, true)
	_ = DropDatabaseURL(ctx, maintenanceURL, target.Name, true)
	ns := renderResource(schema.KindSchema, schema.Name{Name: "extension_preflight_app"}, `{}`)
	extension := renderResource(schema.KindExtension, schema.Name{Schema: ns.Name.Name, Name: "autosql_missing_control_file", Parent: ns.ID}, `{"version":"1.0","relocatable":true,"trusted":true,"superuser":true,"requires":[]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, extension}}}
	whole, err := PlanDatabaseBootstrap(ctx, target, desired, plan.Options{Render: map[string]string{
		"extension_allowlist":                      extension.Name.Name,
		"extension_version." + extension.Name.Name: "1.0",
		"extension_schemas." + extension.Name.Name: ns.Name.Name,
	}})
	if err != nil {
		t.Fatal(err)
	}
	result, err := ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, whole, BootstrapExecutionHooks{})
	if err == nil || result.CreatedDatabase || !strings.Contains(err.Error(), "missing_package_control_file") || !strings.Contains(err.Error(), ".control") {
		t.Fatalf("result=%+v err=%v", result, err)
	}
	conn, connectErr := pgx.Connect(ctx, maintenanceURL)
	if connectErr != nil {
		t.Fatal(connectErr)
	}
	defer conn.Close(ctx)
	var exists bool
	if err := conn.QueryRow(ctx, `select exists(select 1 from pg_database where datname=$1)`, target.Name).Scan(&exists); err != nil || exists {
		t.Fatalf("target exists=%v err=%v; readiness ran after mutation", exists, err)
	}
}

func TestBootstrapExtensionReadinessUsesServerTrustAndExplicitAuthority(t *testing.T) {
	maintenanceURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if maintenanceURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	config, err := pgx.ParseConfig(maintenanceURL)
	if err != nil {
		t.Fatal(err)
	}
	catalog, err := InspectExtensionCatalogURL(ctx, maintenanceURL)
	if err != nil {
		t.Fatal(err)
	}
	version, secondVersion := "", ""
	for _, available := range catalog.Versions {
		if available.Name == "dblink" && !available.Trusted {
			version = available.Version
		}
		if available.Name == "amcheck" && !available.Trusted {
			secondVersion = available.Version
		}
	}
	if version == "" || secondVersion == "" {
		t.Skip("server does not expose both untrusted dblink and amcheck control files")
	}
	var owner string
	conn, err := pgx.Connect(ctx, maintenanceURL)
	if err != nil {
		t.Fatal(err)
	}
	if err := conn.QueryRow(ctx, `select pg_get_userbyid(datdba) from pg_database where datname=current_database()`).Scan(&owner); err != nil {
		conn.Close(ctx)
		t.Fatal(err)
	}
	conn.Close(ctx)
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ExternalDatabase, Endpoint: bootstrap.ServerEndpoint{Host: config.Host, Port: uint16(config.Port), TLSMode: "disable"}, MaintenanceDatabase: config.Database, Name: config.Database, Owner: owner, ConnectionLimit: -1}.Normalize()
	ns := renderResource(schema.KindSchema, schema.Name{Name: "public"}, `{}`)
	extension := renderResource(schema.KindExtension, schema.Name{Schema: "public", Name: "dblink", Parent: ns.ID}, fmt.Sprintf(`{"version":%q,"relocatable":true,"trusted":true,"superuser":true,"requires":[]}`, version), schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, extension}}}
	render := map[string]string{"extension_allowlist": "dblink", "extension_version.dblink": version, "extension_schemas.dblink": "public"}

	withoutAuthority, err := PlanDatabaseBootstrap(ctx, target, desired, plan.Options{Render: render})
	if err != nil {
		t.Fatal(err)
	}
	report, err := PreflightBootstrapExtensionsURL(ctx, maintenanceURL, withoutAuthority)
	if err != nil || report.Extensions[0].Status != ExtensionUnauthorized {
		t.Fatalf("HCL trusted metadata bypassed server trust: report=%+v err=%v", report, err)
	}

	legacyRender := cloneRenderOptions(render)
	legacyRender["allow_untrusted_extensions"] = "true"
	legacy, err := PlanDatabaseBootstrap(ctx, target, desired, plan.Options{Render: legacyRender})
	if err != nil {
		t.Fatal(err)
	}
	if report, err = PreflightBootstrapExtensionsURL(ctx, maintenanceURL, legacy); err != nil || !report.Ready {
		t.Fatalf("explicit legacy authority was not preserved: report=%+v err=%v", report, err)
	}

	var untrustedSpec map[string]any
	if err := json.Unmarshal(extension.Spec, &untrustedSpec); err != nil {
		t.Fatal(err)
	}
	untrustedSpec["trusted"] = false
	extension.Spec, _ = json.Marshal(untrustedSpec)
	desired.Graph.Resources[1] = extension
	// The signed inventory intentionally trusts amcheck while authorizing
	// dblink as untrusted. Live server metadata says both are untrusted, so the
	// exact dblink capability must not spill over to amcheck.
	second := renderResource(schema.KindExtension, schema.Name{Schema: "public", Name: "amcheck", Parent: ns.ID}, fmt.Sprintf(`{"version":%q,"relocatable":true,"trusted":true,"superuser":true,"requires":[]}`, secondVersion), schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	desired.Graph.Resources = append(desired.Graph.Resources, second)
	inventory, err := PrepareBootstrapAuthorizationInventory(ctx, target, desired, BootstrapAuthorizationInventoryOptions{})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	manifest, err := NewBootstrapAuthorizationManifest(inventory, now.Add(-time.Minute), now.Add(-time.Minute), now.Add(time.Hour), "security", "dba", "bootstrap-authorization")
	if err != nil {
		t.Fatal(err)
	}
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	if err := manifest.Sign("test-key", private); err != nil {
		t.Fatal(err)
	}
	verified, err := VerifyBootstrapAuthorizationManifest(manifest, inventory, BootstrapAuthorizationVerifyPolicy{Now: func() time.Time { return now }, Keys: map[string]artifact.KeyRecord{"test-key": {PublicKey: public, Issuer: "security", Identity: "dba", Purpose: "bootstrap-authorization", Status: "active", NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}}, Issuer: "security", Signer: "dba", Purpose: "bootstrap-authorization"})
	if err != nil {
		t.Fatal(err)
	}
	manifestPlan, err := PlanDatabaseBootstrapAuthorized(ctx, target, desired, plan.Options{}, verified)
	if err != nil {
		t.Fatal(err)
	}
	report, err = PreflightBootstrapExtensionsURL(ctx, maintenanceURL, manifestPlan)
	if err != nil {
		t.Fatal(err)
	}
	statuses := map[string]ExtensionReadinessStatus{}
	for _, item := range report.Extensions {
		statuses[item.Name] = item.Status
	}
	if report.Ready || statuses["dblink"] != ExtensionReady || statuses["amcheck"] != ExtensionUnauthorized {
		t.Fatalf("manifest extension authority was not exact: report=%+v", report)
	}
}

func TestWholeDatabaseBootstrapResumesBeforeEveryExecutionPhase(t *testing.T) {
	maintenanceURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if maintenanceURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Minute)
	defer cancel()
	desired := bootstrapExecutionDocument(t, "phase_app")
	probeTarget := bootstrapExecutionTarget(t, ctx, maintenanceURL, "autosql_bootstrap_probe")
	probe, err := PlanDatabaseBootstrap(ctx, probeTarget, desired, plan.Options{Render: map[string]string{"concurrent_indexes": "true"}})
	if err != nil {
		t.Fatal(err)
	}
	executionPhaseIndexes := []int{}
	for index, phase := range probe.Phases {
		if phase.Stage != bootstrap.StageDatabaseTarget {
			executionPhaseIndexes = append(executionPhaseIndexes, index)
		}
	}
	for _, phaseIndex := range executionPhaseIndexes {
		phaseIndex := phaseIndex
		t.Run(fmt.Sprintf("phase_%02d_%s", phaseIndex, probe.Phases[phaseIndex].Stage), func(t *testing.T) {
			target := bootstrapExecutionTarget(t, ctx, maintenanceURL, fmt.Sprintf("autosql_bootstrap_phase_%02d", phaseIndex))
			defer DropDatabaseURL(context.Background(), maintenanceURL, target.Name, true)
			_ = DropDatabaseURL(ctx, maintenanceURL, target.Name, true)
			whole, err := PlanDatabaseBootstrap(ctx, target, desired, plan.Options{Render: map[string]string{"concurrent_indexes": "true"}})
			if err != nil {
				t.Fatal(err)
			}
			phaseID := whole.Phases[phaseIndex].ID
			interrupted := false
			_, err = ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, whole, BootstrapExecutionHooks{BeforePhase: func(_ context.Context, phase bootstrap.BootstrapPhase) error {
				if phase.ID == phaseID && !interrupted {
					interrupted = true
					return errors.New("injected")
				}
				return nil
			}})
			if err == nil || !interrupted {
				t.Fatalf("phase interruption err=%v interrupted=%v", err, interrupted)
			}
			result, err := ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, whole, BootstrapExecutionHooks{})
			if err != nil || !result.Completed || !result.Resumed {
				t.Fatalf("phase resume=%+v err=%v", result, err)
			}
			if err := AbortDatabaseBootstrapURL(ctx, maintenanceURL, whole, false); err == nil {
				t.Fatal("managed abort dropped database without explicit authorization")
			}
			if err := AbortDatabaseBootstrapURL(ctx, maintenanceURL, whole, true); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestManagedBootstrapExecutesEnumCheckOutsideSearchPath(t *testing.T) {
	maintenanceURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if maintenanceURL == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	target := bootstrapExecutionTarget(t, ctx, maintenanceURL, "autosql_bootstrap_enum_check")
	defer DropDatabaseURL(context.Background(), maintenanceURL, target.Name, true)
	_ = DropDatabaseURL(ctx, maintenanceURL, target.Name, true)

	namespace := renderResource(schema.KindSchema, schema.Name{Name: "global"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "global", Name: "cells", Parent: namespace.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	cellType := renderResource(schema.KindEnum, schema.Name{Schema: "global", Name: "cell_type", Parent: namespace.ID}, `{"values":["dedicated","isolated","shared"]}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	id := renderResource(schema.KindColumn, schema.Name{Schema: "global", Name: "id", Parent: table.ID}, `{"type":"bigint","not_null":true,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	typeColumn := renderResource(schema.KindColumn, schema.Name{Schema: "global", Name: "type", Parent: table.ID}, `{"type":"global.cell_type","not_null":true,"ordinal":2}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: cellType.ID, Type: schema.DependencyUses})
	// Legacy inspected HCL (including v0.1.19-era snapshots) predates the
	// CHECK-to-type uses edge now required by the renderer.
	check := renderResource(schema.KindCheckConstraint, schema.Name{Schema: "global", Name: "cells_dedicated_capacity_check", Parent: table.ID}, `{"definition":"CHECK (type = ANY (ARRAY['dedicated'::cell_type, 'isolated'::cell_type, 'shared'::cell_type]))","columns":["type"],"deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: typeColumn.ID, Type: schema.DependencyReferences})
	legacy := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{check, typeColumn, id, table, cellType, namespace}}}
	desired, err := New().Normalize(ctx, legacy)
	if err != nil {
		t.Fatal(err)
	}
	for index := range desired.Graph.Resources {
		if desired.Graph.Resources[index].ID == check.ID {
			values := specMap(desired.Graph.Resources[index].Spec)
			values["definition"] = "CHECK (type = ANY (ARRAY['dedicated'::cell_type, 'isolated'::cell_type, 'shared'::cell_type]))"
			desired.Graph.Resources[index].Spec, err = json.Marshal(values)
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	for _, resource := range desired.Graph.Resources {
		if resource.ID == check.ID && strings.Contains(stringValue(spec(resource), "definition"), "global.cell_type") {
			t.Fatal("regression fixture unexpectedly schema-qualified the desired CHECK")
		}
	}
	empty, err := New().Normalize(ctx, schema.Document{Version: schema.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	schemaPlan, err := plan.Build(ctx, New(), empty, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	whole, err := bootstrap.ComposePlan(target, schemaPlan)
	if err != nil {
		t.Fatal(err)
	}
	result, err := ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, whole, BootstrapExecutionHooks{})
	if err != nil || !result.Completed {
		t.Fatalf("enum-check bootstrap result=%+v err=%v", result, err)
	}

	config, err := pgx.ParseConfig(maintenanceURL)
	if err != nil {
		t.Fatal(err)
	}
	config.Database = target.Name
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	if _, err := conn.Exec(ctx, `insert into global.cells(id,type) values (1,'dedicated')`); err != nil {
		t.Fatalf("schema-bound enum check rejected valid row: %v", err)
	}
	inspected, err := InspectConn(ctx, conn, Options{Schemas: []string{"global"}})
	if err != nil {
		t.Fatal(err)
	}
	inspectedHCL, err := source.FormatHCL(inspected)
	if err != nil {
		t.Fatal(err)
	}
	inspected, err = source.LoadContext(ctx, source.Input{URI: "current-global.hcl", Format: source.FormatHCLSource, Data: inspectedHCL})
	if err != nil {
		t.Fatal(err)
	}
	current, err := New().Normalize(ctx, inspected)
	if err != nil {
		t.Fatal(err)
	}
	normalizedDesired, err := New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	noOp, err := plan.Build(ctx, New(), current, normalizedDesired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if len(noOp.Changes.Changes) != 0 || len(noOp.Steps) != 0 {
		t.Fatalf("reinspected enum check did not converge: changes=%+v steps=%+v", noOp.Changes.Changes, noOp.Steps)
	}
}

func bootstrapExecutionTarget(t *testing.T, ctx context.Context, maintenanceURL, prefix string) bootstrap.DatabaseTarget {
	t.Helper()
	config, err := pgx.ParseConfig(maintenanceURL)
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.ConnectConfig(ctx, config)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var owner string
	if err := conn.QueryRow(ctx, `select current_user`).Scan(&owner); err != nil {
		t.Fatal(err)
	}
	return bootstrap.DatabaseTarget{
		Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: config.Host, Port: uint16(config.Port), TLSMode: config.RuntimeParams["sslmode"]},
		MaintenanceDatabase: config.Database, Name: fmt.Sprintf("%s_%d", prefix, time.Now().UnixNano()), Owner: owner,
		Encoding: "UTF8", Template: "template0", Tablespace: "pg_default", ConnectionLimit: -1, AllowConnections: true,
	}
}

func bootstrapExecutionDocument(t *testing.T, namespaceName string) schema.Document {
	t.Helper()
	namespace := renderResource(schema.KindSchema, schema.Name{Name: namespaceName}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: namespaceName, Name: "items", Parent: namespace.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	id := renderResource(schema.KindColumn, schema.Name{Schema: namespaceName, Name: "id", Parent: table.ID}, `{"type":"bigint","not_null":true,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	value := renderResource(schema.KindColumn, schema.Name{Schema: namespaceName, Name: "value", Parent: table.ID}, `{"type":"text","not_null":false,"ordinal":2}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	pk := renderResource(schema.KindPrimaryKey, schema.Name{Schema: namespaceName, Name: "items_pkey", Parent: table.ID}, `{"definition":"PRIMARY KEY (id)","columns":["id"],"deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: id.ID, Type: schema.DependencyReferences})
	index := renderResource(schema.KindIndex, schema.Name{Schema: namespaceName, Name: "items_value_idx", Parent: table.ID}, fmt.Sprintf(`{"definition":"CREATE INDEX items_value_idx ON %s.items USING btree (value)","method":"btree","unique":false,"valid":true,"ready":true,"columns":["value"]}`, namespaceName), schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: value.ID, Type: schema.DependencyReferences})
	document := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{namespace, table, id, value, pk, index}}}
	document, err := New().Normalize(context.Background(), document)
	if err != nil {
		t.Fatal(err)
	}
	return document
}
