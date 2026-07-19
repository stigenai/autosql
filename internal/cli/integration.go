package cli

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"

	"autosql/pkg/integrations/gitops"
)

type integrationVerification struct {
	ContractDigest string          `json:"contract_digest"`
	Platform       gitops.Platform `json:"platform"`
	Mode           gitops.Mode     `json:"mode"`
	ArtifactDigest string          `json:"artifact_digest"`
	PolicyDigest   string          `json:"policy_digest"`
	TargetSnapshot string          `json:"target_snapshot"`
	ImageDigest    string          `json:"image_digest"`
	Verified       bool            `json:"verified"`
}

func runIntegration(ctx context.Context, args []string, o output, services Services, execute bool) error {
	name := "integration verify"
	if execute {
		name = "integration run"
	}
	fs := newFlags(name, o.streams.Err)
	contractPath := fs.String("contract", "", "path to an AutoSQL integration contract")
	expectedDigest := fs.String("contract-digest", "", "expected immutable contract digest")
	jsonFlag := fs.Bool("json", false, "emit JSON")
	if err := fs.Parse(args); err != nil {
		return usageError(err)
	}
	if fs.NArg() != 0 || *contractPath == "" || *expectedDigest == "" {
		return usageError(errors.New("--contract and --contract-digest are required"))
	}
	o.json = *jsonFlag
	raw, err := os.ReadFile(*contractPath)
	if err != nil {
		return &Error{Kind: "integration", Message: "integration contract is unavailable", Code: ExitConfig, Cause: err}
	}
	var contract gitops.Contract
	if err := json.Unmarshal(raw, &contract); err != nil {
		return &Error{Kind: "integration", Message: "integration contract is invalid", Code: ExitValidation, Cause: err}
	}
	actualDigest, err := contract.BindingDigest()
	if err != nil || !equalDigest(actualDigest, *expectedDigest) {
		return &Error{Kind: "integration", Message: "integration contract binding mismatch", Code: ExitValidation, Cause: err}
	}
	artifactPath, err := gitops.VerifyMaterial(contract)
	if err != nil {
		return &Error{Kind: "integration", Message: "integration material binding mismatch", Code: ExitValidation, Cause: err}
	}
	result := integrationVerification{ContractDigest: actualDigest, Platform: contract.Platform, Mode: contract.Mode, ArtifactDigest: contract.ArtifactDigest, PolicyDigest: contract.PolicyDigest, TargetSnapshot: contract.TargetSnapshot, ImageDigest: contract.Image.Digest, Verified: true}
	if !execute || contract.Mode == gitops.Review {
		return o.success(result, fmt.Sprintf("verified %s integration contract %s", contract.Platform, actualDigest))
	}
	if services.Apply == nil {
		return &Error{Kind: "migration", Message: "verified artifact apply service is not wired", Code: ExitMigration, Status: "refused"}
	}
	applyResult, err := services.Apply.Apply(ctx, ApplyRequest{ArtifactPath: artifactPath, ApprovalMode: "artifact", NoEdits: true})
	if err != nil {
		return applyFailure(applyResult, err)
	}
	if applyResult.Status == "" {
		applyResult.Status = "success"
	}
	return o.success(map[string]any{"verification": result, "apply": applyResult}, applyResult.Status)
}

func equalDigest(a, b string) bool {
	if !strings.HasPrefix(a, "sha256:") || !strings.HasPrefix(b, "sha256:") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(a), []byte(b)) == 1
}
