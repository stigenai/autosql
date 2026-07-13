package contract

import (
	"strings"
	"testing"
)

func TestPreviewBindsDestructivePlan(t *testing.T) {
	s, e := New("release_two", strings.Repeat("a", 64), "v1", "v2", []Step{{ID: "drop_old", Summary: "drop old version", SQL: "DROP SCHEMA v1", CheckSQL: "select true", Recovery: "retry after inspecting schema", Transactional: true}})
	if e != nil {
		t.Fatal(e)
	}
	p, e := PreviewOf(s)
	if e != nil || p.Digest != s.Digest || len(p.Steps) != 1 {
		t.Fatalf("bad preview: %+v %v", p, e)
	}
	s.Steps[0].SQL = "DROP SCHEMA other"
	if s.Validate() == nil {
		t.Fatal("tampered plan accepted")
	}
}
