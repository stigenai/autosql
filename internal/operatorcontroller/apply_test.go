package operatorcontroller

import (
	"context"
	"os"
	"strings"
	"testing"

	"autosql/pkg/artifact"
	"autosql/pkg/operator"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/source"
	"github.com/jackc/pgx/v5"
)

// This test is opt-in because it requires a reachable PostgreSQL instance.
// It exercises the same source-to-plan check used immediately before the
// signed artifact mutation boundary.
func TestDeclarativePlanVerificationAgainstPostgres(t *testing.T) {
	url := strings.TrimSpace(os.Getenv("AUTOSQL_OPERATOR_PG_URL"))
	if url == "" {
		t.Skip("AUTOSQL_OPERATOR_PG_URL is not set")
	}
	ctx := context.Background()
	const schemaName = "autosql_operator_plan_test"
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(ctx)
	if _, err := conn.Exec(ctx, "drop schema if exists "+pgx.Identifier{schemaName}.Sanitize()+" cascade"); err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(ctx, "drop schema if exists "+pgx.Identifier{schemaName}.Sanitize()+" cascade")

	desiredSQL := "create schema " + schemaName + "; create table " + schemaName + ".orders (id bigint);"
	desired, err := source.LoadContext(ctx, source.Input{URI: "operator:inline", Format: source.FormatSQL, Data: []byte(desiredSQL)})
	if err != nil {
		t.Fatal(err)
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{schemaName}})
	if err != nil {
		t.Fatal(err)
	}
	current, err = postgres.New().Normalize(ctx, current)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	resource := operatorResourceForPlanTest(desiredSQL, url)
	if err := verifyDeclarativePlan(ctx, resource.ResolvedSource, resource.ResolvedDatabaseURL, artifact.Artifact{Plan: p}); err != nil {
		t.Fatal(err)
	}
	if err := verifyDeclarativePlan(ctx, "create schema "+schemaName+";", url, artifact.Artifact{Plan: p}); err == nil {
		t.Fatal("mismatched declarative source was accepted")
	}
}

func operatorResourceForPlanTest(sql, url string) operator.Resource {
	return operator.Resource{ResolvedSource: sql, ResolvedDatabaseURL: url}
}
