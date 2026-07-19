package gitops

import (
	"autosql/pkg/integration"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func contract(p Platform) Contract {
	return Contract{Platform: p, Version: "v1", Mode: Deploy, ArtifactRef: "env://AUTOSQL_ARTIFACT", ArtifactDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PolicyDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", TargetSnapshot: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", ApprovalRef: "env://AUTOSQL_APPROVAL", ApprovalDigest: "sha256:eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", OIDC: true, Image: integration.Image{Name: "ghcr.io/acme/autosql", Digest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", Version: "1", Signature: "sig", SBOM: "sbom", Scanned: true, Reproducible: true}, Retry: RetryPolicy{MaxAttempts: 3, Backoff: time.Second}}
}

func TestVerifyMaterialBindsArtifactAndApproval(t *testing.T) {
	dir := t.TempDir()
	artifact := filepath.Join(dir, "artifact.json")
	approval := filepath.Join(dir, "approval.json")
	if err := os.WriteFile(artifact, []byte("artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(approval, []byte("approval"), 0600); err != nil {
		t.Fatal(err)
	}
	digest := func(value string) string {
		h := sha256.Sum256([]byte(value))
		return "sha256:" + hex.EncodeToString(h[:])
	}
	c := contract(GitHub)
	c.ArtifactRef, c.ArtifactDigest = "file://"+artifact, digest("artifact")
	c.ApprovalRef, c.ApprovalDigest = "file://"+approval, digest("approval")
	if got, err := VerifyMaterial(c); err != nil || got != artifact {
		t.Fatalf("path=%q err=%v", got, err)
	}
	c.ApprovalDigest = digest("changed")
	if _, err := VerifyMaterial(c); !errors.Is(err, ErrBinding) {
		t.Fatalf("err=%v", err)
	}
}
func TestPlatformsRenderBoundContract(t *testing.T) {
	for _, p := range []Platform{CircleCI, Bitbucket, AzureDevOps, ArgoCD, GitHub, GitLab, Flux, Crossplane} {
		c := contract(p)
		out, e := Render(c)
		if e != nil || !strings.Contains(out, "sha256:") {
			t.Fatalf("platform=%s out=%s err=%v", p, out, e)
		}
		d, _ := c.BindingDigest()
		if e = (Check{ContractDigest: d, Status: StatusPassed}).Validate(c); e != nil {
			t.Fatal(e)
		}
	}
}
func TestDeployRequiresOIDCAndRejectsResolvedRefs(t *testing.T) {
	c := contract(CircleCI)
	c.OIDC = false
	if _, e := Render(c); e == nil {
		t.Fatal("non-OIDC deploy accepted")
	}
	c = contract(CircleCI)
	c.ArtifactRef = "postgres://user:pass@db/x"
	if _, e := Render(c); e == nil {
		t.Fatal("credential URL accepted")
	}
}
