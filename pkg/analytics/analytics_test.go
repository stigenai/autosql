package analytics

import (
	"errors"
	"testing"
	"time"
)

const d1 = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
const d2 = "sha256:2222222222222222222222222222222222222222222222222222222222222222"

func fixture() StaticSource {
	return StaticSource{Tables: []Table{{ID: "table:users", Schema: "public", Name: "users", TotalBytes: 100, DeadRows: 4}}, Indexes: []Index{{ID: "index:users_email", TableID: "table:users", Schema: "public", Name: "users_email", Bytes: 20, Valid: true}}, Constraints: []Constraint{{ID: "constraint:users_pk", TableID: "table:users", Kind: "primary_key", Name: "users_pkey", Columns: 1}}, Permissions: PermissionSummary{Role: "autosql_reader", Granted: []string{"catalog.statistics"}}}
}

func TestCollectorBindsDigestsAndComputesComplexity(t *testing.T) {
	c := Collector{Source: fixture(), Now: func() time.Time { return time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC) }}
	s, err := c.Collect(Request{TargetID: "prod", ArtifactDigest: d1, SchemaDigest: d2, MaxTables: 10, MaxIndexes: 10, MaxConstraints: 10})
	if err != nil {
		t.Fatal(err)
	}
	if s.Complexity.Tables != 1 || s.Complexity.Indexes != 1 || s.Complexity.Constraints != 1 || s.ObservedAt.Year() != 2026 {
		t.Fatalf("unexpected snapshot: %#v", s)
	}
	if _, err := s.Digest(); err != nil {
		t.Fatal(err)
	}
}

func TestCollectorBoundsAndRedactsCredentialLikeValues(t *testing.T) {
	c := Collector{Source: fixture()}
	if _, err := c.Collect(Request{TargetID: "prod", ArtifactDigest: d1, SchemaDigest: d2, MaxTables: 0, MaxIndexes: 1, MaxConstraints: 1}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("expected invalid request, got %v", err)
	}
	bad := fixture()
	bad.Tables[0].Name = "postgres://user:pass@host/db"
	if _, err := (Collector{Source: bad}).Collect(Request{TargetID: "prod", ArtifactDigest: d1, SchemaDigest: d2, MaxTables: 10, MaxIndexes: 10, MaxConstraints: 10}); !errors.Is(err, ErrSensitive) {
		t.Fatalf("expected sensitive error, got %v", err)
	}
}

func TestEvaluateGrowthAndHistoryRetention(t *testing.T) {
	c := Collector{Source: fixture(), Now: func() time.Time { return time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC) }}
	now, _ := c.Collect(Request{TargetID: "prod", ArtifactDigest: d1, SchemaDigest: d2, MaxTables: 10, MaxIndexes: 10, MaxConstraints: 10})
	old := now
	old.ObservedAt = old.ObservedAt.Add(-time.Hour)
	old.Tables = append([]Table(nil), now.Tables...)
	old.Tables[0].TotalBytes = 1
	findings, err := Evaluate(now, &old, Thresholds{MaxGrowthBytes: 50})
	if err != nil || len(findings) != 1 || findings[0].Code != "growth_bytes" {
		t.Fatalf("unexpected findings: %#v %v", findings, err)
	}
	store := NewStore(30*time.Minute, 10)
	if err := store.Append(old); err != nil {
		t.Fatal(err)
	}
	if err := store.Append(now); err != nil {
		t.Fatal(err)
	}
	if got := len(store.Query("prod", time.Time{}, time.Time{})); got != 1 {
		t.Fatalf("retention should remove old snapshot, got %d", got)
	}
}

func TestEvaluateIgnoresDriftedComparison(t *testing.T) {
	c := Collector{Source: fixture()}
	current, _ := c.Collect(Request{TargetID: "prod", ArtifactDigest: d1, SchemaDigest: d2, MaxTables: 10, MaxIndexes: 10, MaxConstraints: 10})
	other := current
	other.SchemaDigest = d1
	if findings, err := Evaluate(current, &other, Thresholds{MaxGrowthBytes: 1}); err != nil || len(findings) != 0 {
		t.Fatalf("drifted snapshot should not compare: %#v %v", findings, err)
	}
}
