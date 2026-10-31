package cli

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"autosql/pkg/artifact"
	"autosql/pkg/config"
	"autosql/pkg/migrate"
	"autosql/pkg/policy"
	"autosql/pkg/schema"
)

const projectManifestMetadata = "autosql.project.manifest"

// projectReleaseVerification belongs to the trusted Apply configuration. Paths,
// overrides, and execution options are never taken from artifact metadata.
type projectReleaseVerification struct {
	Path            string                  `json:"path"`
	Overrides       config.Overrides        `json:"overrides"`
	Execution       projectReleaseExecution `json:"execution"`
	MigrationPrefix *int                    `json:"migration_prefix"`
}

type projectReleaseExecution struct {
	Version         string `json:"version"`
	Label           string `json:"label"`
	RenameHints     string `json:"rename_hints"`
	PolicyFile      string `json:"policy_file"`
	PolicyIdentity  string `json:"policy_identity"`
	PostgresVersion int    `json:"postgres_version"`
}

type capturedProjectRelease struct {
	Config    *config.Config
	Desired   schema.Document
	Policy    policy.Document
	Manifest  config.InputManifest
	Root      string
	directory *os.File
}

func (c capturedProjectRelease) close() {
	if c.directory != nil {
		_ = c.directory.Close()
	}
}

func projectReleaseFailure(message string, cause error) error {
	return &Error{Kind: "config", Message: message, Code: ExitConfig, Cause: cause}
}

func (e projectReleaseExecution) digest() (string, error) {
	if err := e.validate(); err != nil {
		return "", err
	}
	e.PolicyFile = filepath.ToSlash(filepath.Clean(e.PolicyFile))
	e.RenameHints = strings.TrimSpace(e.RenameHints)
	if e.RenameHints == "" {
		e.RenameHints = "{}"
	}
	var hints map[string]string
	if err := json.Unmarshal([]byte(e.RenameHints), &hints); err != nil {
		return "", projectReleaseFailure("project release rename hints must be canonical JSON", nil)
	}
	canonical, _ := json.Marshal(hints)
	if string(canonical) != e.RenameHints {
		return "", projectReleaseFailure("project release rename hints must be canonical JSON", nil)
	}
	raw, err := json.Marshal(e)
	if err != nil {
		return "", err
	}
	digest := sha256.Sum256(append([]byte("autosql.project-generation-context/v1\x00"), raw...))
	return "sha256:" + hex.EncodeToString(digest[:]), nil
}

func (e projectReleaseExecution) validate() error {
	if _, err := migrate.ParseVersion(e.Version); err != nil {
		return projectReleaseFailure("project release version is invalid", nil)
	}
	if e.Label == "" || strings.Trim(e.Label, "abcdefghijklmnopqrstuvwxyz0123456789_-") != "" || e.PolicyIdentity == "" || e.PostgresVersion < 14 || e.PostgresVersion > 18 {
		return projectReleaseFailure("project release execution settings are incomplete", nil)
	}
	return nil
}

func captureProjectRelease(parent context.Context, settings projectReleaseVerification) (capturedProjectRelease, error) {
	var result capturedProjectRelease
	c, err := loadProjectConfig(settings.Path, settings.Overrides.Environment, settings.Overrides)
	if err != nil {
		return result, err
	}
	if err := validateProjectReleaseConfig(c, settings.Execution); err != nil {
		return result, err
	}
	contextDigest, err := settings.Execution.digest()
	if err != nil {
		return result, err
	}
	ctx, cancel := context.WithTimeout(parent, projectTimeout(c))
	defer cancel()
	files, err := c.OpenInputFiles(ctx)
	if err != nil {
		return result, err
	}
	defer files.Close()
	desired, err := loadCapturedProjectSources(ctx, c, files)
	if err != nil {
		return result, err
	}
	document, err := readProjectReleasePolicy(c, files)
	if err != nil {
		return result, err
	}
	manifest, err := c.CaptureInputManifest(files, settings.MigrationPrefix, contextDigest)
	if err != nil {
		return result, projectReleaseFailure("capture project release inputs failed", err)
	}
	directory, err := files.OpenDirectory(manifest.Migrations.Directory)
	if err != nil {
		return result, projectReleaseFailure("open project migration directory failed", err)
	}
	return capturedProjectRelease{Config: c, Desired: desired.Document, Policy: document, Manifest: manifest, Root: files.RootPath(), directory: directory}, nil
}

func validateProjectReleaseConfig(c *config.Config, execution projectReleaseExecution) error {
	if c.Version != config.CurrentVersion {
		return projectReleaseFailure("project release binding requires version 2", nil)
	}
	e := c.Effective().Values
	if len(e.Include)+len(e.Exclude) != 0 {
		return projectReleaseFailure("project release generation requires an unfiltered schema; use project planning to preview filters", nil)
	}
	if len(e.PolicyFiles) != 1 || e.PolicyFiles[0] != filepath.ToSlash(filepath.Clean(execution.PolicyFile)) {
		return projectReleaseFailure("project release generation requires one policy file matching the trusted policy setting", nil)
	}
	return nil
}

