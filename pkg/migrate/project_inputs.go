package migrate

import "autosql/pkg/artifact"

func validateProjectGeneration(r GenerateRequest) error {
	if _, exists := r.Metadata[artifact.ProjectInputsMetadata]; exists {
		return generationFailure("project_metadata_reserved", ErrGenerateConfig)
	}
	if r.ProjectInputDigest == "" && r.ExpectedProjectMigrationDigest == "" {
		return nil
	}
	if !digestRE.MatchString(r.ProjectInputDigest) || !digestRE.MatchString(r.ExpectedProjectMigrationDigest) {
		return generationFailure("project_input_identity", ErrGenerateConfig)
	}
	return nil
}
