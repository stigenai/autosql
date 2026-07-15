package operatorcontroller

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"autosql/internal/cli"
	"autosql/pkg/artifact"
	"autosql/pkg/operator"
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/source"
)

// ArtifactApply resolves an immutable artifact from the mounted operator
// artifact directory and delegates to the same verified production service as
// the CLI. No raw SQL or database URL is accepted from the CR.
func ArtifactApply(ctx context.Context, resource operator.Resource, digest string) (operator.ApplyResult, error) {
	if digest == "" || !strings.HasPrefix(digest, "sha256:") {
		return operator.ApplyResult{}, errors.New("operator migration requires a sha256 artifact digest")
	}
	directory := strings.TrimSpace(os.Getenv("AUTOSQL_OPERATOR_ARTIFACT_DIR"))
	if directory == "" {
		return operator.ApplyResult{}, errors.New("AUTOSQL_OPERATOR_ARTIFACT_DIR is required")
	}
	artifactPath := filepath.Join(directory, digest+".json")
	raw, err := os.ReadFile(artifactPath)
	if err != nil {
		return operator.ApplyResult{}, errors.New("operator artifact unavailable")
	}
	a, err := artifact.Parse(raw)
	if err != nil {
		return operator.ApplyResult{}, errors.New("operator artifact invalid")
	}
	if a.Digest != digest {
		return operator.ApplyResult{}, errors.New("operator artifact digest mismatch")
	}
	if resource.Spec.Kind == operator.Declarative && resource.ResolvedSource != "" &&
		(resource.Spec.Source.Inline != "" || resource.Spec.Source.SecretRef != nil || resource.Spec.Source.ConfigMapRef != nil) {
		if err := verifyDeclarativePlan(ctx, resource.ResolvedSource, resource.ResolvedDatabaseURL, a); err != nil {
			return operator.ApplyResult{PlanDigest: a.Plan.Digest, TargetIdentity: a.DatabaseIdentity}, err
		}
	}
	if strings.TrimSpace(resource.ResolvedDatabaseURL) == "" {
		return operator.ApplyResult{}, errors.New("operator database reference is unresolved")
	}
	services, err := cli.ProductionServicesForURL(resource.ResolvedDatabaseURL)
	if err != nil {
		return operator.ApplyResult{}, fmt.Errorf("load production apply configuration: %w", err)
	}
	if services.Apply == nil {
		return operator.ApplyResult{}, errors.New("production artifact apply service is unavailable")
	}
	result, err := services.Apply.Apply(ctx, cli.ApplyRequest{
		ArtifactPath: artifactPath,
		ApprovalMode: "artifact",
		NoEdits:      true,
	})
	outcome := operator.ApplyResult{Status: result.Status, PlanDigest: a.Plan.Digest, TargetIdentity: a.DatabaseIdentity, ExecutionID: result.ExecutionID, PendingStep: result.PendingStep, RecoveryGuidance: result.RecoveryGuidance, AppliedSteps: result.AppliedSteps}
	if err != nil {
		if result.Status == "uncertain" {
			outcome.Status = "uncertain"
		}
		return outcome, err
	}
	if result.Status != "success" && result.Status != "applied" && result.Status != "no_op" {
		return outcome, fmt.Errorf("artifact apply returned status %q", result.Status)
	}
	return outcome, nil
}

// verifyDeclarativePlan makes source-to-plan generation explicit while still
// keeping the signed artifact as the only mutation input. This catches drift
// between an inline/Kubernetes-backed desired schema and the approved plan.
func verifyDeclarativePlan(ctx context.Context, desiredSQL, databaseURL string, a artifact.Artifact) error {
	desired, err := source.LoadContext(ctx, source.Input{URI: "operator:inline", Format: source.FormatSQL, Data: []byte(desiredSQL)})
	if err != nil {
		return errors.New("declarative source is invalid")
	}
	desired, err = postgres.New().Normalize(ctx, desired)
	if err != nil {
		return errors.New("declarative source normalization failed")
	}
	schemas := make([]string, 0)
	seenSchemas := map[string]bool{}
	for _, resource := range desired.Graph.Resources {
		name := resource.Name.Schema
		if resource.Kind == schema.KindSchema {
			name = resource.Name.Name
		}
		if name != "" && !seenSchemas[name] {
			seenSchemas[name] = true
			schemas = append(schemas, name)
		}
	}
	target, err := postgres.InspectURL(ctx, databaseURL, postgres.Options{Schemas: schemas})
	if err != nil {
		return errors.New("inspect declarative target")
	}
	target, err = postgres.New().Normalize(ctx, target)
	if err != nil {
		return errors.New("normalize declarative target")
	}
	generated, err := plan.Build(ctx, postgres.New(), target, desired, plan.Options{})
	if err != nil || generated.Digest != a.Plan.Digest {
		return errors.New("declarative source does not match approved plan")
	}
	return nil
}
