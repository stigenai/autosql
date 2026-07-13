package rollback

import (
	"strings"
	"testing"
)

func TestSpecAndRepairDigests(t *testing.T) {
	s, e := New("release", strings.Repeat("a", 64), "v1", "v2")
	if e != nil {
		t.Fatal(e)
	}
	s.NewVersion = "v3"
	if s.Validate() == nil {
		t.Fatal("tampered rollback accepted")
	}
	p, e := NewRepair("repair", "subject", "before", "action", "after")
	if e != nil {
		t.Fatal(e)
	}
	p.Action = "other"
	if repairDigest(p) == p.Digest {
		t.Fatal("tampered repair accepted")
	}
}
