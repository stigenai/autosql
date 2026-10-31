package cli

import (
	"context"
	"crypto/ed25519"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/migrate"
)

// PreparedCompletion is inspection-only input from the current trusted release
// configuration. Its database URL remains empty until the controller resolves
// the resource reference. It grants no application execution capability.
type PreparedCompletion struct {
	Artifact   artifact.VerifiedObservation
	Database   executor.PostgreSQLCompletionConfig
	ValidUntil time.Time
	Audit      *executor.FileAudit
}

var ErrCompletionAuthorization = errors.New("completion release authorization invalid")

func PrepareProductionCompletion(ctx context.Context, a artifact.Artifact) (PreparedCompletion, error) {
	services, err := productionVerificationServices()
	if err != nil {
		return PreparedCompletion{}, ErrCompletionAuthorization
	}
	service, ok := services.Apply.(resolvingApply)
	if !ok || service.completionConfig == nil {
		return PreparedCompletion{}, ErrCompletionAuthorization
	}
	prepared, err := prepareProductionCompletion(ctx, a, *service.completionConfig, service.verified)
	if err != nil {
		return PreparedCompletion{}, ErrCompletionAuthorization
	}
	return prepared, nil
}

func prepareProductionCompletion(ctx context.Context, a artifact.Artifact, c applyConfig, service VerifiedArtifactApplyService) (PreparedCompletion, error) {
	verified, err := service.VerifyArtifactForObservation(a, true)
	if err != nil {
		return PreparedCompletion{}, err
	}
	a, err = verified.Payload()
	if err != nil {
		return PreparedCompletion{}, err
	}
	trust, ok := c.TrustedMigrations[a.Digest]
	if !ok || len(trust.Schemas) == 0 || c.LifecycleAuditPath == "" {
		return PreparedCompletion{}, ErrCompletionAuthorization
	}
	identity, signed, err := completionApproval(a, c, trust)
	if err != nil {
		return PreparedCompletion{}, err
	}
	in, err := service.Input(a)
	if err != nil || in.Approval.Override != nil {
		return PreparedCompletion{}, ErrCompletionAuthorization
	}
	// The supported operator publisher issues one automation approval for the
	// exact bundle. Legacy identity-only manifests cannot prove its lifetime.
	in.Approval.Approvals = []approval.Approval{trust.ApprovalEvidence[0]}
	g := service.GuardrailFor(a)
	g.Approval.Authority = completionAuthority{actors: map[string]approval.Identity{c.Author: {ID: c.Author}, c.Requester: {ID: c.Requester}, identity.ID: identity}, proof: trust.ApprovalEvidence[0].Proof, signed: signed}
	if _, err = g.ObserveAuthorization(ctx, in); err != nil {
		return PreparedCompletion{}, err
	}
	database, audit, err := completionDatabaseConfig(c, trust)
	if err != nil {
		return PreparedCompletion{}, err
	}
	until := c.KeyNotAfter.UTC()
	if signed.ExpiresAt.Before(until) {
		until = signed.ExpiresAt.UTC()
	}
	for _, attestation := range a.ValidationAttestations {
		if attestation.ExpiresAt.Before(until) {
			until = attestation.ExpiresAt.UTC()
		}
	}
	for _, suppression := range trust.SafetySuppressions {
		if suppression.ExpiresAt != nil && suppression.ExpiresAt.Before(until) {
			until = suppression.ExpiresAt.UTC()
		}
	}
	if !until.After(time.Now().UTC()) {
		return PreparedCompletion{}, ErrCompletionAuthorization
	}
	return PreparedCompletion{Artifact: verified, ValidUntil: until, Audit: audit, Database: database}, nil
}

