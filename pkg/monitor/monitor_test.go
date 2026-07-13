package monitor

import (
	"context"
	"errors"
	"testing"
	"time"
)

func target(id string) Target {
	return Target{Provider: AWS, ID: id, Environment: "production", ConnectionRef: "env://DB_" + id, Labels: map[string]string{"team": "core"}}
}
func TestDiscoveryMasksAndBindsSnapshot(t *testing.T) {
	s, e := DiscoverTargets(context.Background(), DiscoveryConfig{Providers: []Discovery{StaticDiscovery{Targets: []Target{target("b"), target("a")}}}, Filter: Filter{Environment: "prod*", Labels: map[string]string{"team": "c*"}}, MaxTargets: 5, Timeout: time.Second})
	if e != nil {
		t.Fatal(e)
	}
	if len(s.Targets) != 2 || s.Targets[0].ID != "a" || s.Targets[0].ConnectionRef != "env://DB_a" {
		t.Fatalf("snapshot=%+v", s)
	}
	if s.ID == "" {
		t.Fatal("missing snapshot digest")
	}
	bad := target("bad")
	bad.ConnectionRef = "postgres://user:pass@db/x"
	if _, e = DiscoverTargets(context.Background(), DiscoveryConfig{Providers: []Discovery{StaticDiscovery{Targets: []Target{bad}}}, MaxTargets: 5, Timeout: time.Second}); !errors.Is(e, ErrSensitive) {
		t.Fatalf("sensitive=%v", e)
	}
}

type fakeInspector struct{ finding bool }

func (f fakeInspector) Inspect(_ context.Context, t Target, _ []string) (Inspection, error) {
	i := Inspection{TargetID: t.ID, ObservedDigest: "sha256:observed", ExpectedDigest: "sha256:expected", CheckedAt: time.Now()}
	if f.finding {
		i.Findings = []Finding{{Code: "RLS001", Severity: Critical, Message: "RLS disabled", Remediation: "enable tenant RLS"}}
		i.ChangePlanDigest = "sha256:plan"
	}
	return i, nil
}

type alerts struct{ n int }

func (a *alerts) Send(_ context.Context, x Alert) error {
	if x.TargetID == "" || x.Type != "drift_detected" {
		return errors.New("bad alert")
	}
	a.n++
	return nil
}
func TestMonitorCreatesResumableHealthAndProposal(t *testing.T) {
	s, _ := DiscoverTargets(context.Background(), DiscoveryConfig{Providers: []Discovery{StaticDiscovery{Targets: []Target{target("a"), target("b")}}}, MaxTargets: 5, Timeout: time.Second})
	m := New()
	sink := &alerts{}
	c, e := m.RunOnce(context.Background(), s, fakeInspector{finding: true}, []string{"public"}, 1, sink)
	if e != nil || len(c) != 2 || sink.n != 2 {
		t.Fatalf("checkpoints=%+v err=%v alerts=%d", c, e, sink.n)
	}
	if cp, ok := m.Checkpoint("a"); !ok || cp.Health != Drifted {
		t.Fatalf("checkpoint=%+v ok=%v", cp, ok)
	}
	if len(m.Proposals()) != 2 || !m.Proposals()[0].RequiresApproval {
		t.Fatalf("proposals=%+v", m.Proposals())
	}
}
func TestMonitorFailureAndStale(t *testing.T) {
	s, _ := DiscoverTargets(context.Background(), DiscoveryConfig{Providers: []Discovery{StaticDiscovery{Targets: []Target{target("a")}}}, MaxTargets: 2, Timeout: time.Second})
	m := New()
	_, e := m.RunOnce(context.Background(), s, inspectorError{}, nil, 1, nil)
	if e != nil {
		t.Fatal(e)
	}
	if cp, _ := m.Checkpoint("a"); cp.Health != Failed {
		t.Fatalf("failure=%+v", cp)
	}
	m.mu.Lock()
	m.checkpoints["a"] = Checkpoint{TargetID: "a", Health: Healthy, LastSuccess: time.Now().Add(-time.Hour)}
	m.mu.Unlock()
	if got := m.Stale(time.Now(), time.Minute); len(got) != 1 || got[0].Health != Stale {
		t.Fatalf("stale=%+v", got)
	}
}

type inspectorError struct{}

func (inspectorError) Inspect(context.Context, Target, []string) (Inspection, error) {
	return Inspection{}, errors.New("revoked credentials")
}
