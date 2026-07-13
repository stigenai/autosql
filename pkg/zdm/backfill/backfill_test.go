package backfill

import (
	"strings"
	"testing"
)

func spec(t *testing.T, dest, job string) Spec {
	t.Helper()
	s, e := New(strings.Repeat("a", 64), job, "bf_app", "items", "id", "old_value", dest, "lower(value)")
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestSpecAndBounds(t *testing.T) {
	s := spec(t, "new_value", "job_one")
	b, e := s.MarshalJSONCanonical()
	if e != nil {
		t.Fatal(e)
	}
	got, e := ParseJSON(b)
	if e != nil || got.Digest != s.Digest {
		t.Fatalf("roundtrip %v", e)
	}
	if _, e = defaults(Config{URL: "x", Target: "t", Environment: "e", BatchSize: 10001, LockTimeoutMS: 1, StatementTimeoutMS: 1}); e == nil {
		t.Fatal("oversize batch accepted")
	}
	bad := s
	bad.Transform = "pg_sleep(value)"
	if e = bad.Validate(); e == nil {
		t.Fatal("unsafe transform accepted")
	}
}
