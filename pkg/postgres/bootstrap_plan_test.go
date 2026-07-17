package postgres

import (
	"context"
	"strings"
	"testing"

	"autosql/pkg/bootstrap"
	"autosql/pkg/plan"
	"autosql/pkg/schema"
)

func TestProjectInspectedBootstrapResourceDoesNotMutateSnapshot(t *testing.T) {
	actual := schema.Resource{
		Kind: schema.KindDefaultPrivilege,
		Name: schema.Name{Name: "owner:tables:reader"},
		Dependencies: []schema.Dependency{
			{Target: "managed", Type: schema.DependencyReferences},
			{Target: "unmanaged", Type: schema.DependencyReferences},
		},
		Spec: []byte(`{}`),
	}
	desired := actual
	desired.Dependencies = actual.Dependencies[:1]
	projected := projectInspectedBootstrapResource(actual, desired, map[string]bool{"managed": true})
	if len(projected.Dependencies) != 1 || projected.Dependencies[0].Target != "managed" {
		t.Fatalf("projected dependencies=%+v", projected.Dependencies)
	}
	if len(actual.Dependencies) != 2 || actual.Dependencies[1].Target != "unmanaged" {
		t.Fatalf("projection mutated inspected snapshot: %+v", actual.Dependencies)
	}
}

func TestManagedBootstrapAdoptsIntrinsicPublicSchema(t *testing.T) {
	public := renderResource(schema.KindSchema, schema.Name{Name: "public"}, `{}`)
	app := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "public", Name: "widgets", Parent: public.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: public.ID, Type: schema.DependencyContains})
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{public, app, table}}}
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: "db.internal", TLSMode: "verify-full"}, MaintenanceDatabase: "postgres", Name: "app", Owner: "postgres", ConnectionLimit: -1, AllowConnections: true}

	whole, err := PlanDatabaseBootstrap(context.Background(), target, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	var publicCreate, appCreate, tableCreate bool
	for _, step := range whole.SchemaPlan.Steps {
		publicCreate = publicCreate || strings.Contains(step.SQL, `CREATE SCHEMA "public"`)
		appCreate = appCreate || strings.Contains(step.SQL, `CREATE SCHEMA "app"`)
		tableCreate = tableCreate || strings.Contains(step.SQL, `CREATE TABLE "public"."widgets"`)
	}
	if publicCreate {
		t.Fatal("managed bootstrap attempted to recreate PostgreSQL's intrinsic public schema")
	}
	if !appCreate || !tableCreate {
		t.Fatalf("ordinary managed resources were not preserved: app_schema=%t public_table=%t", appCreate, tableCreate)
	}

	external := target
	external.Mode = bootstrap.ExternalDatabase
	externalPlan, err := PlanDatabaseBootstrap(context.Background(), external, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	publicCreate = false
	for _, step := range externalPlan.SchemaPlan.Steps {
		publicCreate = publicCreate || strings.Contains(step.SQL, `CREATE SCHEMA "public"`)
	}
	if !publicCreate {
		t.Fatal("external database planning unexpectedly adopted the public schema")
	}
}

func TestManagedBootstrapOrdersEnumBeforeDependentAddedColumn(t *testing.T) {
	namespace := renderResource(schema.KindSchema, schema.Name{Name: "global"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "global", Name: "cells", Parent: namespace.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	statusType := renderResource(schema.KindEnum, schema.Name{Schema: "global", Name: "cell_status", Parent: namespace.ID}, `{"values":["provisioning","active","draining","offline"]}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	status := renderResource(schema.KindColumn, schema.Name{Schema: "global", Name: "status", Parent: table.ID}, `{"type":"\"global\".\"cell_status\"","default":"'provisioning'::cell_status","not_null":true,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: statusType.ID, Type: schema.DependencyUses})
	// Keep the desired graph deliberately out of dependency order. Planning must
	// derive execution topology from edges rather than input serialization.
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{status, table, statusType, namespace}}}
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: "db.internal", TLSMode: "verify-full"}, MaintenanceDatabase: "postgres", Name: "app", Owner: "postgres", ConnectionLimit: -1, AllowConnections: true}

	whole, err := PlanDatabaseBootstrap(context.Background(), target, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	typePosition, columnPosition := -1, -1
	var typeStepID, columnStepID string
	for position, step := range whole.SchemaPlan.Steps {
		switch {
		case strings.Contains(step.SQL, `CREATE TYPE "global"."cell_status"`):
			typePosition, typeStepID = position, step.ID
		case strings.Contains(step.SQL, `ADD COLUMN "status"`):
			columnPosition, columnStepID = position, step.ID
			if !strings.Contains(step.SQL, `'provisioning'::global.cell_status`) && !strings.Contains(step.SQL, `'provisioning'::"global"."cell_status"`) {
				t.Fatalf("column default cast is not bound to the declared enum dependency: %s", step.SQL)
			}
		}
	}
	if typePosition < 0 || columnPosition < 0 || typePosition >= columnPosition {
		t.Fatalf("enum type position=%d column position=%d steps=%+v", typePosition, columnPosition, whole.SchemaPlan.Steps)
	}
	columnStep := schemaStepByID(whole.SchemaPlan, columnStepID)
	if !containsBootstrapID(columnStep.DependsOn, typeStepID) {
		t.Fatalf("column step dependencies=%v missing enum step %s", columnStep.DependsOn, typeStepID)
	}
	var typeBootstrapID string
	for _, step := range whole.Steps {
		if step.SchemaStepID == typeStepID {
			typeBootstrapID = step.ID
		}
	}
	for _, step := range whole.Steps {
		if step.SchemaStepID == columnStepID && (!containsBootstrapID(step.DependsOn, typeBootstrapID) || step.Stage != bootstrap.StageStorage) {
			t.Fatalf("column bootstrap step=%+v missing type bootstrap dependency=%s", step, typeBootstrapID)
		}
	}
}