func completionApproval(a artifact.Artifact, c applyConfig, trust migrationTrust) (approval.Identity, approval.VerifiedApproval, error) {
	if len(trust.ApprovalEvidence) != 1 {
		return approval.Identity{}, approval.VerifiedApproval{}, ErrCompletionAuthorization
	}
	item := trust.ApprovalEvidence[0]
	identity, ok := trust.ApprovalIdentities[a.Approval.ProofDigest]
	public, err := base64.RawStdEncoding.Strict().DecodeString(c.AutomationApprovalPublicKey)
	digest := sha256.Sum256([]byte(item.Proof))
	if !ok || err != nil || "sha256:"+hex.EncodeToString(digest[:]) != a.Approval.ProofDigest || identity.ID != a.Approval.Identity {
		return approval.Identity{}, approval.VerifiedApproval{}, ErrCompletionAuthorization
	}
	signed, err := migrate.VerifyAutomationApproval(item, c.AutomationApprovalKeyID, ed25519.PublicKey(public), identity)
	if err != nil || signed.PlanDigest != a.GuardrailDigest || signed.Environment != a.TargetEnvironment || !signed.ApprovedAt.Equal(a.Approval.ApprovedAt) {
		return approval.Identity{}, approval.VerifiedApproval{}, ErrCompletionAuthorization
	}
	return identity, signed, nil
}

type completionAuthority struct {
	actors map[string]approval.Identity
	proof  string
	signed approval.VerifiedApproval
}

func (a completionAuthority) ResolveActor(_ context.Context, id string) (approval.Identity, error) {
	identity, ok := a.actors[id]
	if !ok {
		return approval.Identity{}, approval.ErrDenied
	}
	return identity, nil
}

func (a completionAuthority) VerifyApproval(_ context.Context, item approval.Approval) (approval.VerifiedApproval, error) {
	if item.Proof != a.proof || item.Approver != a.signed.Identity.ID {
		return approval.VerifiedApproval{}, approval.ErrDenied
	}
	return a.signed, nil
}

// PrepareProductionExecutionCheck authenticates a currently executable release
// and supplies only its trusted inspection scope. The eventual apply still
// performs all guardrail, approval, precondition, and execution checks.
func PrepareProductionExecutionCheck(a artifact.Artifact) (artifact.VerifiedArtifact, executor.PostgreSQLCompletionConfig, error) {
	services, err := productionVerificationServices()
	if err != nil {
		return artifact.VerifiedArtifact{}, executor.PostgreSQLCompletionConfig{}, ErrCompletionAuthorization
	}
	service, ok := services.Apply.(resolvingApply)
	if !ok || service.completionConfig == nil {
		return artifact.VerifiedArtifact{}, executor.PostgreSQLCompletionConfig{}, ErrCompletionAuthorization
	}
	verified, err := service.VerifyArtifactForApply(a, true)
	if err != nil {
		return artifact.VerifiedArtifact{}, executor.PostgreSQLCompletionConfig{}, ErrCompletionAuthorization
	}
	trust, ok := service.completionConfig.TrustedMigrations[a.Digest]
	if !ok {
		return artifact.VerifiedArtifact{}, executor.PostgreSQLCompletionConfig{}, ErrCompletionAuthorization
	}
	config, _, err := completionDatabaseConfig(*service.completionConfig, trust)
	return verified, config, err
}

func completionDatabaseConfig(c applyConfig, trust migrationTrust) (executor.PostgreSQLCompletionConfig, *executor.FileAudit, error) {
	if len(trust.Schemas) == 0 || c.LifecycleAuditPath == "" {
		return executor.PostgreSQLCompletionConfig{}, nil, ErrCompletionAuthorization
	}
	managed, err := managedInspectionDocument(trust.SchemaPolicyResources)
	if err != nil {
		return executor.PostgreSQLCompletionConfig{}, nil, ErrCompletionAuthorization
	}
	audit := &executor.FileAudit{Path: c.LifecycleAuditPath}
	return executor.PostgreSQLCompletionConfig{DatabaseIdentity: trust.Expected.DatabaseIdentity, Environment: trust.Expected.Environment, SourceRevision: trust.Expected.SourceRevision, Schemas: append([]string(nil), trust.Schemas...), Managed: managed, Audit: audit}, audit, nil
}
