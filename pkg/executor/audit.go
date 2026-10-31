package executor

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"syscall"
	"time"

	"autosql/pkg/artifact"
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
	return appendLifecycle(ctx, f, event, nil)
}

func appendLifecycle(ctx context.Context, f *os.File, event LifecycleEvent, check func([]LifecycleEvent) error) error {
	if _, err := f.Seek(0, 0); err != nil {
		return err
	}
	duplicate := false
	var events []LifecycleEvent
	tail, err := readLifecycle(ctx, f, func(r lifecycleRecord) error {
		if check != nil && (r.Event.ArtifactDigest == event.ArtifactDigest || r.Event.ExecutionID == event.ExecutionID) {
			events = append(events, r.Event)
		}
		if event.EventID != "" && r.Event.EventID == event.EventID {
			// Event IDs intentionally exclude wall-clock time so retries can
			// safely replay the same lifecycle transition. Compare the stable
			// event identity rather than treating a new timestamp as a conflict.
			previous := r.Event
			previous.At = time.Time{}
			current := event
			current.At = time.Time{}
			a, _ := json.Marshal(previous)
			b, _ := json.Marshal(current)
			if string(a) != string(b) {
				return errors.New("lifecycle audit event identity conflict")
			}
			duplicate = true
		}
		return nil
	})
	if err != nil {
		return err
	}
	if check != nil {
		if err := check(events); err != nil {
			return err
		}
	}
	if duplicate {
		return f.Sync()
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

// ErrAuditEvidence means the chain cannot serve as completion evidence.
var ErrAuditEvidence = errors.New("invalid lifecycle audit evidence")

// ErrAuditMissing distinguishes a fresh audit path from a damaged existing chain.
var ErrAuditMissing = fmt.Errorf("%w: missing file", ErrAuditEvidence)

// ReadExecution opens an existing audit without creating or appending records.
// A shared lock protects chain validation and fsync establishes durability even
// if an earlier writer failed after writing bytes but before completing sync.
func (a *FileAudit) ReadExecution(ctx context.Context, artifactDigest string) ([]LifecycleEvent, error) {
	if a == nil || a.Path == "" || artifactDigest == "" {
		return nil, ErrAuditEvidence
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	f, err := os.Open(a.Path)
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrAuditMissing
	}
	if err != nil {
		return nil, errors.New("open lifecycle audit evidence")
	}
	defer f.Close()
	if err = lockLifecycle(ctx, f, syscall.LOCK_SH); err != nil {
		return nil, err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	var events []LifecycleEvent
	_, err = readLifecycle(ctx, f, func(r lifecycleRecord) error {
		if r.Event.ArtifactDigest == artifactDigest || r.Event.ExecutionID == artifactDigest {
			events = append(events, r.Event)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if err := f.Sync(); err != nil {
		return nil, errors.New("sync lifecycle audit evidence")
	}
	return events, ctx.Err()
}

func readLifecycle(ctx context.Context, source io.Reader, visit func(lifecycleRecord) error) (lifecycleRecord, error) {
	scan := bufio.NewScanner(source)
	scan.Split(func(data []byte, atEOF bool) (int, []byte, error) {
		if atEOF && len(data) > 0 && bytes.IndexByte(data, '\n') < 0 {
			return 0, nil, ErrAuditEvidence
		}
		return bufio.ScanLines(data, atEOF)
	})
	var tail lifecycleRecord
	for scan.Scan() {
		if err := ctx.Err(); err != nil {
			return tail, err
		}
		var r lifecycleRecord
		if json.Unmarshal(scan.Bytes(), &r) != nil || r.Sequence != tail.Sequence+1 || r.PreviousHash != tail.Hash || r.Hash != recordHash(r) {
			return tail, ErrAuditEvidence
		}
		if err := visit(r); err != nil {
			return tail, err
		}
		tail = r
	}
	if err := scan.Err(); err != nil {
		if errors.Is(err, ErrAuditEvidence) {
			return tail, err
		}
		return tail, errors.New("read lifecycle audit evidence")
	}
	return tail, ctx.Err()
}

// AppendCompletionObservation adds only the observation event, to an existing
// chain whose original execution evidence is revalidated under the same lock.
// Retrying the same resource generation and execution attempt is idempotent.
func (a *FileAudit) AppendCompletionObservation(ctx context.Context, verified artifact.VerifiedObservation, evidence CompletionEvidence, uid string, generation int64) error {
	payload, err := verified.Payload()
	if err != nil || a == nil || a.Path == "" || uid == "" || generation < 1 || evidence.Outcome != CompletionProven || evidence.ArtifactDigest != payload.Digest || evidence.PlanDigest != payload.Plan.Digest || evidence.TargetIdentity != payload.DatabaseIdentity || evidence.Environment != payload.TargetEnvironment || evidence.Attempt < 1 || evidence.ConfirmedSteps != len(payload.Plan.Steps) || evidence.Fingerprint != payload.Plan.ToFingerprint || evidence.ObservedAt.IsZero() || evidence.ObservedAt.After(time.Now().UTC()) {
		return ErrAuditEvidence
	}
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if err := ctx.Err(); err != nil {
		return err
	}
	f, err := os.OpenFile(a.Path, os.O_RDWR, 0)
	if errors.Is(err, os.ErrNotExist) {
		return ErrAuditEvidence
	}
	if err != nil {
		return errors.New("open lifecycle observation audit")
	}
	defer f.Close()
	if err := lockLifecycle(ctx, f, syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(f.Fd()), syscall.LOCK_UN)
	event := LifecycleEvent{EventID: CompletionObservationID(uid, generation, payload.Digest, evidence.Attempt), Type: "completion_observed", ExecutionID: payload.Digest, ArtifactDigest: payload.Digest, PlanDigest: payload.Plan.Digest, BundleDigest: payload.GuardrailDigest, Target: payload.DatabaseIdentity + "/" + payload.TargetEnvironment, Attempt: evidence.Attempt, At: evidence.ObservedAt.UTC()}
	return appendLifecycle(ctx, f, event, func(events []LifecycleEvent) error {
		if !completionAuditMatches(payload, evidence.Attempt, events, time.Now().UTC()) {
			return ErrAuditEvidence
		}
		return nil
	})
}

func lockLifecycle(ctx context.Context, f *os.File, mode int) error {
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		err := syscall.Flock(int(f.Fd()), mode|syscall.LOCK_NB)
		if err == nil {
			return nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) {
			return errors.New("lock lifecycle audit evidence")
		}
		timer := time.NewTimer(10 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// CompletionObservationID correlates durable observation evidence with operator logs.
func CompletionObservationID(uid string, generation int64, artifactDigest string, attempt int) string {
	identity, _ := json.Marshal(struct {
		UID        string
		Generation int64
		Artifact   string
		Attempt    int
		Type       string
	}{uid, generation, artifactDigest, attempt, "completion_observed"})
	digest := sha256.Sum256(identity)
	return "sha256:" + hex.EncodeToString(digest[:])
}
