package shadowsync

import (
	"strings"
	"testing"
)

func sample(t *testing.T) Spec {
	t.Helper()
	s, e := New(strings.Repeat("a", 64), "sync_physical", []Table{{Name: "accounts", Pairs: []Pair{{ID: "p01", OldColumn: "old_name", NewColumn: "new_name", Forward: "lower(value)", Reverse: "upper(value)", Lossy: true}, {ID: "p02", OldColumn: "old_code", NewColumn: "new_code", Forward: "upper(value)", Reverse: "lower(value)", Lossy: true}}}})
	if e != nil {
		t.Fatal(e)
	}
	return s
}
func TestValidationPolicyAndDeterminism(t *testing.T) {
	s := sample(t)
	undeclared := sample(t)
	undeclared.Tables[0].Pairs[0].Lossy = false
	d, _ := digest(undeclared)
	undeclared.Digest = d
	if e := undeclared.Validate(Policy{AllowLossy: true}); e == nil {
		t.Fatal("potentially lossy transform accepted without declaration")
	}
	if e := s.Validate(Policy{}); e == nil {
		t.Fatal("lossy accepted without policy")
	}
	if e := s.Validate(Policy{AllowLossy: true}); e != nil {
		t.Fatal(e)
	}
	b, e := s.MarshalJSONCanonical()
	if e != nil {
		t.Fatal(e)
	}
	got, e := ParseJSON(b)
	if e != nil || got.Digest != s.Digest {
		t.Fatalf("roundtrip: %v", e)
	}
	bad := s
	bad.Tables[0].Pairs[0].Forward = "pg_sleep(value)"
	if e := bad.Validate(Policy{true, true}); e == nil {
		t.Fatal("unsafe expression accepted")
	}
}
func TestNonReversibleRequiresPolicy(t *testing.T) {
	s := sample(t)
	s.Tables[0].Pairs[0].Reverse = ""
	d, e := digest(s)
	if e != nil {
		t.Fatal(e)
	}
	s.Digest = d
	if e = s.Validate(Policy{AllowLossy: true}); e == nil {
		t.Fatal("nonreversible accepted")
	}
	if e = s.Validate(Policy{AllowLossy: true, AllowNonReversible: true}); e != nil {
		t.Fatal(e)
	}
}