func TestWholeDatabasePlanStagesCyclicForeignKeysAndOnlineIndexes(t *testing.T) {
	namespace := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	a := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "a", Parent: namespace.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	b := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "b", Parent: namespace.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	aID := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "id", Parent: a.ID}, `{"type":"bigint","not_null":true,"ordinal":1}`, schema.Dependency{Target: a.ID, Type: schema.DependencyContains})
	aBID := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "b_id", Parent: a.ID}, `{"type":"bigint","not_null":false,"ordinal":2}`, schema.Dependency{Target: a.ID, Type: schema.DependencyContains})
	bID := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "id", Parent: b.ID}, `{"type":"bigint","not_null":true,"ordinal":1}`, schema.Dependency{Target: b.ID, Type: schema.DependencyContains})
	bAID := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "a_id", Parent: b.ID}, `{"type":"bigint","not_null":false,"ordinal":2}`, schema.Dependency{Target: b.ID, Type: schema.DependencyContains})
	aPK := renderResource(schema.KindPrimaryKey, schema.Name{Schema: "app", Name: "a_pkey", Parent: a.ID}, `{"definition":"PRIMARY KEY (id)","columns":["id"],"deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: a.ID, Type: schema.DependencyContains}, schema.Dependency{Target: aID.ID, Type: schema.DependencyReferences})
	bPK := renderResource(schema.KindPrimaryKey, schema.Name{Schema: "app", Name: "b_pkey", Parent: b.ID}, `{"definition":"PRIMARY KEY (id)","columns":["id"],"deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: b.ID, Type: schema.DependencyContains}, schema.Dependency{Target: bID.ID, Type: schema.DependencyReferences})
	aFK := renderResource(schema.KindForeignKey, schema.Name{Schema: "app", Name: "a_b_fkey", Parent: a.ID}, `{"definition":"FOREIGN KEY (b_id) REFERENCES app.b(id) NOT VALID","columns":["b_id"],"referenced_columns":["id"],"deferrable":false,"initially_deferred":false,"validated":false}`, schema.Dependency{Target: a.ID, Type: schema.DependencyContains}, schema.Dependency{Target: aBID.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: b.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: bID.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: bPK.ID, Type: schema.DependencyReferences})
	bFK := renderResource(schema.KindForeignKey, schema.Name{Schema: "app", Name: "b_a_fkey", Parent: b.ID}, `{"definition":"FOREIGN KEY (a_id) REFERENCES app.a(id) NOT VALID","columns":["a_id"],"referenced_columns":["id"],"deferrable":false,"initially_deferred":false,"validated":false}`, schema.Dependency{Target: b.ID, Type: schema.DependencyContains}, schema.Dependency{Target: bAID.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: a.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: aID.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: aPK.ID, Type: schema.DependencyReferences})
	index := renderResource(schema.KindIndex, schema.Name{Schema: "app", Name: "a_b_idx", Parent: a.ID}, `{"definition":"CREATE INDEX a_b_idx ON app.a (b_id)","method":"btree","unique":false,"valid":true,"ready":true,"columns":["b_id"]}`, schema.Dependency{Target: a.ID, Type: schema.DependencyContains}, schema.Dependency{Target: aBID.ID, Type: schema.DependencyReferences})
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{namespace, a, b, aID, aBID, bID, bAID, aPK, bPK, aFK, bFK, index}}}
	var err error
	desired, err = New().Normalize(context.Background(), desired)
	if err != nil {
		t.Fatal(err)
	}
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: "db.internal", TLSMode: "verify-full"}, MaintenanceDatabase: "postgres", Name: "app", Owner: "postgres", ConnectionLimit: -1, AllowConnections: true}
	options := plan.Options{Render: map[string]string{"concurrent_indexes": "true"}}
	first, err := PlanDatabaseBootstrap(context.Background(), target, desired, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := PlanDatabaseBootstrap(context.Background(), target, desired, options)
	if err != nil {
		t.Fatal(err)
	}
	left, _ := first.MarshalCanonical()
	right, _ := second.MarshalCanonical()
	if string(left) != string(right) {
		t.Fatal("whole-database plans are not byte deterministic")
	}
	if first.Steps[0].Action != bootstrap.ActionPrepareDatabase || first.Steps[0].Transaction != plan.TransactionProhibited || first.Steps[0].Scope != bootstrap.ScopeServer {
		t.Fatalf("database boundary=%+v", first.Steps[0])
	}
	prepareID := first.Steps[0].ID
	positions := map[string]int{}
	stages := map[bootstrap.ExecutionStage]bool{}
	checkpoints := map[string]bool{}
	for _, phase := range first.Phases {
		if phase.Checkpoint == "" || checkpoints[phase.Checkpoint] {
			t.Fatalf("invalid checkpoint: %+v", phase)
		}
		checkpoints[phase.Checkpoint] = true
		stages[phase.Stage] = true
	}
	for position, step := range first.Steps[1:] {
		if !containsBootstrapID(step.DependsOn, prepareID) {
			t.Fatalf("schema step does not depend on database preparation: %+v", step)
		}
		schemaStep := schemaStepByID(first.SchemaPlan, step.SchemaStepID)
		for _, needle := range []string{`CREATE TABLE "app"."a"`, `CREATE TABLE "app"."b"`, `ADD CONSTRAINT "a_pkey"`, `ADD CONSTRAINT "b_pkey"`, `ADD CONSTRAINT "a_b_fkey"`, `ADD CONSTRAINT "b_a_fkey"`, `CREATE INDEX CONCURRENTLY`} {
			if strings.Contains(schemaStep.SQL, needle) {
				positions[needle] = position
			}
		}
	}
	if positions[`CREATE TABLE "app"."a"`] >= positions[`ADD CONSTRAINT "a_b_fkey"`] || positions[`CREATE TABLE "app"."b"`] >= positions[`ADD CONSTRAINT "a_b_fkey"`] || positions[`ADD CONSTRAINT "b_pkey"`] >= positions[`ADD CONSTRAINT "a_b_fkey"`] {
		t.Fatalf("a-to-b foreign key was not staged after both tables and its referenced key: %v", positions)
	}
	if positions[`CREATE TABLE "app"."a"`] >= positions[`ADD CONSTRAINT "b_a_fkey"`] || positions[`CREATE TABLE "app"."b"`] >= positions[`ADD CONSTRAINT "b_a_fkey"`] || positions[`ADD CONSTRAINT "a_pkey"`] >= positions[`ADD CONSTRAINT "b_a_fkey"`] {
		t.Fatalf("b-to-a foreign key was not staged after both tables and its referenced key: %v", positions)
	}
	if !stages[bootstrap.StageDatabaseTarget] || !stages[bootstrap.StageStorage] || !stages[bootstrap.StageConstraints] || !stages[bootstrap.StageIndexes] {
		t.Fatalf("missing execution stages: %v", stages)
	}
	if positions[`CREATE INDEX CONCURRENTLY`] == 0 {
		t.Fatalf("concurrent index missing: %v", positions)
	}
	for _, step := range first.Steps {
		if step.Stage == bootstrap.StageIndexes && schemaStepByID(first.SchemaPlan, step.SchemaStepID).Transaction != plan.TransactionProhibited {
			t.Fatal("online index is not isolated outside transactions")
		}
	}
	empty := schema.Document{Version: desired.Version}
	teardown, err := PlanDatabaseTransition(context.Background(), target, desired, empty, options)
	if err != nil {
		t.Fatal(err)
	}
	dropPositions := map[string]int{}
	for position, step := range teardown.SchemaPlan.Steps {
		for _, needle := range []string{`DROP CONSTRAINT "a_b_fkey"`, `DROP CONSTRAINT "b_a_fkey"`, `DROP CONSTRAINT "a_pkey"`, `DROP CONSTRAINT "b_pkey"`, `DROP TABLE "app"."a"`, `DROP TABLE "app"."b"`, `DROP SCHEMA "app"`} {
			if strings.Contains(step.SQL, needle) {
				dropPositions[needle] = position
			}
		}
	}
	if dropPositions[`DROP CONSTRAINT "a_b_fkey"`] >= dropPositions[`DROP CONSTRAINT "b_pkey"`] || dropPositions[`DROP CONSTRAINT "b_a_fkey"`] >= dropPositions[`DROP CONSTRAINT "a_pkey"`] || dropPositions[`DROP CONSTRAINT "a_pkey"`] >= dropPositions[`DROP TABLE "app"."a"`] || dropPositions[`DROP CONSTRAINT "b_pkey"`] >= dropPositions[`DROP TABLE "app"."b"`] || dropPositions[`DROP TABLE "app"."a"`] >= dropPositions[`DROP SCHEMA "app"`] || dropPositions[`DROP TABLE "app"."b"`] >= dropPositions[`DROP SCHEMA "app"`] {
		t.Fatalf("teardown did not reverse protected dependencies: %v", dropPositions)
	}
	tampered := first
	tampered.Steps = append([]bootstrap.BootstrapStep(nil), first.Steps...)
	tampered.Steps[1].DependsOn = nil
	if err := tampered.Validate(); err == nil {
		t.Fatal("tampered database dependency passed bootstrap-plan validation")
	}
}

func schemaStepByID(schemaPlan plan.Plan, id string) plan.Step {
	for _, step := range schemaPlan.Steps {
		if step.ID == id {
			return step
		}
	}
	return plan.Step{}
}

func containsBootstrapID(values []string, want string) bool {
	for _, value := range values {
		if value == want {
			return true
		}
	}
	return false
}
