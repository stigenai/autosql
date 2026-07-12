package down

import (
	"autosql/pkg/artifact"
	"autosql/pkg/migrate"
	"autosql/pkg/migrate/revision"
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/base64"
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
