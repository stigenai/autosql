package source

import (
	"encoding/json"
	"testing"

	"autosql/pkg/schema"
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

func TestInlineConstraintDefinitionsRetainColumnsAndAttributes(t *testing.T) {
	doc, err := ParseSQL("inline.sql", `CREATE TABLE app.accounts (
id bigint CONSTRAINT account_identity PRIMARY KEY,
email text UNIQUE DEFERRABLE INITIALLY DEFERRED,
owner_id bigint REFERENCES app.accounts(id) ON DELETE SET NULL,
balance integer CHECK (balance >= 0)
);`)
	if err != nil {
		t.Fatal(err)
	}
	seen := map[schema.Kind]bool{}
	for _, resource := range doc.Graph.Resources {
		if resource.Kind != schema.KindPrimaryKey && resource.Kind != schema.KindUniqueConstraint && resource.Kind != schema.KindForeignKey && resource.Kind != schema.KindCheckConstraint {
			continue
		}
		var spec struct {
			Definition string `json:"definition"`
		}
		if err := json.Unmarshal(resource.Spec, &spec); err != nil {
			t.Fatal(err)
		}
		parsed, err := pg_query.Parse("ALTER TABLE app.accounts ADD CONSTRAINT expected " + spec.Definition)
		if err != nil || len(parsed.Stmts) != 1 {
			t.Fatalf("%s lost its definition: %q: %v", resource.Kind, spec.Definition, err)
		}
		constraint := parsed.Stmts[0].Stmt.GetAlterTableStmt().Cmds[0].GetAlterTableCmd().Def.GetConstraint()
		switch resource.Kind {
		case schema.KindPrimaryKey:
			if resource.Name.Name != "account_identity" || len(constraint.Keys) != 1 || constraint.Keys[0].GetString_().Sval != "id" {
				t.Fatal("primary key name/column lost")
			}
		case schema.KindUniqueConstraint:
			if len(constraint.Keys) != 1 || !constraint.Deferrable || !constraint.Initdeferred {
				t.Fatal("unique key columns/deferral lost")
			}
		case schema.KindForeignKey:
			if len(constraint.FkAttrs) != 1 || constraint.FkAttrs[0].GetString_().Sval != "owner_id" || constraint.FkDelAction != "n" {
				t.Fatal("foreign key local column/action lost")
			}
		case schema.KindCheckConstraint:
			if constraint.RawExpr == nil {
				t.Fatal("check expression lost")
			}
		}
		seen[resource.Kind] = true
	}
	if len(seen) != 4 {
		t.Fatalf("missing constraint kinds: %v", seen)
	}
}
