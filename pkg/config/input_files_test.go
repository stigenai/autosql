package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"autosql/pkg/source"
)

func projectInputFixture(t *testing.T, ctx context.Context) (*Config, *InputFiles) {
	t.Helper()
	path := writeProject(t, "autosql.json", projectJSON())
	c, err := Load(path, noProjectEnv, Overrides{})
	if err != nil {
		t.Fatal(err)
	}
	inputs, err := c.OpenInputFiles(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := inputs.Close(); err != nil {
			t.Error(err)
		}
	})
	return c, inputs
}

func writeProjectInput(t *testing.T, root, name, content string) {
	t.Helper()
	path := filepath.Join(root, name)
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestProjectInputFilesCaptureImmutableBytes(t *testing.T) {
	_, files := projectInputFixture(t, context.Background())
	writeProjectInput(t, files.RootPath(), "schema.hcl", "original input")
	first, err := files.ReadFile(filepath.Join(files.RootPath(), "schema.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	first[0] = '!'
	writeProjectInput(t, files.RootPath(), "schema.hcl", "changed after capture")
	second, err := files.ReadFile("./schema.hcl")
	if err != nil || string(second) != "original input" {
		t.Fatalf("capture was mutable: %q %v", second, err)
	}
	digest := sha256.Sum256([]byte("original input"))
	want := []InputFile{{Path: "schema.hcl", Digest: "sha256:" + hex.EncodeToString(digest[:]), Size: len("original input")}}
	if !reflect.DeepEqual(files.Records(), want) {
		t.Fatalf("wrong content binding: %+v", files.Records())
	}
	records := files.Records()
	records[0].Path = "changed"
	if !reflect.DeepEqual(files.Records(), want) {
		t.Fatal("records mutated the capture")
	}
	if err := files.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := files.ReadFile("schema.hcl"); err == nil {
		t.Fatal("closed reader served cached bytes")
	}
}

func TestProjectInputFilesConfineImportsToProject(t *testing.T) {
	_, files := projectInputFixture(t, context.Background())
	outside := t.TempDir()
	writeProjectInput(t, outside, "outside.hcl", "must-not-leak")
	if err := os.Symlink(filepath.Join(outside, "outside.hcl"), filepath.Join(files.RootPath(), "escape.hcl")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../outside.hcl", "escape.hcl", filepath.Join(outside, "outside.hcl"), "env://PRODUCTION_TARGET"} {
		if _, err := files.ReadFile(name); err == nil || strings.Contains(err.Error(), "must-not-leak") {
			t.Fatalf("unsafe input boundary for %q: %v", name, err)
		}
	}
	writeProjectInput(t, files.RootPath(), "inside.hcl", "inside")
	if err := os.Symlink("inside.hcl", filepath.Join(files.RootPath(), "alias.hcl")); err != nil {
		t.Fatal(err)
	}
	if raw, err := files.ReadFile("alias.hcl"); err != nil || string(raw) != "inside" {
		t.Fatalf("contained relative link: %q %v", raw, err)
	}
}

func TestProjectInputFilesRejectFIFOAndOversize(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	_, files := projectInputFixture(t, ctx)
	if err := syscall.Mkfifo(filepath.Join(files.RootPath(), "blocked.hcl"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := files.ReadFile("blocked.hcl"); err == nil {
		t.Fatal("FIFO accepted as an input file")
	}
	large, err := os.Create(filepath.Join(files.RootPath(), "huge.hcl"))
	if err != nil {
		t.Fatal(err)
	}
	if err := large.Truncate(maxInputFileBytes + 1); err != nil {
		t.Fatal(err)
	}
	if err := large.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := files.ReadFile("huge.hcl"); err == nil || !strings.Contains(err.Error(), "32 MiB") {
		t.Fatalf("oversize input: %v", err)
	}
	if len(files.Records()) != 0 {
		t.Fatal("refused inputs entered the manifest")
	}
}

func TestProjectInputFilesCancelAndConcurrentReads(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	c, files := projectInputFixture(t, ctx)
	writeProjectInput(t, files.RootPath(), "schema.hcl", "input")
	var workers sync.WaitGroup
	for range 8 {
		workers.Add(1)
		go func() {
			defer workers.Done()
			raw, err := files.ReadFile("schema.hcl")
			if err != nil || !bytes.Equal(raw, []byte("input")) {
				t.Errorf("parallel capture: %q %v", raw, err)
				return
			}
			raw[0] = '!'
		}()
	}
	workers.Wait()
	if len(files.Records()) != 1 {
		t.Fatal("same input counted repeatedly")
	}
	cancel()
	if _, err := files.ReadFile("schema.hcl"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cached input ignored cancellation: %v", err)
	}
	if _, err := c.OpenInputFiles(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("opening input ignored cancellation: %v", err)
	}
}

func TestProjectInputFilesCaptureExistingHCLModuleLoader(t *testing.T) {
	_, files := projectInputFixture(t, context.Background())
	writeProjectInput(t, files.RootPath(), "schema.hcl", `module "accounts" { source="accounts" }`)
	writeProjectInput(t, files.RootPath(), "accounts/schema.hcl", `schema "demo" {}`)
	writeProjectInput(t, files.RootPath(), "accounts/table.hcl", `table "accounts" { schema="demo" }`)
	loader := source.HCLLoader{ReadFile: files.ReadFile, ReadDir: files.ReadDir}
	doc, err := loader.Load(context.Background(), filepath.Join(files.RootPath(), "schema.hcl"))
	if err != nil || len(doc.Graph.Resources) != 2 {
		t.Fatalf("existing module loader: %v resources=%d", err, len(doc.Graph.Resources))
	}
	var names []string
	for _, record := range files.Records() {
		names = append(names, record.Path)
	}
	if !reflect.DeepEqual(names, []string{"accounts/schema.hcl", "accounts/table.hcl", "schema.hcl"}) {
		t.Fatalf("module input capture: %v", names)
	}
}
