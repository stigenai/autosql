package cli

import (
	"context"
	"errors"

	"autosql/pkg/artifact"
	"autosql/pkg/executor"
	"autosql/pkg/simulate"
)

// A project secret may rotate without changing its reference. Compare the
// actual execution connection with the server identity signed by generation.
func verifyProjectRuntimeTarget(ctx context.Context, a artifact.Artifact, session executor.Session) error {
	if _, present := a.Metadata[artifact.ProjectInputsMetadata]; !present {
		return nil
	}
	actual, err := simulate.ResolvePostgresConnectionIdentity(ctx, session.Raw())
	if err != nil {
		return errors.New("project execution target identity cannot be verified")
	}
	for _, att := range a.ValidationAttestations {
		if att.Stage == "replay_simulation" && att.Simulation != nil && att.Simulation.TargetIdentity == actual {
			return nil
		}
	}
	return errors.New("project execution target differs from the validated release target")
}
