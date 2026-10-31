package cli

import (
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/guardrail"
)

func verifyProjectApprovalLifetime(a artifact.Artifact, c applyConfig, trust migrationTrust) error {
	if !projectSignedApproval(c, trust) {
		return nil // legacy/precomputed trusted approval configuration
	}
	_, signed, err := completionApproval(a, c, trust)
	now := time.Now().UTC()
	if err != nil || signed.ApprovedAt.After(now) || !signed.ExpiresAt.After(now) {
		return approval.ErrDenied
	}
	return nil
}

func projectApprovalGuardrail(a artifact.Artifact, c applyConfig, trust migrationTrust, g guardrail.Guardrail) guardrail.Guardrail {
	if !projectSignedApproval(c, trust) {
		return g
	}
	identity, signed, err := completionApproval(a, c, trust)
	if err != nil {
		g.Approval.Authority = completionAuthority{} // refuses every actor/proof
		return g
	}
	g.Approval.Authority = completionAuthority{actors: map[string]approval.Identity{c.Author: {ID: c.Author}, c.Requester: {ID: c.Requester}, identity.ID: identity}, proof: trust.ApprovalEvidence[0].Proof, signed: signed}
	return g
}

func projectSignedApproval(c applyConfig, trust migrationTrust) bool {
	return trust.Project != nil && (len(trust.ApprovalEvidence) != 0 || c.AutomationApprovalKeyID != "" || c.AutomationApprovalPublicKey != "")
}
