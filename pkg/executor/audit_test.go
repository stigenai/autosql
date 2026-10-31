package executor

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestFileAuditRejectsTamperedChain(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	a := &FileAudit{Path: p}
	if err := a.AppendDurable(context.Background(), LifecycleEvent{Type: "requested"}); err != nil {
		t.Fatal(err)
	}
	f, err := os.OpenFile(p, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	_, _ = f.WriteString("{}\n")
	_ = f.Close()
	if err = a.AppendDurable(context.Background(), LifecycleEvent{Type: "lock"}); err == nil {
		t.Fatal("tampered audit accepted")
	}
}

func TestFileAuditRetryIgnoresWallClockTimestamp(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	a := &FileAudit{Path: p}
	first := LifecycleEvent{EventID: "event-1", Type: "requested", ExecutionID: "exec-1", At: time.Unix(1, 0).UTC(), Attempt: 1}
	if err := a.AppendDurable(context.Background(), first); err != nil {
		t.Fatal(err)
	}
	second := first
	second.At = time.Unix(2, 0).UTC()
	if err := a.AppendDurable(context.Background(), second); err != nil {
		t.Fatalf("retry with new timestamp: %v", err)
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(raw, []byte("\n")); got != 1 {
		t.Fatalf("records=%d, want one idempotent record", got)
	}
}

func TestFileAuditIndependentInstancesSerializeAndWriteFailure(t *testing.T) {
	p := filepath.Join(t.TempDir(), "audit.jsonl")
	a, b := &FileAudit{Path: p}, &FileAudit{Path: p}
	var wg sync.WaitGroup
	errs := make(chan error, 20)
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			x := a
			if i%2 == 1 {
				x = b
			}
			errs <- x.AppendDurable(context.Background(), LifecycleEvent{Type: "event"})
		}(i)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		if err != nil {
			t.Fatal(err)
		}
	}
	raw, err := os.ReadFile(p)
	if err != nil {
		t.Fatal(err)
	}
	if got := bytes.Count(raw, []byte("\n")); got != 20 {
		t.Fatalf("records=%d", got)
	}
	if err = (&FileAudit{Path: t.TempDir()}).AppendDurable(context.Background(), LifecycleEvent{}); err == nil {
		t.Fatal("directory write succeeded")
	}
}

func TestFileAuditReadExecutionDoesNotCreateOrChangeEvidence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	audit := &FileAudit{Path: path}
	if _, err := audit.ReadExecution(context.Background(), "artifact"); !errors.Is(err, ErrAuditEvidence) {
		t.Fatalf("missing audit was accepted: %v", err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("observation created audit: %v", err)
	}
	event := LifecycleEvent{Type: "completed", ArtifactDigest: "artifact", EventID: "event", At: time.Now().UTC()}
	if err := audit.AppendDurable(context.Background(), event); err != nil {
		t.Fatal(err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := audit.ReadExecution(context.Background(), "artifact")
	if err != nil || len(got) != 1 || got[0] != event {
		t.Fatalf("read=%+v error=%v", got, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("observation changed audit: %v", err)
	}
	for _, invalid := range [][]byte{append(append([]byte{}, before...), []byte("{}\n")...), bytes.TrimSuffix(before, []byte("\n"))} {
		if err := os.WriteFile(path, invalid, 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := audit.ReadExecution(context.Background(), "artifact"); !errors.Is(err, ErrAuditEvidence) {
			t.Fatalf("corrupt or interrupted audit accepted: %v", err)
		}
		if err := audit.AppendDurable(context.Background(), event); !errors.Is(err, ErrAuditEvidence) {
			t.Fatalf("append ignored the same corruption: %v", err)
		}
	}
}

func TestFileAuditReadExecutionCancelsWhileWriterHoldsLock(t *testing.T) {
	path := filepath.Join(t.TempDir(), "audit.jsonl")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		t.Fatal(err)
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()
	if _, err := (&FileAudit{Path: path}).ReadExecution(ctx, "artifact"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("reader did not cancel while writer held lock: %v", err)
	}
}
