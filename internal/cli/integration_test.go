package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"autosql/pkg/integration"
	"autosql/pkg/integrations/gitops"
)

type integrationApply struct{ calls int }

func (f *integrationApply) Apply(context.Context, ApplyRequest) (ApplyResult, error) {
	f.calls++
	return ApplyResult{Status: "success", AppliedSteps: 1}, nil
}

func TestIntegrationVerifyAndRunUseBoundMaterial(t *testing.T) {
	dir := t.TempDir()
	artifact, approval, contractPath := filepath.Join(dir, "artifact.json"), filepath.Join(dir, "approval.json"), filepath.Join(dir, "contract.json")
	if err := os.WriteFile(artifact, []byte("signed artifact"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(approval, []byte("signed approval"), 0600); err != nil {
		t.Fatal(err)
	}
	digest := func(value string) string {
		h := sha256.Sum256([]byte(value))
		return "sha256:" + hex.EncodeToString(h[:])
	}
	c := gitops.Contract{Platform: gitops.GitHub, Version: "v1", Mode: gitops.Deploy, ArtifactRef: "file://" + artifact, ArtifactDigest: digest("signed artifact"), PolicyDigest: digest("policy"), TargetSnapshot: digest("target"), ApprovalRef: "file://" + approval, ApprovalDigest: digest("signed approval"), OIDC: true, Image: integration.Image{Name: "ghcr.io/stigenai/autosql", Digest: digest("image"), Version: "v1", Signature: "sig", SBOM: "sbom", Scanned: true, Reproducible: true}, Retry: gitops.RetryPolicy{MaxAttempts: 1, Backoff: time.Second}}
	raw, err := json.Marshal(c)
	if err != nil || os.WriteFile(contractPath, raw, 0600) != nil {
		t.Fatal(err)
	}
	binding, err := c.BindingDigest()
	if err != nil {
		t.Fatal(err)
	}
	apply := &integrationApply{}
	for _, mode := range []string{"verify", "run"} {
		var out bytes.Buffer
		code := RunWithServices(context.Background(), []string{"integration", mode, "--contract", contractPath, "--contract-digest", binding, "--json"}, Streams{Out: &out, Err: &bytes.Buffer{}}, Services{Apply: apply})
		if code != int(ExitOK) || strings.Contains(out.String(), "signed artifact") || strings.Contains(out.String(), "signed approval") {
			t.Fatalf("mode=%s code=%d out=%s", mode, code, out.String())
		}
	}
	if apply.calls != 1 {
		t.Fatalf("apply calls=%d", apply.calls)
	}
	if err := os.WriteFile(approval, []byte("altered"), 0600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	code := RunWithServices(context.Background(), []string{"integration", "run", "--contract", contractPath, "--contract-digest", binding, "--json"}, Streams{Out: &out, Err: &bytes.Buffer{}}, Services{Apply: apply})
	if code != int(ExitValidation) || apply.calls != 1 || strings.Contains(out.String(), "altered") {
		t.Fatalf("code=%d calls=%d out=%s", code, apply.calls, out.String())
	}
}
