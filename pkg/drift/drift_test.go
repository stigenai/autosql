package drift

import (
	"context"
	"testing"
	"time"

	"autosql/pkg/schema"
)

type inspector struct{ doc schema.Document }

func (i inspector) Inspect(context.Context, Target) (schema.Document, error) { return i.doc, nil }

func doc(name string) schema.Document {
	n := schema.Name{Schema: "app", Name: name}
	r := schema.Resource{ID: schema.StableID(schema.KindTable, n), Kind: schema.KindTable, Name: n}
	d := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{r}}}
	d.Normalize()
	return d
}

func TestMonitorDeduplicatesAndResolves(t *testing.T) {
	expected, actual := doc("users"), doc("accounts")
	now := time.Unix(100, 0)
	m := New()
	m.Now = func() time.Time { return now }
	target := Target{ID: "db-1", Expected: expected, ExpectedDigest: "sha256:expected", ReadOnly: true, MaxResources: 10}
	a, err := m.Check(context.Background(), inspector{actual}, target)
	if err != nil {
		t.Fatal(err)
	}
	if a.Status != "open" || len(a.Remediation) == 0 {
		t.Fatalf("incident %#v", a)
	}
	b, err := m.Check(context.Background(), inspector{actual}, target)
	if err != nil {
		t.Fatal(err)
	}
	if a.Key != b.Key || len(m.Incidents()) != 1 {
		t.Fatalf("dedup %#v", m.Incidents())
	}
	resolved, err := m.Check(context.Background(), inspector{expected}, target)
	if err != nil {
		t.Fatal(err)
	}
	if resolved.Status != "in_sync" || len(m.Incidents()) != 1 || m.Incidents()[0].Status != "resolved" {
		t.Fatalf("resolve %#v", m.Incidents())
	}
}

func TestMonitorRequiresReadOnlyBound(t *testing.T) {
	d := doc("users")
	m := New()
	if _, err := m.Check(context.Background(), inspector{d}, Target{ID: "x", Expected: d, ReadOnly: false, MaxResources: 1}); err != ErrInvalidTarget {
		t.Fatalf("err %v", err)
	}
}

func TestAcceptBaselineResolvesWithoutMutation(t *testing.T) {
	expected, actual := doc("users"), doc("accounts")
	m := New()
	if _, err := m.Check(context.Background(), inspector{actual}, Target{ID: "db-1", Expected: expected, ExpectedDigest: "sha256:x", ReadOnly: true, MaxResources: 10}); err != nil {
		t.Fatal(err)
	}
	if err := m.AcceptBaseline("db-1"); err != nil {
		t.Fatal(err)
	}
	if got := m.Incidents(); len(got) != 1 || got[0].Status != "resolved" {
		t.Fatalf("incidents=%+v", got)
	}
}
