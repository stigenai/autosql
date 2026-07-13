package devflow

import (
	"context"
	"testing"

	"autosql/pkg/source"
)

func req(env string) Request {
	return Request{URI: "schema.sql", Data: []byte("CREATE SCHEMA app; CREATE TABLE app.users (id bigint);"), Format: source.FormatSQL, Environment: env, DatabaseTarget: "local"}
}
func TestOperationsUseCoreParserAndDiff(t *testing.T) {
	ctx := context.Background()
	if _, err := Format(ctx, req("dev")); err != nil {
		t.Fatal(err)
	}
	if _, err := Validate(ctx, req("dev")); err != nil {
		t.Fatal(err)
	}
	p, err := PreviewDiff(ctx, req("dev"), req("dev"))
	if err != nil {
		t.Fatal(err)
	}
	if p.Preview == nil {
		t.Fatal("preview missing")
	}
	if _, err := Generate(ctx, req("dev")); err != nil {
		t.Fatal(err)
	}
}
func TestProductionTargetRejected(t *testing.T) {
	if _, err := Validate(context.Background(), req("production")); err != ErrProductionTarget {
		t.Fatalf("err=%v", err)
	}
	if _, err := (LocalHelper{Environment: "prod"}).ConnectionReference(); err != ErrProductionTarget {
		t.Fatalf("helper err=%v", err)
	}
}
