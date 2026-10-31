package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"autosql/pkg/migrate"
)

const InputManifestVersion = "autosql.project-inputs/v1"

var inputDigestPattern = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)

type MigrationInputs struct {
	Directory      string `json:"directory"`
	ManifestDigest string `json:"manifest_digest"`
	Count          int    `json:"count"`
	// Paths are logical migration paths; publication stores these bytes in an
	// immutable generation. ManifestDigest binds their order and metadata.
	Files []InputFile `json:"files"`
}

// InputManifest describes captured project input bytes, not authorization.
// ExecutionContextDigest is supplied by the command from its non-secret,
// execution-relevant options and must be recomputed by trusted verification.
type InputManifest struct {
	Version                string          `json:"version"`
	Configuration          ProjectIdentity `json:"configuration"`
	Files                  []InputFile     `json:"files"`
	Migrations             MigrationInputs `json:"migrations"`
	ExecutionContextDigest string          `json:"execution_context_digest"`
	EffectiveInputDigest   string          `json:"effective_input_digest"`
}

// CaptureInputManifest completes a source loader's capture with the selected
// test, policy, lock, and verified migration inputs. No target is resolved.
// A nil prefix captures the whole migration generation; a non-nil count
// reconstructs a verified pre-generation prefix during release verification.
func (c *Config) CaptureInputManifest(files *InputFiles, prefix *int, executionDigest string) (InputManifest, error) {
	identity, err := c.Identity()
	if err != nil {
		return InputManifest{}, err
	}
	if files == nil || files.identity != identity || !inputDigestPattern.MatchString(executionDigest) {
		return InputManifest{}, c.problem("/inputs", "input capture or execution context does not match the project")
	}
	if err := c.requireCapturedSources(files); err != nil {
		return InputManifest{}, err
	}
	if err := c.captureReferencedInputs(files); err != nil {
		return InputManifest{}, err
	}
	migrations, err := c.captureMigrationInputs(files, prefix)
	if err != nil {
		return InputManifest{}, err
	}
	if err := files.ctx.Err(); err != nil {
		return InputManifest{}, err
	}
	result := InputManifest{Version: InputManifestVersion, Configuration: identity, Files: files.Records(), Migrations: migrations, ExecutionContextDigest: executionDigest}
	return c.finishInputManifest(result)
}

func (c *Config) finishInputManifest(result InputManifest) (InputManifest, error) {
	if err := result.checkLimits(); err != nil {
		return InputManifest{}, c.problem("/inputs", "effective inputs exceed 4096 files or 128 MiB")
	}
	if err := result.setDigest(); err != nil {
		return InputManifest{}, c.problem("/inputs", "cannot encode effective input identity")
	}
	return result, nil
}

func (m InputManifest) checkLimits() error {
	if len(m.Files)+len(m.Migrations.Files) > maxInputFiles {
		return os.ErrInvalid
	}
	total := 0
	for _, input := range append(append([]InputFile(nil), m.Files...), m.Migrations.Files...) {
		total += input.Size
		if total > maxInputTotalBytes {
			return os.ErrInvalid
		}
	}
	return nil
}

func (m *InputManifest) setDigest() error {
	// Reporting preferences affect Configuration.ConfigDigest, but must not
	// invalidate approval when only a report destination moves.
	material := *m
	material.EffectiveInputDigest = ""
	material.Configuration.ConfigDigest = ""
	raw, err := json.Marshal(material)
	if err != nil {
		return err
	}
	digest := sha256.Sum256(append([]byte(InputManifestVersion+"\x00"), raw...))
	m.EffectiveInputDigest = "sha256:" + hex.EncodeToString(digest[:])
	return nil
}

func (c *Config) requireCapturedSources(files *InputFiles) error {
	captured := map[string]bool{}
	for _, input := range files.Records() {
		captured[input.Path] = true
	}
	for index, input := range c.Effective().Values.SchemaSources {
		if !captured[input.Path] {
			return c.problem(fmt.Sprintf("%s/schema_sources/%d", environmentPath(c.Environment), index), "schema loader must capture every configured source before input identity")
		}
	}
	return nil
}

