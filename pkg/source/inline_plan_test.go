package source_test

import (
	"context"
	"testing"

	"autosql/pkg/dbtest"
	"autosql/pkg/source"
)

func TestInlineConstraintsRenderBlankToDesired(t *testing.T) {
	doc, err := source.ParseSQL("inline.sql", `CREATE SCHEMA app;
CREATE TABLE app.parent(id bigint CONSTRAINT parent_pk PRIMARY KEY);
CREATE TABLE app.child(
 id bigint PRIMARY KEY,
 email text UNIQUE DEFERRABLE INITIALLY DEFERRED,
 parent_id bigint REFERENCES app.parent(id) ON DELETE SET NULL,
 balance integer DEFAULT 0 CONSTRAINT nonnegative CHECK (balance >= 0),
 constant integer CHECK (true)
);`)
	if err != nil {
		t.Fatal(err)
	}
	for _, major := range []int{14, 15, 16, 17, 18} {
		if _, err := dbtest.PrepareSchema(context.Background(), doc, major); err != nil {
			t.Fatalf("major %d: %+v", major, err)
		}
	}
}
