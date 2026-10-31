package executor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestCompletionAuditAppendIsIdempotentAndRequiresExecutionEvidence(t *testing.T) {
	f := newCompletionFixture(t)
	evidence := ObserveCompletion(context.Background(), f.verified, &completionReader{rows: &valueRows{values: f.rows}, state: f.state}, completionAudit{events: f.events}, f.now)
	audit := &FileAudit{Path: filepath.Join(t.TempDir(), "audit.jsonl")}
	appendObservation := func() error {
		return audit.AppendCompletionObservation(context.Background(), f.verified, evidence, "uid", 1)
	}
	if err := appendObservation(); !errors.Is(err, ErrAuditEvidence) {
		t.Fatalf("missing audit: %v", err)
	}
	if _, err := os.Stat(audit.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("created missing audit")
	}
	for _, event := range f.events {
		if err := audit.AppendDurable(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
	before, err := os.ReadFile(audit.Path)
	if err != nil {
		t.Fatal(err)
	}
	if err := appendObservation(); err != nil {
		t.Fatal(err)
	}
	evidence.ObservedAt = evidence.ObservedAt.Add(time.Second)
	if err := appendObservation(); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(audit.Path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(after, before) || bytes.Count(after, []byte("\n")) != len(f.events)+1 {
		t.Fatal("observation rewrote or duplicated execution history")
	}
	events, err := audit.ReadExecution(context.Background(), f.a.Digest)
	if err != nil || events[len(events)-1].Type != "completion_observed" || !completionAuditMatches(f.a, 1, events, evidence.ObservedAt) {
		t.Fatalf("observation invalidated evidence: %v", err)
	}
	if err := os.WriteFile(audit.Path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if err := appendObservation(); !errors.Is(err, ErrAuditEvidence) {
		t.Fatalf("empty chain fabricated completion: %v", err)
	}
	if raw, _ := os.ReadFile(audit.Path); len(raw) != 0 {
		t.Fatal("invalid chain was appended")
	}
	if err := os.WriteFile(audit.Path, append(before, []byte("{}\n")...), 0600); err != nil {
		t.Fatal(err)
	}
	if err := appendObservation(); !errors.Is(err, ErrAuditEvidence) {
		t.Fatalf("corrupt chain accepted: %v", err)
	}
}

func TestCompletionAuditAppendCancelsWhileLocked(t *testing.T) {
	f := newCompletionFixture(t)
	evidence := ObserveCompletion(context.Background(), f.verified, &completionReader{rows: &valueRows{values: f.rows}, state: f.state}, completionAudit{events: f.events}, f.now)
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	if err := syscall.Flock(int(file.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if err := (&FileAudit{Path: path}).AppendCompletionObservation(ctx, f.verified, evidence, "uid", 1); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("writer failed to cancel: %v", err)
	}
}
