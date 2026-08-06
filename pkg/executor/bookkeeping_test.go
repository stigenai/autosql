package executor

import (
	"testing"

	"autosql/pkg/schema"
)

func bookkeepingDoc() schema.Document {
	resource := func(kind schema.Kind, schemaName, name, parent string) schema.Resource {
		n := schema.Name{Schema: schemaName, Name: name, Parent: parent}
		return schema.Resource{Kind: kind, Name: n, ID: schema.StableID(kind, n)}
	}
	history := resource(schema.KindTable, "public", BookkeepingTableName, "")
	historyColumn := resource(schema.KindColumn, "public", "artifact_digest", history.ID)
	historyPkey := resource(schema.KindPrimaryKey, "public", "autosql_migration_history_pkey", history.ID)
	keep := resource(schema.KindTable, "public", "widgets", "")
	keepColumn := resource(schema.KindColumn, "public", "id", keep.ID)
	// Same table name in a schema that is not managed still gets excluded:
	// the executor lands its history wherever the search_path points.
	otherHistory := resource(schema.KindTable, "legacy", BookkeepingTableName, "")
	return schema.Document{Graph: schema.Graph{Resources: []schema.Resource{history, historyColumn, historyPkey, keep, keepColumn, otherHistory}}}
}

func TestExcludeBookkeepingRemovesHistoryTableAndContents(t *testing.T) {
	got := ExcludeBookkeeping(bookkeepingDoc())
	if len(got.Graph.Resources) != 2 {
		t.Fatalf("expected only the user table and its column to survive, got %#v", got.Graph.Resources)
	}
	for _, r := range got.Graph.Resources {
		if r.Name.Name != "widgets" && r.Name.Name != "id" {
			t.Fatalf("unexpected survivor: %#v", r)
		}
	}
}

func TestExcludeBookkeepingLeavesCleanDocumentUntouched(t *testing.T) {
	clean := ExcludeBookkeeping(bookkeepingDoc())
	again := ExcludeBookkeeping(clean)
	if len(again.Graph.Resources) != len(clean.Graph.Resources) {
		t.Fatalf("clean document changed: %#v", again.Graph.Resources)
	}
}
