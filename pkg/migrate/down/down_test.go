package down

import (
	"autosql/pkg/artifact"
	"autosql/pkg/migrate"
	"autosql/pkg/migrate/revision"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"testing"
	"time"
)

func TestDirtyAndUncertainHeadRefuseBeforePlanning(t *testing.T) {
	_, key, _ := ed25519.GenerateKey(rand.Reader)
	m := migrate.Manifest{Digest: "sha256:x", Generation: "g", Entries: []migrate.Migration{{Version: "1"}, {Version: "2"}}}
	for _, state := range []string{"started", "failed", "uncertain", "partial_failure"} {
		_, err := Build(context.Background(), Request{Snapshot: migrate.Snapshot{Manifest: m}, Revisions: []revision.Revision{{Version: "2", State: state, ManifestDigest: m.Digest, ManifestGeneration: m.Generation}}, TargetVersion: "1", Now: time.Now().UTC(), ExpiresAt: time.Now().UTC().Add(time.Hour), SignerKeyID: "k", Signer: key, VerifyOriginal: func(artifact.Artifact) (artifact.VerifiedArtifact, error) {
			return artifact.VerifiedArtifact{}, errors.New("unused")
		}})
		if !errors.Is(err, ErrRefused) {
			t.Fatalf("state %s: %v", state, err)
		}
	}
}

func TestSignedDownPlanBindsHeadManifestAndPayload(t *testing.T) {
	pub, priv, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC()
	m := migrate.Manifest{Digest: "manifest", Generation: "generation"}
	head := revision.Revision{Version: "2", ArtifactDigest: "artifact", ManifestDigest: m.Digest}
	p := DownPlan{Version: "autosql.down-plan/v1", ManifestDigest: m.Digest, ManifestGeneration: m.Generation, HeadVersion: head.Version, HeadArtifactDigest: head.ArtifactDigest, TargetVersion: "1", CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), SignerKeyID: "key"}
	p.Digest = digest(p)
	p.Signature = sign(priv, p.Digest)
	if err := p.Verify(pub, now, m, head); err != nil {
		t.Fatal(err)
	}
	p.TargetVersion = "0"
	if !errors.Is(p.Verify(pub, now, m, head), ErrStale) {
		t.Fatal("tamper accepted")
	}
	p.TargetVersion = "1"
	p.Digest = digest(p)
	p.Signature = sign(priv, p.Digest)
	head.ArtifactDigest = "other"
	if !errors.Is(p.Verify(pub, now, m, head), ErrStale) {
		t.Fatal("stale head accepted")
	}
}
func sign(k ed25519.PrivateKey, d string) string {
	return base64.RawStdEncoding.EncodeToString(ed25519.Sign(k, []byte("autosql.down-plan.signature/v1\x00"+d)))
}

func TestIrreversibleOverrideBindsActorReasonScopeAndExpiry(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC()
	o := &Override{Actor: "incident-commander", Reason: "approved data loss", Scopes: []string{"data:2.0.0", "nontransactional"}, ExpiresAt: now.Add(time.Minute), KeyID: "override"}
	copy := *o
	copy.Signature = ""
	raw, _ := json.Marshal(copy)
	o.Signature = base64.RawStdEncoding.EncodeToString(ed25519.Sign(key, append([]byte("autosql.down.override/v1\x00"), raw...)))
	keys := map[string]ed25519.PublicKey{"override": pub}
	if !validOverride(o, keys, now, "data:2.0.0") {
		t.Fatal("valid override refused")
	}
	for name, mutate := range map[string]func(*Override){"actor": func(x *Override) { x.Actor = "other" }, "reason": func(x *Override) { x.Reason = "changed" }, "scope": func(x *Override) { x.Scopes = []string{"other"} }, "expiry": func(x *Override) { x.ExpiresAt = now.Add(-time.Second) }} {
		t.Run(name, func(t *testing.T) {
			x := *o
			x.Scopes = append([]string(nil), o.Scopes...)
			mutate(&x)
			if validOverride(&x, keys, now, "data:2.0.0") {
				t.Fatal("mutated override accepted")
			}
		})
	}
}

func TestEngineRechecksLockedHeadBeforeFreshAuthorization(t *testing.T) {
	pub, key, _ := ed25519.GenerateKey(rand.Reader)
	now := time.Now().UTC()
	manifest := migrate.Manifest{Digest: "manifest", Generation: "generation"}
	head := revision.Revision{Version: "2", ArtifactDigest: "artifact"}
	p := DownPlan{Version: "autosql.down-plan/v1", ManifestDigest: manifest.Digest, ManifestGeneration: manifest.Generation, HeadVersion: head.Version, HeadArtifactDigest: head.ArtifactDigest, TargetVersion: "1", CreatedAt: now.Add(-time.Minute), ExpiresAt: now.Add(time.Hour), SignerKeyID: "key"}
	p.Digest = digest(p)
	p.Signature = sign(key, p.Digest)
	authorized := false
	err := (Engine{PlanKey: pub, Reload: func(context.Context) (migrate.Snapshot, revision.Revision, error) {
		changed := head
		changed.ArtifactDigest = "stale"
		return migrate.Snapshot{Manifest: manifest}, changed, nil
	}, Authorize: func(context.Context, DownPlan) (artifact.VerifiedArtifact, error) {
		authorized = true
		return artifact.VerifiedArtifact{}, nil
	}, Execute: func(context.Context, DownPlan, artifact.VerifiedArtifact) error { return nil }}).Apply(context.Background(), p)
	if !errors.Is(err, ErrStale) || authorized {
		t.Fatalf("err=%v authorized=%v", err, authorized)
	}
}
