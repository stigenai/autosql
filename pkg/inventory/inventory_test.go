package inventory

import (
	"errors"
	"testing"
	"time"
)

func observation(at time.Time, id string) Observation {
	return Observation{ReportID: id, CurrentVersion: "v1", ExpectedVersion: "v1", SyncStatus: SyncCurrent, ObservedAt: at}
}

func TestUpsertIsIdempotentAndRejectsConflictingReports(t *testing.T) {
	s := NewStore()
	target := Target{Project: "billing", Environment: "prod", ID: "primary"}
	at := time.Unix(100, 0).UTC()
	want, err := s.Upsert(target, observation(at, "report-1"))
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.Upsert(target, observation(at, "report-1"))
	if err != nil || got != want {
		t.Fatalf("duplicate changed record: %+v %v", got, err)
	}
	changed := observation(at, "report-1")
	changed.CurrentVersion = "v2"
	if err = func() error { _, e := s.Upsert(target, changed); return e }(); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting duplicate err=%v", err)
	}
	if _, err = s.Get(target); err != nil {
		t.Fatal(err)
	}
}

func TestDeploymentStatusesQueriesAndSafeDetails(t *testing.T) {
	s := NewStore()
	at := time.Unix(100, 0).UTC()
	base := DeploymentEvent{ID: "run-1", RunID: "fleet-1", Project: "billing", Environment: "prod", ArtifactDigest: "sha256:a", Status: PartiallySuccessful, At: at, Targets: []TargetResult{{TargetID: "a", Status: Passed, Details: map[string]string{"duration": "2s"}}, {TargetID: "b", Status: Failed}}}
	if err := s.Append(base); err != nil {
		t.Fatal(err)
	}
	if err := s.Append(base); err != nil {
		t.Fatal(err)
	}
	q := s.Events(Query{Project: "billing", TargetID: "b", Statuses: []EventStatus{PartiallySuccessful}})
	if len(q) != 1 || q[0].ID != "run-1" {
		t.Fatalf("query=%+v", q)
	}
	conflict := base
	conflict.Status = Failed
	if err := s.Append(conflict); !errors.Is(err, ErrConflict) {
		t.Fatalf("conflicting event err=%v", err)
	}
	bad := base
	bad.ID = "run-2"
	bad.Status = Passed
	bad.Targets = []TargetResult{{TargetID: "a", Status: Passed, Details: map[string]string{"password": "leak"}}}
	if err := s.Append(bad); !errors.Is(err, ErrSensitive) {
		t.Fatalf("sensitive event err=%v", err)
	}
}

func TestValidationRejectsCredentialsAndInvalidPartiallySuccessful(t *testing.T) {
	s := NewStore()
	at := time.Unix(100, 0).UTC()
	if _, err := s.Upsert(Target{Project: "p", Environment: "prod", ID: "postgres://u:pw@db/x"}, observation(at, "r")); !errors.Is(err, ErrSensitive) {
		t.Fatalf("credential target err=%v", err)
	}
	e := DeploymentEvent{ID: "r", Project: "p", Environment: "prod", Status: PartiallySuccessful, At: at, Targets: []TargetResult{{TargetID: "x", Status: Failed}}}
	if err := s.Append(e); !errors.Is(err, ErrInvalid) {
		t.Fatalf("partial event err=%v", err)
	}
}
