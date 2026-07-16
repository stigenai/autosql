package operator_test

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"autosql/pkg/bootstrap"
	"autosql/pkg/operator"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"

	"github.com/jackc/pgx/v5"
)

func TestOperatorWholeDatabaseBootstrapAgainstNewPostgresDatabase(t *testing.T) {
	maintenanceURL := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
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
	database := fmt.Sprintf("autosql_operator_bootstrap_%d", time.Now().UnixNano())
	target := bootstrap.DatabaseTarget{
		Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: config.Host, Port: uint16(config.Port), TLSMode: "disable"},
		MaintenanceDatabase: config.Database, Name: database, Owner: owner, Encoding: "UTF8", Template: "template0", Tablespace: "pg_default", ConnectionLimit: -1, AllowConnections: true,
	}
	defer postgres.DropDatabaseURL(context.Background(), maintenanceURL, database, true)
	namespace := operatorBootstrapResource(schema.KindSchema, schema.Name{Name: "operator_bootstrap"}, `{}`)
	table := operatorBootstrapResource(schema.KindTable, schema.Name{Schema: "operator_bootstrap", Name: "items", Parent: namespace.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: namespace.ID, Type: schema.DependencyContains})
	column := operatorBootstrapResource(schema.KindColumn, schema.Name{Schema: "operator_bootstrap", Name: "id", Parent: table.ID}, `{"type":"bigint","not_null":true,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	desired, err := postgres.New().Normalize(ctx, schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{namespace, table, column}}})
	if err != nil {
		t.Fatal(err)
	}
	whole, err := postgres.PlanDatabaseBootstrap(ctx, target, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	digest := "sha256:" + strings.Repeat("a", 64)
	authority := &bootstrap.Contract{Identities: []bootstrap.Identity{{Name: "operator", Subject: owner, Authentication: bootstrap.CurrentSession, Capabilities: []bootstrap.Capability{bootstrap.CreateDatabase, bootstrap.ManageSchema}}}}
	resource := operator.Resource{Name: "cell", Spec: operator.Spec{
		Kind: operator.Declarative, Generation: 1, Source: operator.Source{Inline: "approved bootstrap graph"}, ArtifactDigest: digest,
		DatabaseURL: &operator.SecretKeyRef{Name: "target", Key: "url"}, MaintenanceDatabaseURL: &operator.SecretKeyRef{Name: "maintenance", Key: "url"},
		CreateDatabase: true, DatabaseTarget: &target, BootstrapAuthority: authority,
	}, ResolvedMaintenanceDatabaseURL: maintenanceURL}
	reconciler := &operator.Reconciler{Store: operator.NewMemoryStore(), Apply: func(applyCtx context.Context, object operator.Resource, approvedDigest string) (operator.ApplyResult, error) {
		if approvedDigest != digest || object.Spec.DatabaseTarget == nil || object.ResolvedMaintenanceDatabaseURL == "" {
			return operator.ApplyResult{}, fmt.Errorf("operator bootstrap bindings missing")
		}
		result, err := postgres.ExecuteDatabaseBootstrapURL(applyCtx, object.ResolvedMaintenanceDatabaseURL, whole, postgres.BootstrapExecutionHooks{})
		return operator.ApplyResult{Status: "applied", PlanDigest: whole.Digest, TargetIdentity: target.Name, ExecutionID: whole.Digest, PendingStep: result.PendingStep, RecoveryGuidance: result.RecoveryGuidance, AppliedSteps: result.AppliedSteps}, err
	}}
	status, err := reconciler.Reconcile(ctx, resource, true)
	if err != nil || status.AppliedDigest != digest || status.PlanDigest != whole.Digest || status.AppliedSteps == 0 {
		t.Fatalf("operator bootstrap status=%+v err=%v", status, err)
	}
	status, err = reconciler.Reconcile(ctx, resource, true)
	if err != nil || status.AppliedDigest != digest {
		t.Fatalf("operator no-op reconcile status=%+v err=%v", status, err)
	}
}

func operatorBootstrapResource(kind schema.Kind, name schema.Name, raw string, dependencies ...schema.Dependency) schema.Resource {
	resource := schema.Resource{Kind: kind, Name: name, Spec: []byte(raw), Dependencies: dependencies}
	resource.ID = schema.StableID(kind, name)
	return resource
}
