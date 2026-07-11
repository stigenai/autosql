package postgres_test

import (
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"

	"github.com/jackc/pgx/v5"
)

func TestActualPlanApplyReinspectConverges(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	_, err = conn.Exec(ctx, `drop schema if exists autosql_plan cascade; create schema autosql_plan; create table autosql_plan.widgets(id bigint not null, legacy text);`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop schema if exists autosql_plan cascade`)
	current, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_plan"}})
	if err != nil {
		t.Fatal(err)
	}
	desired := current
	desired.Graph.Resources = append([]schema.Resource(nil), current.Graph.Resources...)
	var namespace, table schema.Resource
	filtered := desired.Graph.Resources[:0]
	for _, r := range desired.Graph.Resources {
		if r.Kind == schema.KindSchema {
			namespace = r
		}
		if r.Kind == schema.KindTable && r.Name.Name == "widgets" {
			table = r
		}
		if r.Kind == schema.KindColumn && r.Name.Name == "legacy" {
			continue
		}
		filtered = append(filtered, r)
	}
	desired.Graph.Resources = filtered
	label := schema.Resource{Kind: schema.KindColumn, Name: schema.Name{Schema: "autosql_plan", Name: "label", Parent: table.ID}, Dependencies: []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(`{"not_null":false,"position":3,"type":"text"}`)}
	label.ID = schema.StableID(label.Kind, label.Name)
	status := schema.Resource{Kind: schema.KindEnum, Name: schema.Name{Schema: "autosql_plan", Name: "status", Parent: namespace.ID}, Dependencies: []schema.Dependency{{Target: namespace.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(`{"values":["new","done"]}`)}
	status.ID = schema.StableID(status.Kind, status.Name)
	desired.Graph.Resources = append(desired.Graph.Resources, label, status)
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		t.Fatal(err)
	}
	p, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	first, _ := p.MarshalCanonical()
	again, err := plan.Build(ctx, postgres.New(), current, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	second, _ := again.MarshalCanonical()
	if string(first) != string(second) {
		t.Fatal("repeated plan bytes differ")
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, step := range p.Steps {
		if step.Transaction != plan.TransactionRequired {
			_ = tx.Rollback(ctx)
			t.Fatalf("unexpected nontransactional step: %+v", step)
		}
		if _, err = tx.Exec(ctx, step.SQL); err != nil {
			_ = tx.Rollback(ctx)
			t.Fatalf("apply %s: %v", step.SQL, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	actual, err := postgres.InspectURL(ctx, url, postgres.Options{Schemas: []string{"autosql_plan"}})
	if err != nil {
		t.Fatal(err)
	}
	actual, err = postgres.New().Normalize(ctx, actual)
	if err != nil {
		t.Fatal(err)
	}
	wantFP, _ := schema.SemanticFingerprint(desired)
	gotFP, _ := schema.SemanticFingerprint(actual)
	if gotFP != wantFP {
		t.Fatalf("fingerprint mismatch got=%s want=%s\nplan=%s", gotFP, wantFP, first)
	}
}
