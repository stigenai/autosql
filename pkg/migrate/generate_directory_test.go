package migrate

import (
	"os"
	"path/filepath"
	"testing"
)

func TestGenerationPinnedDirectorySurvivesAncestorReplacement(t *testing.T) {
	root := t.TempDir()
	parent, replacement := filepath.Join(root, "project"), filepath.Join(root, "outside")
	for _, p := range []string{parent, replacement} {
		if err := os.MkdirAll(filepath.Join(p, "migrations"), 0700); err != nil {
			t.Fatal(err)
		}
		if _, err := Update(filepath.Join(p, "migrations"), UpdateRequest{}); err != nil {
			t.Fatal(err)
		}
	}
	dir := filepath.Join(parent, "migrations")
	handle, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	defer handle.Close()
	request := GenerateRequest{Directory: dir, DirectoryHandle: handle}
	before, err := generationSnapshot(request)
	if err != nil {
		t.Fatal(err)
	}
	moved := filepath.Join(root, "original")
	if err := os.Rename(parent, moved); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(replacement, parent); err != nil {
		t.Fatal(err)
	}
	outsideBefore := treeState(t, replacement)
	if _, err := (GenerateService{}).publishGeneration(request, UpdateRequest{Files: []File{{Name: "V1__pinned.sql", SQL: []byte("SELECT 1;")}}, ExpectedManifestDigest: before.Manifest.Digest}); err != nil {
		t.Fatal(err)
	}
	after, err := generationSnapshot(request)
	if err != nil || len(after.Manifest.Entries) != 1 {
		t.Fatalf("pinned publication missing: %v", err)
	}
	if treeState(t, replacement) != outsideBefore {
		t.Fatal("generation wrote to the substituted outside directory")
	}
	if err := handle.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := generationSnapshot(request); err == nil {
		t.Fatal("closed pinned handle fell back to its pathname")
	}
}
