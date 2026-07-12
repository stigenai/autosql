package executor

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync"
)

// FileAudit durably appends redacted, structured lifecycle events.
type FileAudit struct {
	Path string
	mu   sync.Mutex
}

func (a *FileAudit) AppendDurable(ctx context.Context, event LifecycleEvent) error {
	if a == nil || a.Path == "" {
		return errors.New("lifecycle audit path required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	raw, err := json.Marshal(event)
	if err != nil {
		return err
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	f, err := os.OpenFile(a.Path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		return errors.New("open lifecycle audit")
	}
	defer f.Close()
	if _, err = f.Write(append(raw, '\n')); err != nil {
		return errors.New("write lifecycle audit")
	}
	if err = f.Sync(); err != nil {
		return errors.New("sync lifecycle audit")
	}
	return nil
}