func readProjectReleasePolicy(c *config.Config, files *config.InputFiles) (policy.Document, error) {
	raw, err := files.ReadFile(c.Effective().Values.PolicyFiles[0])
	if err != nil {
		return policy.Document{}, projectReleaseFailure("read project release policy failed", err)
	}
	document, err := policy.Parse(raw)
	if err != nil {
		return policy.Document{}, projectReleaseFailure("parse project release policy failed", nil)
	}
	return *document, nil
}

func projectReleaseMetadata(metadata map[string]string, manifest config.InputManifest) (map[string]string, error) {
	for _, name := range []string{projectManifestMetadata, artifact.ProjectInputsMetadata} {
		if _, exists := metadata[name]; exists {
			return nil, projectReleaseFailure("project release metadata is reserved", nil)
		}
	}
	result := make(map[string]string, len(metadata)+1)
	for key, value := range metadata {
		result[key] = value
	}
	raw, err := json.Marshal(manifest)
	if err != nil {
		return nil, err
	}
	result[projectManifestMetadata] = string(raw)
	return result, nil
}

func verifyCurrentProjectRelease(a artifact.Artifact, settings *projectReleaseVerification, applyConfigPath string, postgresVersion int, trustedPolicy policy.Document) (string, error) {
	claim, err := requiredProjectReleaseClaim(a, settings)
	if err != nil || claim == "" {
		return "", err
	}
	local := *settings
	if !filepath.IsAbs(local.Path) {
		local.Path = filepath.Join(filepath.Dir(applyConfigPath), local.Path)
	}
	captured, err := captureProjectRelease(context.Background(), local)
	if err != nil {
		return "", errors.New("current project release inputs cannot be verified")
	}
	defer captured.close()
	if captured.Config.Environment != a.TargetEnvironment || local.Execution.PostgresVersion != postgresVersion || captured.Manifest.EffectiveInputDigest != claim {
		return "", errors.New("current project release inputs do not match the signed release")
	}
	if err := verifyProjectReleaseManifest(a, captured.Manifest, trustedPolicy, captured.Policy); err != nil {
		return "", err
	}
	if err := verifyProjectPublishedMigration(a, captured, local); err != nil {
		return "", err
	}
	return claim, nil
}

func requiredProjectReleaseClaim(a artifact.Artifact, settings *projectReleaseVerification) (string, error) {
	claim, exists, err := a.ProjectInputDigest()
	if err != nil {
		return "", err
	}
	if !exists && settings == nil {
		return "", nil
	}
	if !exists || settings == nil {
		return "", errors.New("trusted current project verification configuration is required")
	}
	if settings.Path == "" || settings.MigrationPrefix == nil || *settings.MigrationPrefix < 0 {
		return "", errors.New("trusted project path and migration prefix are required")
	}
	return claim, nil
}

func verifyProjectReleaseManifest(a artifact.Artifact, current config.InputManifest, trustedPolicy, actualPolicy policy.Document) error {
	var signed config.InputManifest
	raw := a.Metadata[projectManifestMetadata]
	decoder := json.NewDecoder(strings.NewReader(raw))
	decoder.DisallowUnknownFields()
	err := decoder.Decode(&signed)
	canonical, _ := json.Marshal(signed)
	if err != nil || string(canonical) != raw || signed.Version != config.InputManifestVersion || signed.EffectiveInputDigest != current.EffectiveInputDigest {
		return errors.New("signed project input manifest is missing or invalid")
	}
	// Report settings may move; every execution-relevant inventory field must
	// still equal the independently reconstructed current capture.
	signed.Configuration.ConfigDigest = current.Configuration.ConfigDigest
	signedBytes, _ := json.Marshal(signed)
	currentBytes, _ := json.Marshal(current)
	wantedPolicy, _ := json.Marshal(trustedPolicy)
	currentPolicy, _ := json.Marshal(actualPolicy)
	if !bytes.Equal(signedBytes, currentBytes) || !bytes.Equal(wantedPolicy, currentPolicy) {
		return errors.New("trusted project manifest or policy inputs changed")
	}
	return nil
}

func verifyProjectPublishedMigration(a artifact.Artifact, captured capturedProjectRelease, settings projectReleaseVerification) error {
	snapshot, err := migrate.InspectSnapshot(captured.directory)
	index := *settings.MigrationPrefix
	if err != nil || index >= len(snapshot.Manifest.Entries) {
		return errors.New("project release is absent from the published migration snapshot")
	}
	entry := snapshot.Manifest.Entries[index]
	raw, err := a.MarshalCanonical()
	if err != nil || entry.File != "V"+settings.Execution.Version+"__"+settings.Execution.Label+".sql" || entry.ArtifactDigest != migrate.ArtifactDigest(raw) {
		return errors.New("published project migration does not match the signed release")
	}
	return nil
}
