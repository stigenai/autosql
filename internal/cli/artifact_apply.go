package cli

import (
	"context"
	"errors"
	"os"
	"reflect"

	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/guardrail"
)

// VerifiedArtifactApplyService is the only CLI bridge to mutation. Callers must
// provide trusted verification policy, the exact bound guardrail input, and an
// executor mutation factory. No raw plan or SQL execution path exists here.
type VerifiedArtifactApplyService struct {
	Policy    artifact.VerifyPolicy
	Guardrail guardrail.Guardrail
	Input     func(artifact.Artifact) (guardrail.Input, error)
	Mutation  func(artifact.VerifiedArtifact) (guardrail.AuthorizedMutation, error)
}

func (s VerifiedArtifactApplyService) Apply(ctx context.Context, request ApplyRequest) (ApplyResult, error) {
	if request.ApprovalMode != "artifact" || request.ArtifactPath == "" || s.Input == nil || s.Mutation == nil {
		return ApplyResult{Status: "refused"}, errors.New("verified artifact apply configuration required")
	}
	raw, err := os.ReadFile(request.ArtifactPath)
	if err != nil {
		return ApplyResult{Status: "refused"}, errors.New("read migration artifact")
	}
	a, err := artifact.Parse(raw)
	if err != nil {
		return ApplyResult{Status: "refused"}, err
	}
	v, err := a.VerifyTrusted(s.Policy)
	if err != nil {
		return ApplyResult{Status: "refused"}, err
	}
	verifiedPayload, err := v.Payload()
	if err != nil {
		return ApplyResult{Status: "refused"}, err
	}
	if request.AssertedDigest != "" && request.AssertedDigest != verifiedPayload.Plan.Digest {
		return ApplyResult{Status: "refused"}, errors.New("artifact plan digest does not match asserted digest")
	}
	in, err := s.Input(verifiedPayload)
	if err != nil {
		return ApplyResult{Status: "refused"}, err
	}
	artifactChangeDigest, err := guardrail.ChangeDigest(verifiedPayload.Plan.Changes)
	if err != nil || in.Precheck.Digest != verifiedPayload.Checks.Digest || in.Precheck.ChangeDigest != artifactChangeDigest || !reflect.DeepEqual(in.Precheck.Statements, verifiedPayload.Checks.Statements) {
		return ApplyResult{Status: "refused"}, errors.New("artifact guardrail input binding mismatch")
	}
	bundleDigest, err := s.Guardrail.BundleDigest(in)
	if err != nil || bundleDigest != verifiedPayload.GuardrailDigest || in.Approval.Plan.Digest != verifiedPayload.GuardrailDigest {
		return ApplyResult{Status: "refused"}, errors.New("artifact guardrail bundle binding mismatch")
	}
	mutation, err := s.Mutation(v)
	if err != nil {
		return ApplyResult{Status: "refused"}, err
	}
	in.Mutation = mutation
	_, err = s.Guardrail.Apply(ctx, in)
	if err != nil {
		result := ApplyResult{}
		if errors.Is(err, executor.ErrPartial) {
			result.Status = "partial_failure"
			if applied, ok := mutation.(*executor.PostgreSQL); ok {
				result.AppliedSteps = applied.Result().AppliedSteps
			}
		}
		return result, err
	}
	appliedSteps := len(verifiedPayload.Plan.Steps)
	if applied, ok := mutation.(*executor.PostgreSQL); ok {
		appliedSteps = applied.Result().AppliedSteps
	}
	status := "success"
	if appliedSteps == 0 {
		status = "no_op"
	}
	return ApplyResult{Status: status, AppliedSteps: appliedSteps}, nil
}
