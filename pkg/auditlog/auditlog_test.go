package auditlog

import (
	"errors"
	"testing"
	"time"
)

func event(id string, at time.Time) Event {
	return Event{ID: id, At: at, Actor: "operator", Action: "deploy", Subject: "target-1", Result: ResultSuccess, CorrelationID: "run-1", Detail: map[string]string{"artifact_digest": "sha256:abc"}}
}

func TestStoreChainSearchExportRetain(t *testing.T) {
	s := New()
	now := time.Unix(100, 0).UTC()
	if _, err := s.Append(event("one", now)); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(event("two", now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Append(event("two", now.Add(time.Hour))); err != nil {
		t.Fatal(err)
	}
	if err := s.Verify(); err != nil {
		t.Fatal(err)
	}
	if got := len(s.Search(Filter{Action: "deploy"})); got != 2 {
		t.Fatalf("search count %d", got)
	}
	raw, err := s.Export(Filter{Subject: "target-1"})
	if err != nil || len(raw) == 0 {
		t.Fatalf("export: %v", err)
	}
	if err := s.Retain(now.Add(30 * time.Minute)); err != nil {
		t.Fatal(err)
	}
	if got := len(s.Search(Filter{})); got != 1 {
		t.Fatalf("retained count %d", got)
	}
	if err := s.Verify(); err != nil {
		t.Fatal(err)
	}
}

func TestSensitiveAndNotificationDedupRetry(t *testing.T) {
	s := New()
	if _, err := s.Append(Event{ID: "x", At: time.Now(), Actor: "a", Action: "b", Subject: "c", Result: ResultSuccess, CorrelationID: "d", Detail: map[string]string{"password": "no"}}); !errors.Is(err, ErrSensitiveData) {
		t.Fatalf("sensitive error %v", err)
	}
	r, err := s.Append(event("x", time.Now()))
	if err != nil {
		t.Fatal(err)
	}
	attempts := 0
	n := NewRouter(func(Notification) error {
		attempts++
		if attempts < 2 {
			return errors.New("temporary")
		}
		return nil
	})
	n.Retries = 3
	if err := n.Deliver(Notification{Channel: "email", Record: r}); err != nil {
		t.Fatal(err)
	}
	if err := n.Deliver(Notification{Channel: "email", Record: r}); err != nil {
		t.Fatal(err)
	}
	if attempts != 2 {
		t.Fatalf("attempts %d", attempts)
	}
}
