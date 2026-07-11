package artifact

import (
	"autosql/pkg/plan"
	"autosql/pkg/plugin/sample"
	"autosql/pkg/precheck"
	"autosql/pkg/schema"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func fixture(t *testing.T) (Artifact, ed25519.PublicKey, ed25519.PrivateKey) {
	t.Helper()
	n := schema.Name{Name: "app"}
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{{ID: schema.StableID(schema.KindSchema, n), Kind: schema.KindSchema, Name: n, Spec: []byte(`{}`)}}}}
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	p, e := plan.Build(context.Background(), sample.Driver{}, empty, doc, plan.Options{})
	if e != nil {
		t.Fatal(e)
	}
	checks := precheck.Plan{ID: "checks", ChangeDigest: "sha256:change", Statements: []string{}}
	checks.Digest, e = precheck.Digest(checks)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	a, e := New(p, checks, now, now.Add(time.Hour), "git:abc", "prod", "sha256:guard", Approval{Identity: "alice", ApprovedAt: now}, map[string]string{"ticket": "DB-1"})
	if e != nil {
		t.Fatal(e)
	}
	pub, priv, _ := ed25519.GenerateKey(nil)
	if e = a.Sign("key-1", priv); e != nil {
		t.Fatal(e)
	}
	return a, pub, priv
}
func cloneArtifact(t *testing.T, a Artifact) Artifact {
	b, _ := json.Marshal(a)
	var out Artifact
	if e := json.Unmarshal(b, &out); e != nil {
		t.Fatal(e)
	}
	return out
}
func TestSignatureBindsEverySemanticArea(t *testing.T) {
	a, pub, _ := fixture(t)
	if e := a.Verify(map[string]ed25519.PublicKey{"key-1": pub}, a.CreatedAt); e != nil {
		t.Fatal(e)
	}
	tests := map[string]func(*Artifact){"sql": func(x *Artifact) { x.Plan.Steps = append(x.Plan.Steps, plan.Step{ID: "x"}) }, "checks": func(x *Artifact) { x.Checks.ID = "other" }, "from": func(x *Artifact) { x.Plan.FromFingerprint = "sha256:" + string(make([]byte, 64)) }, "order": func(x *Artifact) { x.Plan.Phases = nil }, "created": func(x *Artifact) { x.CreatedAt = x.CreatedAt.Add(time.Second) }, "expiry": func(x *Artifact) { x.ExpiresAt = x.ExpiresAt.Add(time.Second) }, "revision": func(x *Artifact) { x.SourceRevision = "git:def" }, "environment": func(x *Artifact) { x.TargetEnvironment = "stage" }, "approval": func(x *Artifact) { x.Approval.Identity = "bob" }, "approval time": func(x *Artifact) { x.Approval.ApprovedAt = x.Approval.ApprovedAt.Add(time.Second) }, "guardrail": func(x *Artifact) { x.GuardrailDigest = "other" }, "metadata": func(x *Artifact) { x.Metadata["ticket"] = "DB-2" }, "signature": func(x *Artifact) { x.Signature.Value = "AAAA" }}
	for name, mutate := range tests {
		t.Run(name, func(t *testing.T) {
			x := cloneArtifact(t, a)
			mutate(&x)
			if e := x.Verify(map[string]ed25519.PublicKey{"key-1": pub}, a.CreatedAt); e == nil {
				t.Fatal("mutation verified")
			}
		})
	}
}
func TestUnknownKeyExpiryAndUnknownFieldsFail(t *testing.T) {
	a, pub, _ := fixture(t)
	if e := a.Verify(map[string]ed25519.PublicKey{"other": pub}, a.CreatedAt); e == nil {
		t.Fatal("key confusion accepted")
	}
	if !errors.Is(a.Verify(map[string]ed25519.PublicKey{"key-1": pub}, a.ExpiresAt), ErrExpired) {
		t.Fatal("expiry accepted")
	}
	b, _ := a.MarshalCanonical()
	b = append(b[:len(b)-1], []byte(`,"future":true}`)...)
	if _, e := Parse(b); e == nil {
		t.Fatal("unknown field accepted")
	}
}
func registryConformance(t *testing.T, r Registry) {
	a, _, _ := fixture(t)
	ctx := context.Background()
	if e := r.Put(ctx, a); e != nil {
		t.Fatal(e)
	}
	if e := r.Put(ctx, a); e != nil {
		t.Fatal(e)
	}
	got, e := r.Get(ctx, a.Digest)
	if e != nil || got.Digest != a.Digest {
		t.Fatalf("get=%v %v", got.Digest, e)
	}
	bad := a
	bad.Metadata = map[string]string{"changed": "yes"}
	if e := r.Put(ctx, bad); !errors.Is(e, ErrCollision) {
		t.Fatalf("collision=%v", e)
	}
}
func TestRegistriesImmutableAndConcurrent(t *testing.T) {
	t.Run("memory", func(t *testing.T) { registryConformance(t, NewMemoryRegistry()) })
	t.Run("local", func(t *testing.T) {
		dir := t.TempDir()
		r := &LocalRegistry{Dir: dir}
		registryConformance(t, r)
		a, _, _ := fixture(t)
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func() { defer wg.Done(); _ = r.Put(context.Background(), a) }()
		}
		wg.Wait()
		info, e := os.Stat(filepath.Join(dir, a.Digest+".json"))
		if e != nil || info.Mode().Perm() != 0600 {
			t.Fatalf("mode=%v err=%v", info.Mode(), e)
		}
		matches, _ := filepath.Glob(filepath.Join(dir, ".artifact-*"))
		if len(matches) != 0 {
			t.Fatalf("temporary files: %v", matches)
		}
	})
}

type reader string

func (r reader) Fingerprint(context.Context) (string, error) { return string(r), nil }

type lockState struct {
	value string
	calls int
}

func (s *lockState) WithLock(ctx context.Context, fn func(context.Context, FingerprintReader) error) error {
	s.calls++
	return fn(ctx, reader(s.value))
}
func TestStaleCheckIsLockScopedAndExecutionFree(t *testing.T) {
	a, _, _ := fixture(t)
	s := &lockState{value: a.Plan.FromFingerprint}
	if e := CheckStale(context.Background(), s, a); e != nil || s.calls != 1 {
		t.Fatalf("%v calls=%d", e, s.calls)
	}
	s.value = "sha256:other"
	if !errors.Is(CheckStale(context.Background(), s, a), ErrStale) {
		t.Fatal("stale accepted")
	}
}
