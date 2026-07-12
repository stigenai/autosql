package plan_test

import (
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"context"
	"encoding/json"
	"strings"
	"testing"
)

func TestAppendAuthorSQLIsDigestBoundOrderedAndTransactional(t *testing.T) {
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	name := schema.Name{Name: "app"}
	res := schema.Resource{ID: schema.StableID(schema.KindSchema, name), Kind: schema.KindSchema, Name: name, Spec: json.RawMessage(`{}`)}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{res}}}
	p, e := plan.Build(context.Background(), postgres.New(), empty, desired, plan.Options{})
	if e != nil {
		t.Fatal(e)
	}
	got, e := plan.AppendAuthorSQL(p, []plan.AuthorSQL{{ID: "v2/data", SQL: "INSERT INTO audit_log(message) VALUES ('reversed')", Transactional: true}})
	if e != nil {
		t.Fatal(e)
	}
	if e = got.Validate(); e != nil {
		t.Fatal(e)
	}
	last := got.Steps[len(got.Steps)-1]
	if !strings.Contains(last.SQL, "audit_log") || last.Transaction != plan.TransactionRequired || len(last.DependsOn) == 0 || got.Digest == p.Digest {
		t.Fatalf("author SQL not bound: %+v", last)
	}
	tampered := got
	tampered.Steps[len(tampered.Steps)-1].SQL = "DELETE FROM secrets"
	if tampered.Validate() == nil {
		t.Fatal("author SQL tamper accepted")
	}
}
