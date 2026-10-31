package cli

import (
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"autosql/pkg/config"
	"autosql/pkg/secret"
)

// Reports may replace an ordinary previous report, but never project inputs or
// referenced credential files, including through a symlinked parent directory.
func validateTestReportPath(c *config.Config, options projectTestArgs, inputs []config.InputFile) error {
	if options.junit == "" {
		return nil
	}
	destination, err := canonicalTestPath(options.junit)
	if err != nil {
		return errTestReportWrite
	}
	root := filepath.Dir(options.project)
	protected := []string{options.project}
	for _, input := range inputs {
		protected = append(protected, filepath.Join(root, input.Path))
	}
	for _, environment := range c.Environments {
		protected = append(protected, testCredentialPaths(root, environment.Target, environment.DevDatabase)...)
		protected = append(protected, filepath.Join(root, environment.MigrationDir))
		for _, source := range environment.SchemaSources {
			protected = append(protected, filepath.Join(root, source.Path))
		}
		for _, name := range append(append([]string{}, environment.PolicyFiles...), environment.Registry.Lock) {
			if name != "" {
				protected = append(protected, filepath.Join(root, name))
			}
		}
	}
	for _, factory := range c.DevFactories {
		protected = append(protected, testCredentialPaths(root, factory.Admin)...)
	}
	return rejectProtectedTestReport(destination, protected)
}

func testCredentialPaths(root string, references ...secret.Reference) []string {
	var result []string
	for _, ref := range references {
		u, err := url.Parse(string(ref))
		if err != nil || u.Scheme != "file" {
			continue
		}
		name := u.Path
		if !filepath.IsAbs(name) {
			name = filepath.Join(root, name)
		}
		result = append(result, name)
	}
	return result
}

func rejectProtectedTestReport(destination string, protected []string) error {
	for _, name := range protected {
		resolved, err := canonicalTestPath(name)
		if err != nil {
			return errTestReportWrite
		}
		if destination == resolved || strings.HasPrefix(destination, resolved+string(filepath.Separator)) {
			return &Error{Kind: "config", Code: ExitConfig, Message: "JUnit destination overlaps project inputs or credentials"}
		}
	}
	return nil
}

func canonicalTestPath(name string) (string, error) {
	name, err := filepath.Abs(name)
	if err != nil {
		return "", err
	}
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(name)
		if err == nil {
			for i := len(suffix) - 1; i >= 0; i-- {
				resolved = filepath.Join(resolved, suffix[i])
			}
			return resolved, nil
		}
		if !errors.Is(err, os.ErrNotExist) || filepath.Dir(name) == name {
			return "", err
		}
		suffix = append(suffix, filepath.Base(name))
		name = filepath.Dir(name)
	}
}