func (c *Config) captureReferencedInputs(files *InputFiles) error {
	e := c.Effective().Values
	if err := c.captureTestInputs(files, e.Tests.Files); err != nil {
		return err
	}
	names := append([]string(nil), e.PolicyFiles...)
	if e.Registry.Lock != "" {
		names = append(names, e.Registry.Lock)
	}
	for _, name := range names {
		if _, err := files.ReadFile(name); err != nil {
			return err
		}
	}
	return nil
}

func (c *Config) captureTestInputs(files *InputFiles, patterns []string) error {
	for index, pattern := range patterns {
		names, err := files.Glob(pattern)
		if files.ctx.Err() != nil {
			return files.ctx.Err()
		}
		if err != nil || len(names) == 0 {
			return c.problem(fmt.Sprintf("%s/tests/files/%d", environmentPath(c.Environment), index), "test pattern must match available project input files")
		}
		for _, name := range names {
			if _, err := files.ReadFile(name); err != nil {
				return err
			}
		}
	}
	return nil
}

func (c *Config) captureMigrationInputs(files *InputFiles, prefix *int) (MigrationInputs, error) {
	name := c.Effective().Values.MigrationDir
	directory, err := files.OpenDirectory(name)
	if err != nil {
		return MigrationInputs{}, err
	}
	defer directory.Close()
	snapshot, err := migrate.InspectSnapshot(directory)
	if err == nil && prefix != nil {
		snapshot, err = migrate.PrefixSnapshot(snapshot, *prefix)
	}
	if err != nil {
		return MigrationInputs{}, c.problem(environmentPath(c.Environment)+"/migration_dir", "migration snapshot cannot be verified")
	}
	result := MigrationInputs{Directory: name, ManifestDigest: snapshot.Manifest.Digest, Count: len(snapshot.Manifest.Entries), Files: []InputFile{}}
	for _, entry := range snapshot.Manifest.Entries {
		result.Files = append(result.Files, migrationInputFile(name, entry.File, snapshot.Files[entry.File]))
		if entry.ArtifactFile != "" {
			result.Files = append(result.Files, migrationInputFile(name, entry.ArtifactFile, snapshot.Files[entry.ArtifactFile]))
		}
	}
	return result, nil
}

func migrationInputFile(directory, name string, raw []byte) InputFile {
	digest := sha256.Sum256(raw)
	return InputFile{Path: filepath.ToSlash(filepath.Join(directory, name)), Digest: "sha256:" + hex.EncodeToString(digest[:]), Size: len(raw)}
}

// Glob expands ordinary per-component filepath.Match patterns within the
// project. It has no recursive ** extension and refuses unmatched parents,
// escaping links, malformed patterns, and more than 4096 matching paths.
func (f *InputFiles) Glob(pattern string) ([]string, error) {
	if !localProjectPath(pattern) || strings.Contains(pattern, "**") {
		return nil, f.failure("test pattern must use project-relative component globs")
	}
	paths := []string{"."}
	components := strings.Split(normalizedPath(pattern), "/")
	for index, component := range components {
		if _, err := filepath.Match(component, ""); err != nil {
			return nil, f.failure("invalid input pattern")
		}
		var err error
		paths, err = f.globComponent(paths, component, index < len(components)-1)
		if err != nil {
			return nil, err
		}
	}
	return paths, nil
}

func (f *InputFiles) globComponent(parents []string, component string, needDirectory bool) ([]string, error) {
	matched := map[string]bool{}
	for _, parent := range parents {
		entries, err := f.ReadDir(parent)
		if err != nil {
			return nil, err
		}
		for _, entry := range entries {
			ok, _ := filepath.Match(component, entry.Name())
			if needDirectory && !entry.IsDir() && entry.Type()&os.ModeSymlink == 0 {
				continue
			}
			if ok {
				matched[filepath.ToSlash(filepath.Join(parent, entry.Name()))] = true
			}
			if len(matched) > maxInputFiles {
				return nil, f.failure("input pattern exceeds 4096 matching paths")
			}
		}
	}
	return sortedKeys(matched), nil
}
