package postgres

import (
	"context"
	"testing"

	"autosql/pkg/schema"
)

func TestPreflightProvisioningValidatesColumnDefaults(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "accounts", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	column := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "created_at", Parent: table.ID}, `{"type":"timestamptz","ordinal":1,"not_null":false,"default":"now(); DROP TABLE app.accounts"}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, column}}}
	report, err := PreflightProvisioning(context.Background(), doc, map[string]string{"postgres_version": "18"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Supported || len(report.Diagnostics) != 1 || report.Diagnostics[0].Field != "default" {
		t.Fatalf("report=%+v", report)
	}
}
