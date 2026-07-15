package autosql_test

import (
	"encoding/json"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"autosql/pkg/postgres"
)

type featureCatalog struct {
	Version      int              `json:"version"`
	Constitution string           `json:"constitution"`
	Features     []catalogFeature `json:"features"`
}

type catalogFeature struct {
	ID               string            `json:"id"`
	Title            string            `json:"title"`
	Packages         []string          `json:"packages"`
	Docs             []string          `json:"docs"`
	Evidence         []catalogEvidence `json:"evidence"`
	PostgresKinds    []string          `json:"postgres_kinds,omitempty"`
	PostgresFeatures []string          `json:"postgres_features,omitempty"`
}

type catalogEvidence struct {
	Level   string `json:"level"`
	Path    string `json:"path"`
	Command string `json:"command"`
}

func TestFeatureCatalogCoversImplementationAndAdvertisedCapabilities(t *testing.T) {
	catalog := loadFeatureCatalog(t)
	if catalog.Version != 1 {
		t.Fatalf("catalog version=%d, want 1", catalog.Version)
	}
	requireRepositoryPath(t, catalog.Constitution)
	constitution, err := os.ReadFile(catalog.Constitution)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(constitution), "Examples and demos are part of the feature") {
		t.Fatal("constitution does not contain the examples-and-demos principle")
	}
	index, err := os.ReadFile("examples/README.md")
	if err != nil {
		t.Fatal(err)
	}

	ids := map[string]bool{}
	coveredPackages := map[string]string{}
	coveredKinds := map[string]bool{}
	coveredPostgresFeatures := map[string]bool{}
	levels := map[string]bool{"example": true, "contract": true, "integration": true, "live": true}
	for _, feature := range catalog.Features {
		if feature.ID == "" || feature.Title == "" || ids[feature.ID] {
			t.Fatalf("feature has missing or duplicate identity: %+v", feature)
		}
		ids[feature.ID] = true
		if !strings.Contains(string(index), "<!-- feature:"+feature.ID+" -->") {
			t.Errorf("examples/README.md is missing feature %q", feature.ID)
		}
		if len(feature.Packages) == 0 || len(feature.Docs) == 0 || len(feature.Evidence) == 0 {
			t.Errorf("feature %q must map packages, docs, and evidence", feature.ID)
		}
		for _, packagePath := range feature.Packages {
			requireRepositoryPath(t, packagePath)
			if owner := coveredPackages[packagePath]; owner != "" {
				t.Errorf("package %q is claimed by both %q and %q", packagePath, owner, feature.ID)
			}
			coveredPackages[packagePath] = feature.ID
		}
		for _, doc := range feature.Docs {
			requireRepositoryPath(t, doc)
			if filepath.Ext(doc) != ".md" {
				t.Errorf("feature %q documentation %q is not Markdown", feature.ID, doc)
			}
		}
		for _, evidence := range feature.Evidence {
			if !levels[evidence.Level] || strings.TrimSpace(evidence.Command) == "" {
				t.Errorf("feature %q has invalid evidence: %+v", feature.ID, evidence)
			}
			requireRepositoryPath(t, evidence.Path)
		}
		for _, kind := range feature.PostgresKinds {
			coveredKinds[kind] = true
		}
		for _, capability := range feature.PostgresFeatures {
			coveredPostgresFeatures[capability] = true
		}
	}

	compareSets(t, "production packages", productionPackageDirectories(t), coveredPackagesAsSet(coveredPackages))
	advertisedKinds, advertisedFeatures := postgresCapabilitySets()
	compareSets(t, "PostgreSQL resource kinds", advertisedKinds, coveredKinds)
	compareSets(t, "PostgreSQL feature flags", advertisedFeatures, coveredPostgresFeatures)
}

func loadFeatureCatalog(t *testing.T) featureCatalog {
	t.Helper()
	file, err := os.Open("examples/catalog.json")
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	decoder := json.NewDecoder(file)
	decoder.DisallowUnknownFields()
	var catalog featureCatalog
	if err := decoder.Decode(&catalog); err != nil {
		t.Fatal(err)
	}
	return catalog
}

func requireRepositoryPath(t *testing.T, path string) {
	t.Helper()
	clean := filepath.Clean(path)
	if path == "" || filepath.IsAbs(path) || clean != path || clean == "." || strings.HasPrefix(clean, "..") {
		t.Errorf("catalog path is not a clean repository-relative path: %q", path)
		return
	}
	if _, err := os.Stat(path); err != nil {
		t.Errorf("catalog path %q: %v", path, err)
	}
}

func productionPackageDirectories(t *testing.T) map[string]bool {
	t.Helper()
	packages := map[string]bool{}
	for _, root := range []string{"cmd", "internal", "pkg"} {
		err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
			if err != nil {
				return err
			}
			if entry.IsDir() {
				if entry.Name() == "testdata" || strings.HasPrefix(entry.Name(), ".") {
					return filepath.SkipDir
				}
				return nil
			}
			if filepath.Ext(path) == ".go" && !strings.HasSuffix(path, "_test.go") {
				packages[filepath.ToSlash(filepath.Dir(path))] = true
			}
			return nil
		})
		if err != nil {
			t.Fatal(err)
		}
	}
	return packages
}

func postgresCapabilitySets() (map[string]bool, map[string]bool) {
	kinds, features := map[string]bool{}, map[string]bool{}
	for _, capability := range postgres.New().Info().Capabilities {
		kinds[string(capability.Kind)] = true
		for _, feature := range capability.Features {
			features[feature] = true
		}
	}
	return kinds, features
}

func coveredPackagesAsSet(packages map[string]string) map[string]bool {
	set := make(map[string]bool, len(packages))
	for path := range packages {
		set[path] = true
	}
	return set
}

func compareSets(t *testing.T, label string, expected, actual map[string]bool) {
	t.Helper()
	missing, extra := []string{}, []string{}
	for item := range expected {
		if !actual[item] {
			missing = append(missing, item)
		}
	}
	for item := range actual {
		if !expected[item] {
			extra = append(extra, item)
		}
	}
	sort.Strings(missing)
	sort.Strings(extra)
	if len(missing) != 0 || len(extra) != 0 {
		t.Errorf("%s catalog mismatch: missing=%v extra=%v", label, missing, extra)
	}
}
