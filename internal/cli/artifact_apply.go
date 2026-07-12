package cli

import (
	"context"
	"errors"
	"os"
	"reflect"

	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/guardrail"
	"autosql/pkg/migrate/repair"
	"autosql/pkg/migrate/revision"
)

// VerifiedArtifactApplyService is the only CLI bridge to mutation. Callers must
// provide trusted verification policy, the exact bound guardrail input, and an
// executor mutation factory. No raw plan or SQL execution path exists here.
type VerifiedArtifactApplyService struct {
	Policy              artifact.VerifyPolicy
	PolicyFor           func(artifact.Artifact) (artifact.VerifyPolicy, error)
	Guardrail           guardrail.Guardrail
	Input               func(artifact.Artifact) (guardrail.Input, error)
	Mutation            func(artifact.VerifiedArtifact) (guardrail.AuthorizedMutation, error)
	MutationLocked      func(artifact.VerifiedArtifact, executor.Session, executor.Tx) (guardrail.AuthorizedMutation, error)
	NoEdits             bool
	LifecycleAudit      executor.LifecycleAudit
	RepairAuthorization func(context.Context, repair.Proposal, revision.Revision) error
}

func (s VerifiedArtifactApplyService) AuthorizeRepair(ctx context.Context, p repair.Proposal, r revision.Revision) error {
	if s.RepairAuthorization == nil {
		return errors.New("production repair authorization is not configured")
	}
	return s.RepairAuthorization(ctx, p, r)
}

func (s VerifiedArtifactApplyService) DrainLifecycle(ctx context.Context, e executor.LifecycleEvent) error {
	if s.LifecycleAudit == nil {
		return errors.New("durable lifecycle audit is not configured")
	}
	return s.LifecycleAudit.AppendDurable(ctx, e)
}

func (s VerifiedArtifactApplyService) ApplyVersioned(ctx context.Context, v artifact.VerifiedArtifact, session executor.Session, tx executor.Tx) (executor.ExternalExecution, error) {
	var out executor.ExternalExecution
	p, err := v.Payload()
	if err != nil || s.Input == nil || s.MutationLocked == nil {
		return out, errors.New("versioned guarded apply is not configured")
	}
	in, err := s.Input(p)
	if err != nil {
		return out, err
	}
	mutation, err := s.MutationLocked(v, session, tx)
	if err != nil {
		return out, err
	}
	in.Mutation = mutation
	if _, err = s.Guardrail.Apply(ctx, in); err != nil {
		if x, ok := mutation.(*executor.PostgreSQL); ok {
			if tx != nil {
				out = x.ExternalExecution()
			} else {
				out.Result = x.Result()
			}
		}
		return out, err
	}
	if x, ok := mutation.(*executor.PostgreSQL); ok {
		if tx != nil {
			out = x.ExternalExecution()
		} else {
			out.Result = x.Result()
		}
	}
	return out, nil
}

// VerifyArtifact exposes exactly the same trusted release-manifest policy used
// by the single-artifact path, without granting a mutation capability.
func (s VerifiedArtifactApplyService) VerifyArtifact(a artifact.Artifact) (artifact.VerifiedArtifact, error) {
	p := s.Policy
	var err error
	if s.PolicyFor != nil {
		p, err = s.PolicyFor(a)
		if err != nil {
			return artifact.VerifiedArtifact{}, err
		}
	}
	if s.NoEdits {
		p.NoEdits = true
	}
	return a.VerifyTrusted(p)
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
	verifyPolicy := s.Policy
	if s.PolicyFor != nil {
		verifyPolicy, err = s.PolicyFor(a)
		if err != nil {
			return ApplyResult{Status: "refused"}, err
		}
	}
	if s.NoEdits || request.NoEdits {
		verifyPolicy.NoEdits = true
	}
	v, err := a.VerifyTrusted(verifyPolicy)
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
		if applied, ok := mutation.(*executor.PostgreSQL); ok {
			r := applied.Result()
			result.PendingStep = r.PendingStep
			result.ExecutionID = r.ExecutionID
			result.RecoveryGuidance = r.RecoveryGuidance
			if r.Uncertain {
				result.Status = "uncertain"
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
