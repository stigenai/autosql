package security

import (
	"context"
	"errors"
	"testing"
	"time"
)

func baseState() State {
	return State{Principals: []Principal{{Name: "autosql_admin", Kind: Role, Managed: true}, {Name: "app", Kind: Role, Managed: true}, {Name: "vendor", Kind: Role, External: true}}, Grants: []Grant{{Object: "app.orders", Grantee: "app", Grantor: "autosql_admin", Privilege: "SELECT"}}}
}

func TestPlanProtectsExternalAndExecutingPrincipal(t *testing.T) {
	s := baseState()
	desired := State{Principals: []Principal{{Name: "app", Kind: Role, Managed: true}}}
	if _, err := Plan(desired, s, PlanOptions{ExecutingPrincipal: "autosql_admin"}); !errors.Is(err, ErrLockout) {
		t.Fatalf("lockout error=%v", err)
	}
	desired.Principals = append(desired.Principals, Principal{Name: "autosql_admin", Kind: Role, Managed: true})
	changes, err := Plan(desired, s, PlanOptions{ExecutingPrincipal: "autosql_admin"})
	if err != nil {
		t.Fatal(err)
	}
	for _, change := range changes {
		if change.Kind == DropPrincipal && change.Before != nil && change.Before.Name == "vendor" {
			t.Fatal("external principal was scheduled for drop")
		}
	}
}

func TestSanitizedDigestAndPolicy(t *testing.T) {
	s := baseState()
	s.Principals[1].AuthRef = "env://APP_PASSWORD"
	d, err := s.Digest()
	if err != nil || d == "" {
		t.Fatal(err)
	}
	clean := s.Sanitized()
	if clean.Principals[1].AuthRef != "" {
		t.Fatal("secret reference leaked")
	}
	v, err := EvaluatePolicy(s, Policy{Rules: []Rule{{Name: "public", Check: NoPublicGrants}}})
	if err != nil || len(v) != 0 {
		t.Fatalf("policy=%v %v", v, err)
	}
	s.Grants = append(s.Grants, Grant{Object: "app.orders", Grantee: "PUBLIC", Grantor: "autosql_admin", Privilege: "SELECT"})
	v, _ = EvaluatePolicy(s, Policy{Rules: []Rule{{Name: "public", Check: NoPublicGrants}}})
	if len(v) != 1 {
		t.Fatalf("expected violation: %+v", v)
	}
}

type tokenSource struct{ n int }

func (s *tokenSource) Token(context.Context) (string, time.Time, error) {
	s.n++
	return "token", time.Now().Add(time.Minute), nil
}
func TestSessionRefreshesShortLivedToken(t *testing.T) {
	src := &tokenSource{}
	sess := &Session{Source: src, RefreshBefore: time.Second}
	if _, err := sess.Credential(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := sess.Credential(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.n != 1 {
		t.Fatalf("refresh count=%d", src.n)
	}
	sess.Clear()
	if _, err := sess.Credential(context.Background()); err != nil {
		t.Fatal(err)
	}
	if src.n != 2 {
		t.Fatalf("clear did not expire token")
	}
}
