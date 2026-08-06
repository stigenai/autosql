package executor

import "autosql/pkg/schema"

// BookkeepingTableName is the unqualified migration-history relation the
// executor maintains inside the target database (see historyDDL). It lands
// in the connection's search_path — in practice a schema that desired-state
// inspections also cover.
const BookkeepingTableName = "autosql_migration_history"

// ExcludeBookkeeping removes the executor's internal bookkeeping relation
// and every resource it contains from an inspected document. The table is
// runtime state, never part of a desired schema, so leaving it in a live
// inspection poisons every desired-state comparison: a plan recomputed
// against a database that has already served one verified apply would
// contain spurious drops of the history table (failing the signed-plan
// digest check), and an apply postcondition fingerprint would never match
// the artifact's ToFingerprint.
func ExcludeBookkeeping(doc schema.Document) schema.Document {
	removed := map[string]bool{}
	for {
		progress := false
		for _, r := range doc.Graph.Resources {
			if removed[r.ID] {
				continue
			}
			if (r.Kind == schema.KindTable && r.Name.Name == BookkeepingTableName) || (r.Name.Parent != "" && removed[r.Name.Parent]) {
				removed[r.ID] = true
				progress = true
			}
		}
		if !progress {
			break
		}
	}
	if len(removed) == 0 {
		return doc
	}
	kept := make([]schema.Resource, 0, len(doc.Graph.Resources)-len(removed))
	for _, r := range doc.Graph.Resources {
		if !removed[r.ID] {
			kept = append(kept, r)
		}
	}
	doc.Graph.Resources = kept
	return doc
}
