package plan

import (
	"encoding/json"
	"testing"

	"autosql/pkg/schema"
)

func metadataResource(kind schema.Kind, spec string) schema.Resource {
	r := schema.Resource{Kind: kind, Name: schema.Name{Name: "x"}, Spec: json.RawMessage(spec)}
	r.ID = schema.StableID(kind, r.Name)
	return r
}

func TestConservativeStructuralMetadata(t *testing.T) {
	volatile := metadataResource(schema.KindColumn, `{"default":"nextval('s')","not_null":false}`)
	if got := impactFor(schema.Change{Operation: schema.OperationCreate, After: &volatile}, "ALTER TABLE t ADD COLUMN x bigint DEFAULT nextval('s')"); !got.Rewrites {
		t.Fatal("unknown/volatile default not marked rewrite")
	}
	before := metadataResource(schema.KindColumn, `{"not_null":false}`)
	after := before
	after.Spec = json.RawMessage(`{"not_null":true}`)
	if got := impactFor(schema.Change{Operation: schema.OperationAlter, Before: &before, After: &after}, "ALTER TABLE t ALTER COLUMN x SET NOT NULL"); !got.Scans {
		t.Fatal("NOT NULL not marked scan")
	}
	constraint := metadataResource(schema.KindForeignKey, `{"definition":"FOREIGN KEY (x) REFERENCES y(id)"}`)
	if got := impactFor(schema.Change{Operation: schema.OperationCreate, After: &constraint}, "ALTER TABLE t ADD CONSTRAINT f FOREIGN KEY (x) REFERENCES y(id)"); !got.Scans {
		t.Fatal("constraint not marked scan")
	}
	if got := impactFor(schema.Change{Operation: schema.OperationAlter, After: &constraint}, "DROP INDEX x"); !got.Destructive {
		t.Fatal("rebuild drop not destructive")
	}
	if got := lockFor(schema.Change{}, "CREATE INDEX CONCURRENTLY x ON t(x)"); got != LockShare {
		t.Fatalf("concurrent lock=%s", got)
	}
}
