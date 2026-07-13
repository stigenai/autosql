package gitops

import (
	"autosql/pkg/integration"
	"strings"
	"testing"
	"time"
)

func contract(p Platform) Contract {
	return Contract{Platform: p, Version: "v1", Mode: Deploy, ArtifactRef: "env://AUTOSQL_ARTIFACT", ArtifactDigest: "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", PolicyDigest: "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb", TargetSnapshot: "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc", ApprovalRef: "env://AUTOSQL_APPROVAL", OIDC: true, Image: integration.Image{Name: "ghcr.io/acme/autosql", Digest: "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd", Version: "1", Signature: "sig", SBOM: "sbom", Scanned: true, Reproducible: true}, Retry: RetryPolicy{MaxAttempts: 3, Backoff: time.Second}}
}
func TestPlatformsRenderBoundContract(t *testing.T) {
	for _, p := range []Platform{CircleCI, Bitbucket, AzureDevOps, ArgoCD, GitHub, GitLab} {
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
