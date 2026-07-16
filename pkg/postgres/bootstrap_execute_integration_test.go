package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"autosql/pkg/bootstrap"
	"autosql/pkg/plan"
	"autosql/pkg/schema"

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
	if err := ConfirmBootstrapStepURL(ctx, maintenanceURL, whole, first.PendingStep); err != nil {
		t.Fatal(err)
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
	if err := ConfirmBootstrapStepURL(ctx, maintenanceURL, maintenancePlan, maintenanceResult.PendingStep); err != nil {
		t.Fatal(err)
	}
	maintenanceResult, err = ExecuteDatabaseBootstrapURL(ctx, maintenanceURL, maintenancePlan, BootstrapExecutionHooks{})
	if err != nil || !maintenanceResult.Completed || !maintenanceResult.Resumed {
		t.Fatalf("maintenance resume=%+v err=%v", maintenanceResult, err)
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
