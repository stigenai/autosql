package executor

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"syscall"
)

type lifecycleRecord struct {
	Sequence     uint64         `json:"sequence"`
	PreviousHash string         `json:"previous_hash,omitempty"`
	Hash         string         `json:"hash"`
	Event        LifecycleEvent `json:"event"`
}
type FileAudit struct{ Path string }

func recordHash(r lifecycleRecord) string {
	r.Hash = ""
	b, _ := json.Marshal(r)
	s := sha256.Sum256(append([]byte("autosql.lifecycle.audit/v1\x00"), b...))
	return hex.EncodeToString(s[:])
}
func (a *FileAudit) AppendDurable(ctx context.Context, event LifecycleEvent) error {
	if a == nil || a.Path == "" {
		return errors.New("lifecycle audit path required")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.OpenFile(a.Path, os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return errors.New("open lifecycle audit")
	}
	defer f.Close()
	if err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		return errors.New("lock lifecycle audit")
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	if _, err = f.Seek(0, 0); err != nil {
		return err
	}
	scan := bufio.NewScanner(f)
	var tail lifecycleRecord
	for scan.Scan() {
		var r lifecycleRecord
		if json.Unmarshal(scan.Bytes(), &r) != nil || r.Sequence != tail.Sequence+1 || r.PreviousHash != tail.Hash || r.Hash != recordHash(r) {
			return errors.New("tampered lifecycle audit")
		}
		tail = r
	}
	if err = scan.Err(); err != nil {
		return err
	}
	r := lifecycleRecord{Sequence: tail.Sequence + 1, PreviousHash: tail.Hash, Event: event}
	r.Hash = recordHash(r)
	raw, _ := json.Marshal(r)
	if _, err = f.Seek(0, 2); err != nil {
		return err
	}
	if _, err = f.Write(append(raw, '\n')); err != nil {
		return errors.New("write lifecycle audit")
	}
	if err = f.Sync(); err != nil {
		return errors.New("sync lifecycle audit")
	}
	return nil
}
