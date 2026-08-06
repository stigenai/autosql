package migrate

import (
	"testing"

	"autosql/pkg/safety"
)

func TestEmptySuppressionsDigestUnchanged(t *testing.T) {
	old := shaJSON([]safety.Diagnostic{})
	newEmpty := shaJSON(append([]safety.Suppression{}, []safety.Suppression(nil)...))
	if old != newEmpty {
		t.Fatalf("empty suppressions digest changed: %s != %s", old, newEmpty)
	}
	withSuppression := shaJSON(append([]safety.Suppression{}, safety.Suppression{Rule: safety.RuleDropObject, ObjectID: "table:abc", Reason: "approved"}))
	if withSuppression == newEmpty {
		t.Fatal("suppressions did not change the digest")
	}
}
