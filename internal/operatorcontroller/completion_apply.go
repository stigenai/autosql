package operatorcontroller

import (
	"context"
	"errors"
	"reflect"
	"strings"

	"autosql/internal/cli"
	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/operator"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
)

// ObserveArtifactCompletion inspects an already executed release and records
// the observation. It has no route to ArtifactApply or a mutating executor.
func ObserveArtifactCompletion(ctx context.Context, resource operator.Resource) operator.CompletionResult {
	prepared, err := prepareArtifactCompletion(ctx, resource)
	if err != nil {
		return completionRefusal("authorization_invalid")
	}
	config := prepared.Database
	config.URL = resource.ResolvedDatabaseURL
	evidence := executor.ObservePostgreSQLCompletion(ctx, prepared.Artifact, config)
	if evidence.Outcome != executor.CompletionProven {
		return operator.CompletionResult{Evidence: evidence}
	}
	// Configuration and lifetimes can change while database evidence is read.
	// Reauthenticate before recording or publishing any successful result.
	fresh, err := prepareArtifactCompletion(ctx, resource)
	if err != nil || !sameCompletionConfiguration(prepared, fresh) {
		return completionRefusal("authorization_invalid")
	}
	if err := fresh.Audit.AppendCompletionObservation(ctx, fresh.Artifact, evidence, resource.UID, resource.MetadataGeneration); err != nil {
		if errors.Is(err, executor.ErrAuditEvidence) {
			return completionRefusal("audit_invalid")
		}
		return operator.CompletionResult{Evidence: executor.CompletionEvidence{Outcome: executor.CompletionRetryable, Reason: "audit_unavailable"}}
	}
	return operator.CompletionResult{Evidence: evidence, AuthorizationExpiresAt: fresh.ValidUntil, EventID: executor.CompletionObservationID(resource.UID, resource.MetadataGeneration, evidence.ArtifactDigest, evidence.Attempt)}
}

// RevalidateArtifactCompletion is the credential-free publication check.
func RevalidateArtifactCompletion(ctx context.Context, resource operator.Resource) error {
	_, err := prepareArtifactCompletion(ctx, resource)
	return err
}

func completionRefusal(reason string) operator.CompletionResult {
	return operator.CompletionResult{Evidence: executor.CompletionEvidence{Outcome: executor.CompletionRefused, Reason: reason}}
}

func prepareArtifactCompletion(ctx context.Context, resource operator.Resource) (cli.PreparedCompletion, error) {
	spec := resource.Spec
	if spec.Suspend || !ordinaryCompletionResource(resource) || resource.UID == "" || resource.MetadataGeneration < 1 {
		return cli.PreparedCompletion{}, cli.ErrCompletionAuthorization
	}
	a, err := loadOperatorArtifact(spec.ArtifactDigest)
	if err != nil {
		return cli.PreparedCompletion{}, cli.ErrCompletionAuthorization
	}
	prepared, err := cli.PrepareProductionCompletion(ctx, a)
	if err != nil {
		return cli.PreparedCompletion{}, err
	}
	if err := completionSourceMatches(ctx, resource, a); err != nil {
		return cli.PreparedCompletion{}, cli.ErrCompletionAuthorization
	}
	return prepared, nil
}

func completionSourceMatches(ctx context.Context, resource operator.Resource, a artifact.Artifact) error {
	if resource.Spec.Source.RegistryDigest != "" {
		if resource.Spec.Source.RegistryDigest == a.Digest {
			return nil
		}
		return cli.ErrCompletionAuthorization
	}
	if strings.TrimSpace(resource.ResolvedSource) == "" {
		return cli.ErrCompletionAuthorization
	}
	desired, err := loadOperatorDeclarativeSource(ctx, resource.ResolvedSource, resource.Spec.Source.Format)
	if err != nil {
		return err
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		return err
	}
	// Ordinary transition publishing excludes the database declaration from
	// the schema fingerprint. Preserve its exact target identity check here.
	resources := make([]schema.Resource, 0, len(desired.Graph.Resources))
	databases := 0
	for _, item := range desired.Graph.Resources {
		if item.Kind != schema.KindDatabase {
			resources = append(resources, item)
			continue
		}
		databases++
		target, err := postgres.DatabaseTargetFromResource(item)
		if err != nil || databases > 1 || !artifactIdentityMatchesBootstrapTarget(a.DatabaseIdentity, target) {
			return cli.ErrCompletionAuthorization
		}
	}
	desired.Graph.Resources = resources
	fingerprint, err := schema.SemanticFingerprint(desired)
	if err != nil || !strings.EqualFold(fingerprint, a.Plan.ToFingerprint) {
		return cli.ErrCompletionAuthorization
	}
	return nil
}

func sameCompletionConfiguration(a, b cli.PreparedCompletion) bool {
	x, y := a.Database, b.Database
	return a.Artifact.Digest() == b.Artifact.Digest() && a.Audit.Path == b.Audit.Path && x.DatabaseIdentity == y.DatabaseIdentity && x.Environment == y.Environment && x.SourceRevision == y.SourceRevision && reflect.DeepEqual(x.Schemas, y.Schemas) && reflect.DeepEqual(x.Managed, y.Managed)
}

func ordinaryCompletionResource(resource operator.Resource) bool {
	s := resource.Spec
	return s.Kind == operator.Declarative && s.AdoptionPolicy == "" && !s.CreateDatabase && s.DatabaseTarget == nil && s.MaintenanceDatabaseURL == nil && s.BootstrapAuthority == nil && s.BootstrapAuthorization == nil
}

func verifyOperatorCompletionRelease(ctx context.Context, digest string) error {
	a, err := loadOperatorArtifact(digest)
	if err != nil {
		return cli.ErrCompletionAuthorization
	}
	_, err = cli.PrepareProductionCompletion(ctx, a)
	return err
}

// ProbeArtifactExecution decides whether the core must observe prior execution.
// An unstarted result never bypasses ArtifactApply or its normal authorization.
func ProbeArtifactExecution(ctx context.Context, resource operator.Resource) executor.ExecutionStartCheck {
	refused := executor.ExecutionStartCheck{Failure: executor.CompletionEvidence{Outcome: executor.CompletionRefused, Reason: "authorization_invalid"}}
	if resource.Spec.Suspend || !ordinaryCompletionResource(resource) {
		return refused
	}
	a, err := resource.VerifiedReleaseArtifact.Payload()
	if err != nil {
		a, err = loadOperatorArtifact(resource.Spec.ArtifactDigest)
	}
	if err != nil || a.Digest != resource.Spec.ArtifactDigest {
		return refused
	}
	verified, config, err := cli.PrepareProductionExecutionCheck(a)
	// URL sources retain their existing apply behavior; this check does not
	// fetch them. Recovery still refuses an unverifiable desired source.
	if err != nil || resource.Spec.Source.URL == "" && completionSourceMatches(ctx, resource, a) != nil {
		return refused
	}
	config.URL = resource.ResolvedDatabaseURL
	config.RequireExistingAudit = resource.ExecutionMayHaveStarted
	return executor.CheckPostgreSQLExecutionStart(ctx, verified, config)
}
