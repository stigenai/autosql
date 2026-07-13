// Package backup replicates signed, content-addressed artifacts to storage
// owned by the customer and provides a verified read fallback.
package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"sync"
	"time"
)

var (
	ErrMissing     = errors.New("backup artifact missing")
	ErrStale       = errors.New("backup artifact is stale")
	ErrDigest      = errors.New("backup artifact digest mismatch")
	ErrSignature   = errors.New("backup artifact signature verification failed")
	ErrPrimary     = errors.New("primary registry unavailable")
	ErrRecoverySLO = errors.New("recovery objective not met")
)

// Blob contains the exact bytes and the release signature covering those
// bytes. Signature verification is deliberately supplied by the caller so a
// customer can use its own key and trust policy.
type Blob struct {
	Digest    string
	Bytes     []byte
	Signature []byte
	CreatedAt time.Time
}

type ObjectStore interface {
	Put(context.Context, Blob) error
	Get(context.Context, string) (Blob, error)
}

type MemoryStore struct {
	mu    sync.RWMutex
	blobs map[string]Blob
}

func NewMemoryStore() *MemoryStore { return &MemoryStore{blobs: map[string]Blob{}} }
func (s *MemoryStore) Put(_ context.Context, b Blob) error {
	if s == nil || b.Digest == "" || len(b.Bytes) == 0 {
		return ErrDigest
	}
	if err := verifyDigest(b); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if old, ok := s.blobs[b.Digest]; ok && string(old.Bytes) != string(b.Bytes) {
		return ErrDigest
	}
	s.blobs[b.Digest] = clone(b)
	return nil
}
func (s *MemoryStore) Get(_ context.Context, d string) (Blob, error) {
	s.mu.RLock()
	b, ok := s.blobs[d]
	s.mu.RUnlock()
	if !ok {
		return Blob{}, ErrMissing
	}
	return clone(b), nil
}

type Source interface {
	Get(context.Context, string) (Blob, error)
}
type ManifestEntry struct {
	Digest       string
	ReplicatedAt time.Time
	CreatedAt    time.Time
	Bytes        int
}
type Replicator struct {
	Source      Source
	Destination ObjectStore
	Now         func() time.Time
	mu          sync.RWMutex
	manifest    map[string]ManifestEntry
}

func NewReplicator(src Source, dst ObjectStore) *Replicator {
	return &Replicator{Source: src, Destination: dst, Now: time.Now, manifest: map[string]ManifestEntry{}}
}

func (r *Replicator) Replicate(ctx context.Context, digest string) (ManifestEntry, error) {
	if r == nil || r.Source == nil || r.Destination == nil || digest == "" {
		return ManifestEntry{}, ErrMissing
	}
	b, err := r.Source.Get(ctx, digest)
	if err != nil {
		return ManifestEntry{}, err
	}
	if b.Digest != digest {
		return ManifestEntry{}, ErrDigest
	}
	if err = verifyDigest(b); err != nil {
		return ManifestEntry{}, err
	}
	if err = r.Destination.Put(ctx, b); err != nil {
		return ManifestEntry{}, err
	}
	now := time.Now()
	if r.Now != nil {
		now = r.Now()
	}
	e := ManifestEntry{Digest: digest, ReplicatedAt: now.UTC(), CreatedAt: b.CreatedAt.UTC(), Bytes: len(b.Bytes)}
	r.mu.Lock()
	r.manifest[digest] = e
	r.mu.Unlock()
	return e, nil
}
func (r *Replicator) Manifest(digest string) (ManifestEntry, bool) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	e, ok := r.manifest[digest]
	return e, ok
}
func (r *Replicator) Lag(digest string, now time.Time) (time.Duration, bool) {
	e, ok := r.Manifest(digest)
	if !ok {
		return 0, false
	}
	if e.CreatedAt.IsZero() || e.ReplicatedAt.IsZero() {
		return 0, true
	}
	// Lag is the replication delay, not the artifact's current age. `now` is
	// retained for API symmetry and to allow callers to reject future clocks.
	if e.ReplicatedAt.After(now) {
		return 0, true
	}
	return e.ReplicatedAt.Sub(e.CreatedAt), true
}

type Fallback struct {
	Primary         Source
	Backup          ObjectStore
	VerifySignature func(Blob) bool
	MaxStaleness    time.Duration
	Now             func() time.Time
}

func (f Fallback) Read(ctx context.Context, digest string) (Blob, error) {
	if digest == "" || f.Backup == nil {
		return Blob{}, ErrMissing
	}
	if f.Primary != nil {
		if b, err := f.Primary.Get(ctx, digest); err == nil {
			if err = verifyBlob(b, digest, f.VerifySignature); err == nil {
				return b, nil
			}
		}
	}
	b, err := f.Backup.Get(ctx, digest)
	if err != nil {
		return Blob{}, ErrMissing
	}
	if err = verifyBlob(b, digest, f.VerifySignature); err != nil {
		return Blob{}, err
	}
	if f.MaxStaleness > 0 && !b.CreatedAt.IsZero() {
		now := time.Now()
		if f.Now != nil {
			now = f.Now()
		}
		if now.Sub(b.CreatedAt) > f.MaxStaleness {
			return Blob{}, ErrStale
		}
	}
	return b, nil
}

type DrillResult struct {
	Digest       string
	UsedFallback bool
	RecoveredAt  time.Time
	RPO          time.Duration
	RTO          time.Duration
	RPOWithin    bool
	RTOWithin    bool
}

func (f Fallback) RecoveryDrill(ctx context.Context, digest string, rpo, rto time.Duration) (DrillResult, error) {
	start := time.Now()
	b, err := f.Read(ctx, digest)
	if err != nil {
		return DrillResult{Digest: digest}, err
	}
	end := time.Now()
	observed := end
	if f.Now != nil {
		observed = f.Now()
	}
	rpoValue := observed.Sub(b.CreatedAt)
	rtoValue := end.Sub(start)
	result := DrillResult{Digest: digest, RecoveredAt: observed.UTC(), RPO: rpoValue, RTO: rtoValue, RPOWithin: rpo <= 0 || rpoValue <= rpo, RTOWithin: rto <= 0 || rtoValue <= rto}
	if !result.RPOWithin || !result.RTOWithin {
		return result, ErrRecoverySLO
	}
	return result, nil
}

func clone(b Blob) Blob {
	b.Bytes = append([]byte(nil), b.Bytes...)
	b.Signature = append([]byte(nil), b.Signature...)
	return b
}
func verifyDigest(b Blob) error {
	h := sha256.Sum256(b.Bytes)
	want := "sha256:" + hex.EncodeToString(h[:])
	if b.Digest != want {
		return fmt.Errorf("%w: want %s", ErrDigest, want)
	}
	return nil
}
func verifyBlob(b Blob, d string, verify func(Blob) bool) error {
	if b.Digest != d {
		return ErrDigest
	}
	if err := verifyDigest(b); err != nil {
		return err
	}
	if verify == nil || !verify(b) {
		return ErrSignature
	}
	return nil
}
