package executor

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
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
