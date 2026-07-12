package executor

import (
	"context"
	"os"
	"path/filepath"
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
