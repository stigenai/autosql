package cli

import (
	"context"
	"time"

	"autosql/pkg/approval"
	"autosql/pkg/migrate"
	"autosql/pkg/secret"
)

// generationAutomationApproval reuses the signed CI approval provider. The
// key reference is trusted configuration; its resolved value is never output.
type generationAutomationApproval struct {
	KeyID, Identity, PrivateKeyReference, TTL string
	Roles                                     []string
}

func validateGenerationApproval(c generationConfig, lifetime time.Duration) error {
	a := c.AutomationApproval
	if a == nil {
		return nil
	}
	ttl, err := time.ParseDuration(a.TTL)
	if err != nil || ttl <= 0 || ttl >= lifetime || secret.Reference(a.PrivateKeyReference).Validate() != nil {
		return projectReleaseFailure("automation approval requires a valid key reference and lifetime shorter than the release", nil)
	}
	if err := validateGenerationApprovalIdentity(c); err != nil {
		return err
	}
	if len(c.Approvals) != 0 || len(c.VerifiedApprovals) != 0 {
		return projectReleaseFailure("choose automation approval or precomputed approvals", nil)
	}
	return nil
}

func validateGenerationApprovalIdentity(c generationConfig) error {
	a := c.AutomationApproval
	if a.KeyID == "" || a.KeyID == c.GeneratorKeyID || a.KeyID == c.SigningKeyID || a.Identity == "" || a.Identity == c.Author || a.Identity == c.Requester || len(a.Roles) == 0 {
		return projectReleaseFailure("automation approval requires a separate key and approver identity with roles", nil)
	}
	return nil
}

func resolveGenerationApproval(ctx context.Context, c generationConfig, resolver *secret.Resolver) (migrate.ApprovalProvider, error) {
	if c.AutomationApproval == nil {
		return nil, nil
	}
	a := c.AutomationApproval
	value, err := resolver.Resolve(ctx, secret.Reference(a.PrivateKeyReference))
	if err != nil {
		return nil, &Error{Kind: "secret", Message: "resolve automation approval key failed", Code: ExitSecret}
	}
	key, err := decodePrivate(value)
	if err != nil {
		return nil, projectReleaseFailure("decode automation approval key failed", nil)
	}
	ttl, _ := time.ParseDuration(a.TTL) // validated before resolving any secret
	return migrate.AutomationApprovalProvider{KeyID: a.KeyID, Identity: approval.Identity{ID: a.Identity, Roles: append([]string(nil), a.Roles...)}, Actors: []approval.Identity{{ID: c.Author}, {ID: c.Requester}}, PrivateKey: key, TTL: ttl}, nil
}
