package artifact

import (
	"autosql/pkg/guardrail"
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
	"strings"
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
	changeDigest, _ := guardrail.ChangeDigest(p.Changes)
	var statements []string
	for _, s := range p.Steps {
		if s.Kind == plan.StepExecutable {
			statements = append(statements, s.SQL)
		}
	}
	checks := precheck.Plan{ID: "checks", ChangeDigest: changeDigest, Statements: statements}
	checks.Digest, e = precheck.Digest(checks)
	if e != nil {
		t.Fatal(e)
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	a, e := New(p, checks, now, now.Add(time.Hour), "git:abc", "prod", "sha256:"+strings.Repeat("a", 64), Approval{Identity: "alice", ApprovedAt: now}, map[string]string{"ticket": "DB-1"})
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
func TestTrustedBindingsAndRecomputedInnerDigestsStillRequireSignature(t *testing.T) {
	a, pub, _ := fixture(t)
	p := trustedPolicy(a, pub, a.CreatedAt)
	if _, e := a.VerifyTrusted(p); e != nil {
		t.Fatal(e)
	}
	for name, mutate := range map[string]func(*ExpectedBindings){"plan": func(x *ExpectedBindings) { x.PlanDigest = "sha256:" + strings.Repeat("b", 64) }, "checks": func(x *ExpectedBindings) { x.ChecksDigest = strings.Repeat("b", 64) }, "guardrail": func(x *ExpectedBindings) { x.GuardrailDigest = "sha256:" + strings.Repeat("b", 64) }, "revision": func(x *ExpectedBindings) { x.SourceRevision = "other" }, "environment": func(x *ExpectedBindings) { x.Environment = "stage" }} {
		t.Run(name, func(t *testing.T) {
			q := p
			mutate(&q.Expected)
			if _, e := a.VerifyTrusted(q); e == nil {
				t.Fatal("binding accepted")
			}
		})
	}
	x := cloneArtifact(t, a)
	x.Checks.ID = "changed"
	x.Checks.Digest, _ = precheck.Digest(x.Checks)
	x.Digest, _ = digest(x)
	q := trustedPolicy(x, pub, a.CreatedAt)
	if _, e := x.VerifyTrusted(q); e == nil {
		t.Fatal("recomputed inner and outer digests bypassed signature")
	}
}
func TestTrustedKeyScopeStatusValidityAndClock(t *testing.T) {
	a, pub, _ := fixture(t)
	base := trustedPolicy(a, pub, a.CreatedAt)
	for name, mutate := range map[string]func(*VerifyPolicy){"issuer": func(p *VerifyPolicy) { p.Issuer = "other" }, "identity": func(p *VerifyPolicy) { p.Identity = "other" }, "purpose": func(p *VerifyPolicy) { p.Purpose = "other" }, "environment": func(p *VerifyPolicy) {
		r := p.Keys["key-1"]
		r.Environment = "stage"
		p.Keys = map[string]KeyRecord{"key-1": r}
	}, "revoked": func(p *VerifyPolicy) {
		r := p.Keys["key-1"]
		r.Status = "revoked"
		p.Keys = map[string]KeyRecord{"key-1": r}
	}, "short key": func(p *VerifyPolicy) {
		r := p.Keys["key-1"]
		r.PublicKey = []byte{1}
		p.Keys = map[string]KeyRecord{"key-1": r}
	}, "not yet valid": func(p *VerifyPolicy) {
		r := p.Keys["key-1"]
		r.NotBefore = a.CreatedAt.Add(time.Minute)
		p.Keys = map[string]KeyRecord{"key-1": r}
	}, "future creation": func(p *VerifyPolicy) { p.Now = func() time.Time { return a.CreatedAt.Add(-time.Second) } }} {
		t.Run(name, func(t *testing.T) {
			p := base
			mutate(&p)
			if _, e := a.VerifyTrusted(p); e == nil {
				t.Fatal("policy mismatch accepted")
			}
		})
	}
}
func TestBoundedCanonicalParsing(t *testing.T) {
	a, _, _ := fixture(t)
	b, _ := a.MarshalCanonical()
	cases := map[string][]byte{"whitespace": append([]byte(" "), b...), "duplicate": []byte(`{"version":"a","version":"b"}`), "utf8": []byte{0xff}, "depth": []byte(strings.Repeat("[", 65) + strings.Repeat("]", 65)), "size": make([]byte, (4<<20)+1)}
	for name, data := range cases {
		t.Run(name, func(t *testing.T) {
			if _, e := Parse(data); e == nil {
				t.Fatal("accepted")
			}
		})
	}
}
func TestErrorsAreTypedStableAndRedacted(t *testing.T) {
	secret := "seeded-super-secret"
	_, e := Parse([]byte(`{"future":"` + secret + `"}`))
	var typed *Error
	if !errors.As(e, &typed) || typed.Code == "" || strings.Contains(e.Error(), secret) || strings.Contains(e.Error(), "future") {
		t.Fatalf("unsafe error=%q", e)
	}
	r := &LocalRegistry{Dir: filepath.Join(t.TempDir(), secret)}
	if _, e = r.Get(context.Background(), "../"+secret); e == nil || strings.Contains(e.Error(), secret) {
		t.Fatalf("registry error=%q", e)
	}
}
func registryConformance(t *testing.T, r Registry) {
	a, _, priv := fixture(t)
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
	if e := bad.Sign("key-2", priv); e != nil {
		t.Fatal(e)
	}
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
		other := &LocalRegistry{Dir: dir}
		var wg sync.WaitGroup
		for i := 0; i < 20; i++ {
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				if i%2 == 0 {
					_ = r.Put(context.Background(), a)
				} else {
					_ = other.Put(context.Background(), a)
				}
			}(i)
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
func TestLocalRegistryRejectsTraversalSymlinkHardlinkAndWrongMode(t *testing.T) {
	a, _, _ := fixture(t)
	dir := t.TempDir()
	r := &LocalRegistry{Dir: dir}
	if _, e := r.Get(context.Background(), "../../secret"); e == nil {
		t.Fatal("traversal accepted")
	}
	path := filepath.Join(dir, a.Digest+".json")
	target := filepath.Join(t.TempDir(), "target")
	_ = os.WriteFile(target, []byte("x"), 0600)
	_ = os.Symlink(target, path)
	if e := r.Put(context.Background(), a); e == nil {
		t.Fatal("symlink accepted")
	}
	_ = os.Remove(path)
	if e := r.Put(context.Background(), a); e != nil {
		t.Fatal(e)
	}
	_ = os.Chmod(path, 0644)
	if _, e := r.Get(context.Background(), a.Digest); e == nil {
		t.Fatal("mode accepted")
	}
	_ = os.Chmod(path, 0600)
	link := path + ".link"
	if e := os.Link(path, link); e != nil {
		t.Fatal(e)
	}
	if _, e := r.Get(context.Background(), a.Digest); e == nil {
		t.Fatal("hardlink accepted")
	}
}

type reader RuntimeState

func (r reader) State(context.Context) (RuntimeState, error) { return RuntimeState(r), nil }

type lockState struct {
	value RuntimeState
	calls int
}

func (s *lockState) WithLock(ctx context.Context, fn func(context.Context, FingerprintReader) error) error {
	s.calls++
	return fn(ctx, reader(s.value))
}
func TestStaleCheckIsLockScopedAndExecutionFree(t *testing.T) {
	a, pub, _ := fixture(t)
	policy := trustedPolicy(a, pub, a.CreatedAt)
	policy.Expected.DatabaseIdentity = "db-1"
	verified, e := a.VerifyTrusted(policy)
	if e != nil {
		t.Fatal(e)
	}
	s := &lockState{value: RuntimeState{Fingerprint: a.Plan.FromFingerprint, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: "db-1"}}
	if e := CheckStale(context.Background(), s, verified); e != nil || s.calls != 1 {
		t.Fatalf("%v calls=%d", e, s.calls)
	}
	s.value.Fingerprint = "sha256:other"
	if !errors.Is(CheckStale(context.Background(), s, verified), ErrStale) {
		t.Fatal("stale accepted")
	}
	for name, mutate := range map[string]func(*RuntimeState){"revision": func(x *RuntimeState) { x.SourceRevision = "other" }, "environment": func(x *RuntimeState) { x.Environment = "stage" }, "database": func(x *RuntimeState) { x.DatabaseIdentity = "db-2" }} {
		t.Run(name, func(t *testing.T) {
			state := RuntimeState{Fingerprint: a.Plan.FromFingerprint, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: "db-1"}
			mutate(&state)
			if !errors.Is(CheckStale(context.Background(), &lockState{value: state}, verified), ErrStale) {
				t.Fatal("runtime mismatch accepted")
			}
		})
	}
}
func trustedPolicy(a Artifact, pub ed25519.PublicKey, now time.Time) VerifyPolicy {
	return VerifyPolicy{Now: func() time.Time { return now }, Expected: ExpectedBindings{PlanDigest: a.Plan.Digest, ChecksDigest: a.Checks.Digest, GuardrailDigest: a.GuardrailDigest, SourceRevision: a.SourceRevision, Environment: a.TargetEnvironment, DatabaseIdentity: "db-1", ApprovalIdentity: a.Approval.Identity}, Keys: map[string]KeyRecord{"key-1": {PublicKey: pub, Issuer: "issuer", Identity: "signer", Environment: a.TargetEnvironment, Purpose: "plan-artifact", Status: "active", NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour)}}, Issuer: "issuer", Identity: "signer", Purpose: "plan-artifact"}
}
