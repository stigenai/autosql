package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"autosql/pkg/plan"
	"autosql/pkg/plugin"
	"autosql/pkg/schema"
	"autosql/pkg/source"
	"github.com/jackc/pgx/v5"
)

func TestParseInspectedPartitionKey(t *testing.T) {
	strategy, columns, err := parseInspectedPartitionKey(`RANGE (tenant_id, "created_at")`)
	if err != nil || strategy != "range" || strings.Join(columns, ",") != "tenant_id,created_at" {
		t.Fatalf("strategy=%q columns=%v err=%v", strategy, columns, err)
	}
	if _, _, err := parseInspectedPartitionKey(`RANGE (date_trunc('day', created_at))`); err == nil {
		t.Fatal("partition expression must fail closed")
	}
}

func TestPartitionLifecycleLive(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("set AUTOSQL_TEST_POSTGRES_URL to run PostgreSQL integration")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	namespace := fmt.Sprintf("autosql_partition_%d", time.Now().UnixNano())
	if _, err := conn.Exec(ctx, `CREATE SCHEMA `+quote(namespace)); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `DROP SCHEMA `+quote(namespace)+` CASCADE`)

	authored := fmt.Sprintf(`
schema %q {}
table "events" {
  schema = schema.%s
  column "created_at" { type = "bigint" }
  partition {
    by = "range"
    columns = [column.created_at]
  }
}
table "events_2026" {
  schema = schema.%s
  partition_of = table.events
  bound = sql("FOR VALUES FROM ('0') TO ('100')")
}
`, namespace, namespace, namespace)
	desired, err := source.ParseHCL("partition-live.hcl", []byte(authored), nil)
	if err != nil {
		t.Fatal(err)
	}
	desired, err = New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	current, err := New().Inspect(ctx, plugin.InspectRequest{URL: url, Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	current, err = New().Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := plan.Build(ctx, New(), current, desired, plan.Options{Render: map[string]string{"postgres_version": "18"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range planned.Steps {
		if _, err := conn.Exec(ctx, step.SQL); err != nil {
			t.Fatalf("execute %s: %v\n%s", step.ID, err, step.SQL)
		}
	}
	applied, err := New().Inspect(ctx, plugin.InspectRequest{URL: url, Schemas: []string{namespace}})
	if err != nil {
		t.Fatal(err)
	}
	applied, err = New().Normalize(ctx, applied)
	if err != nil {
		t.Fatal(err)
	}
	noop, err := plan.Build(ctx, New(), applied, desired, plan.Options{Render: map[string]string{"postgres_version": "18"}})
	if err != nil {
		diff, _ := schema.Diff(applied, desired, schema.DiffOptions{})
		for _, change := range diff.Changes {
			if change.Before != nil && change.After != nil {
				t.Logf("%s %s before=%s deps=%+v after=%s deps=%+v", change.After.Kind, change.After.Name.String(), change.Before.Spec, change.Before.Dependencies, change.After.Spec, change.After.Dependencies)
			}
		}
		t.Fatal(err)
	}
	if len(noop.Steps) != 0 {
		t.Fatalf("partition lifecycle did not converge: %+v", noop.Changes.Changes)
	}
}

func TestRenderPartitionedTableAndPartition(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	parent := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "events", Parent: ns.ID}, `{"partitioned":true,"partition_strategy":"range","partition_columns":["created_at"],"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	column := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "created_at", Parent: parent.ID}, `{"type":"timestamptz","ordinal":1,"not_null":true}`, schema.Dependency{Target: parent.ID, Type: schema.DependencyContains})
	partition := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "events_2026", Parent: ns.ID}, `{"partitioned":false,"partition_of":"`+parent.ID+`","partition_bound":"FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')","persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains}, schema.Dependency{Target: parent.ID, Type: schema.DependencyReferences})
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, parent, column, partition}}}
	normalized, err := New().Normalize(context.Background(), desired)
	if err != nil {
		t.Fatal(err)
	}
	planned, err := plan.Build(context.Background(), New(), schema.Document{Version: schema.SchemaVersion, Annotations: map[string]string{"dialect": "postgresql"}}, normalized, plan.Options{Render: map[string]string{"postgres_version": "18"}})
	if err != nil {
		t.Fatal(err)
	}
	joined := ""
	for _, step := range planned.Steps {
		joined += step.SQL + "\n"
	}
	for _, want := range []string{`CREATE TABLE "app"."events" ("created_at" timestamptz NOT NULL) PARTITION BY RANGE ("created_at")`, `CREATE TABLE "app"."events_2026" PARTITION OF "app"."events" FOR VALUES FROM`} {
		if !strings.Contains(joined, want) {
			t.Fatalf("statements missing %q:\n%s", want, joined)
		}
	}
}
