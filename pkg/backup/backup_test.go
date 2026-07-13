package backup

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"testing"
	"time"
)

type failingSource struct{ err error }

func (s failingSource) Get(context.Context, string) (Blob, error) { return Blob{}, s.err }
func digestBytes(b []byte) string                                 { h := sha256.Sum256(b); return "sha256:" + hex.EncodeToString(h[:]) }

func TestReplicateRecordsManifestAndFallbackVerifies(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0).UTC()
	bytes := []byte("artifact-v1")
	b := Blob{Digest: digestBytes(bytes), Bytes: bytes, Signature: []byte("sig"), CreatedAt: now}
	primary := NewMemoryStore()
	backup := NewMemoryStore()
	if err := primary.Put(ctx, b); err != nil {
		t.Fatal(err)
	}
	r := NewReplicator(primary, backup)
	r.Now = func() time.Time { return now.Add(time.Minute) }
	e, err := r.Replicate(ctx, b.Digest)
	if err != nil {
		t.Fatal(err)
	}
	if e.Digest != b.Digest || e.Bytes != len(bytes) {
		t.Fatalf("manifest=%+v", e)
	}
	lag, ok := r.Lag(b.Digest, now.Add(2*time.Minute))
	if !ok || lag != time.Minute {
		t.Fatalf("lag=%v ok=%v", lag, ok)
	}
	f := Fallback{Primary: failingSource{errors.New("outage")}, Backup: backup, VerifySignature: func(x Blob) bool { return string(x.Signature) == "sig" }, MaxStaleness: time.Hour, Now: func() time.Time { return now.Add(2 * time.Minute) }}
	got, err := f.Read(ctx, b.Digest)
	if err != nil || string(got.Bytes) != "artifact-v1" {
		t.Fatalf("fallback=%q err=%v", got.Bytes, err)
	}
}

func TestFallbackExplicitMissingStaleDigestAndSignatureErrors(t *testing.T) {
	ctx := context.Background()
	now := time.Unix(1000, 0).UTC()
	bytes := []byte("x")
	b := Blob{Digest: digestBytes(bytes), Bytes: bytes, Signature: []byte("bad"), CreatedAt: now.Add(-2 * time.Hour)}
	backup := NewMemoryStore()
	if err := backup.Put(ctx, b); err != nil {
		t.Fatal(err)
	}
	f := Fallback{Primary: failingSource{errors.New("down")}, Backup: backup, VerifySignature: func(Blob) bool { return false }, MaxStaleness: time.Hour, Now: func() time.Time { return now }}
	if _, err := f.Read(ctx, b.Digest); !errors.Is(err, ErrSignature) {
		t.Fatalf("signature err=%v", err)
	}
	f.VerifySignature = func(Blob) bool { return true }
	if _, err := f.Read(ctx, b.Digest); !errors.Is(err, ErrStale) {
		t.Fatalf("stale err=%v", err)
	}
	missing := Fallback{Primary: failingSource{errors.New("down")}, Backup: NewMemoryStore(), VerifySignature: func(Blob) bool { return true }}
	if _, err := missing.Read(ctx, b.Digest); !errors.Is(err, ErrMissing) {
		t.Fatalf("missing err=%v", err)
	}
	tampered := NewMemoryStore()
	bad := b
	bad.Digest = digestBytes([]byte("other"))
	if err := tampered.Put(ctx, bad); !errors.Is(err, ErrDigest) {
		t.Fatalf("tamper put err=%v", err)
	}
}

func TestRecoveryDrillReportsRPOAndRTO(t *testing.T) {
	ctx := context.Background()
	at := time.Unix(1000, 0).UTC()
	bytes := []byte("drill")
	b := Blob{Digest: digestBytes(bytes), Bytes: bytes, Signature: []byte("sig"), CreatedAt: at}
	backup := NewMemoryStore()
	if err := backup.Put(ctx, b); err != nil {
		t.Fatal(err)
	}
	now := at.Add(2 * time.Minute)
	f := Fallback{Primary: failingSource{errors.New("outage")}, Backup: backup, VerifySignature: func(Blob) bool { return true }, MaxStaleness: time.Hour, Now: func() time.Time { return now }}
	d, err := f.RecoveryDrill(ctx, b.Digest, 5*time.Minute, time.Second)
	if err != nil || !d.RPOWithin || !d.RTOWithin || d.RPO != 2*time.Minute {
		t.Fatalf("drill=%+v err=%v", d, err)
	}
}
