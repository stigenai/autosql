package config

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"autosql/pkg/migrate"
	"autosql/pkg/source"
)

var executionFixture = "sha256:" + strings.Repeat("a", 64)

func manifestProjectFixture(t *testing.T) (*Config, string) {
	t.Helper()
	path := writeProject(t, "autosql.json", projectJSON())
	root := filepath.Dir(path)
	writeProjectInput(t, root, "schema.hcl", `import "child" { source="child.hcl" }`)
	writeProjectInput(t, root, "child.hcl", `schema "demo" {}`)
	writeProjectInput(t, root, "tests/a.test.hcl", `test "a" {}`)
	writeProjectInput(t, root, "tests/b.test.hcl", `test "b" {}`)
	writeProjectInput(t, root, "policy.json", `{"rules":[]}`)
	writeProjectInput(t, root, "registry.lock", `{"version":1}`)
	if err := os.Mkdir(filepath.Join(root, "migrations"), 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := migrate.Update(filepath.Join(root, "migrations"), migrate.UpdateRequest{Files: []migrate.File{{Name: "V1__init.sql", SQL: []byte("CREATE SCHEMA demo;")}}}); err != nil {
		t.Fatal(err)
	}
	c, err := Load(path, noProjectEnv, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	return c, root
}

func captureManifestFixture(t *testing.T, c *Config, prefix *int, execution string) InputManifest {
	t.Helper()
	files, err := c.OpenInputFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	loader := source.HCLLoader{ReadFile: files.ReadFile, ReadDir: files.ReadDir}
	if _, err := loader.Load(context.Background(), filepath.Join(files.RootPath(), "schema.hcl")); err != nil {
		t.Fatal(err)
	}
	manifest, err := c.CaptureInputManifest(files, prefix, execution)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func TestEffectiveInputsCapturePortableContentAndMigrationOrder(t *testing.T) {
	c, root := manifestProjectFixture(t)
	first := captureManifestFixture(t, c, nil, executionFixture)
	other, _ := manifestProjectFixture(t)
	second := captureManifestFixture(t, other, nil, executionFixture)
	if !reflect.DeepEqual(first, second) {
		t.Fatal("relocated identical project changed effective inputs")
	}
	var names []string
	for _, file := range first.Files {
		names = append(names, file.Path)
		if strings.Contains(file.Path, root) || !inputDigestPattern.MatchString(file.Digest) {
			t.Fatalf("nonportable or unbound input: %+v", file)
		}
	}
	if !reflect.DeepEqual(names, []string{"child.hcl", "policy.json", "registry.lock", "schema.hcl", "tests/a.test.hcl", "tests/b.test.hcl"}) || first.Migrations.Count != 1 || first.Migrations.Files[0].Path != "migrations/V1__init.sql" {
		t.Fatalf("missing input inventory: %+v", first)
	}
	if !inputDigestPattern.MatchString(first.EffectiveInputDigest) || first.EffectiveInputDigest == first.Configuration.ExecutionConfigDigest {
		t.Fatal("effective inputs lack a distinct digest")
	}
}

func TestEffectiveInputsBindEveryInputClassAndExecutionContext(t *testing.T) {
	for _, name := range []string{"schema.hcl", "child.hcl", "tests/a.test.hcl", "policy.json", "registry.lock"} {
		t.Run(name, func(t *testing.T) {
			c, root := manifestProjectFixture(t)
			before := captureManifestFixture(t, c, nil, executionFixture)
			path := filepath.Join(root, name)
			raw, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(path, append(raw, '\n'), 0600); err != nil {
				t.Fatal(err)
			}
			after := captureManifestFixture(t, c, nil, executionFixture)
			if before.Configuration != after.Configuration || before.EffectiveInputDigest == after.EffectiveInputDigest {
				t.Fatal("byte edit did not affect effective input identity independently of config")
			}
		})
	}
	c, root := manifestProjectFixture(t)
	before := captureManifestFixture(t, c, nil, executionFixture)
	changedExecution := captureManifestFixture(t, c, nil, "sha256:"+strings.Repeat("b", 64))
	writeProjectInput(t, root, "tests/new.test.hcl", `test "new" {}`)
	added := captureManifestFixture(t, c, nil, executionFixture)
	if before.EffectiveInputDigest == changedExecution.EffectiveInputDigest || before.EffectiveInputDigest == added.EffectiveInputDigest {
		t.Fatal("execution options or a new matching test were not bound")
	}
}

func TestEffectiveInputsReconstructPreGenerationAndSeparateReports(t *testing.T) {
	c, root := manifestProjectFixture(t)
	before := captureManifestFixture(t, c, nil, executionFixture)
	if _, err := migrate.Update(filepath.Join(root, "migrations"), migrate.UpdateRequest{ExpectedManifestDigest: before.Migrations.ManifestDigest, Files: []migrate.File{{Name: "V1__init.sql", SQL: []byte("CREATE SCHEMA demo;")}, {Name: "V2__next.sql", SQL: []byte("CREATE TABLE demo.accounts(id int);")}}}); err != nil {
		t.Fatal(err)
	}
	after := captureManifestFixture(t, c, nil, executionFixture)
	prefix := captureManifestFixture(t, c, &before.Migrations.Count, executionFixture)
	if before.EffectiveInputDigest == after.EffectiveInputDigest || !reflect.DeepEqual(before, prefix) {
		t.Fatal("migration publication lost its pre-generation identity")
	}
	environment := c.Environments[c.Environment]
	environment.Reporting.JUnit = "other-report.xml"
	c.Environments[c.Environment] = environment
	report := captureManifestFixture(t, c, &before.Migrations.Count, executionFixture)
	if report.Configuration.ConfigDigest == before.Configuration.ConfigDigest || report.EffectiveInputDigest != before.EffectiveInputDigest {
		t.Fatal("report destination invalidated execution approval identity")
	}
}

func TestEffectiveInputsRefuseMissingMismatchedAndCanceledCaptures(t *testing.T) {
	c, _ := manifestProjectFixture(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	files, err := c.OpenInputFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	if _, err := c.CaptureInputManifest(files, nil, executionFixture); err == nil {
		t.Fatal("uncaptured desired source received an effective input digest")
	}
	if _, err := files.ReadFile("schema.hcl"); err != nil {
		t.Fatal(err)
	}
	if _, err := c.CaptureInputManifest(files, nil, "unsigned-report"); err == nil {
		t.Fatal("invalid execution context accepted")
	}
	other, _ := manifestProjectFixture(t)
	other.Environment = "missing"
	if _, err := other.CaptureInputManifest(files, nil, executionFixture); err == nil {
		t.Fatal("mismatched project received a captured identity")
	}
	cancel()
	if _, err := c.CaptureInputManifest(files, nil, executionFixture); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation was masked: %v", err)
	}
}

func TestEffectiveInputsRefuseMissingAndEscapingReferences(t *testing.T) {
	for _, name := range []string{"policy.json", "registry.lock", "tests/a.test.hcl", "migrations"} {
		t.Run(name, func(t *testing.T) {
			c, root := manifestProjectFixture(t)
			files, err := c.OpenInputFiles(context.Background())
			if err != nil {
				t.Fatal(err)
			}
			defer files.Close()
			if _, err := files.ReadFile("schema.hcl"); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(root, name)
			if err := os.RemoveAll(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(t.TempDir(), path); err != nil {
				t.Fatal(err)
			}
			if _, err := c.CaptureInputManifest(files, nil, executionFixture); err == nil {
				t.Fatal("escaping reference was accepted")
			}
		})
	}
}

func TestProjectInputGlobIsFiniteSortedAndConfined(t *testing.T) {
	c, root := manifestProjectFixture(t)
	files, err := c.OpenInputFiles(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer files.Close()
	names, err := files.Glob("*/*.test.hcl")
	if err != nil || !reflect.DeepEqual(names, []string{"tests/a.test.hcl", "tests/b.test.hcl"}) {
		t.Fatalf("component glob selected regular parents or was unordered: %v %v", names, err)
	}
	for _, pattern := range []string{"../*.hcl", "/tmp/*.hcl", "tests/[.hcl", "**/*.hcl"} {
		if _, err := files.Glob(pattern); err == nil {
			t.Fatalf("unsupported pattern accepted: %s", pattern)
		}
	}
	if err := os.Symlink(t.TempDir(), filepath.Join(root, "escape")); err != nil {
		t.Fatal(err)
	}
	if _, err := files.Glob("escape/*.test.hcl"); err == nil {
		t.Fatal("glob traversed an escaping link")
	}
}
