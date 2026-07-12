package migrate

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestUpdateVerifyAndTamperMatrix(t *testing.T) {
	d := t.TempDir()
	if err := os.Chmod(d, 0700); err != nil {
		t.Fatal(err)
	}
	files := []File{{Name: "V1__init.sql", SQL: []byte("-- autosql:transaction=required\nCREATE TABLE t(id bigint);\n")}, {Name: "1.1__index.sql", SQL: []byte("CREATE INDEX i ON t(id);\n")}}
	m, err := Update(d, UpdateRequest{Files: files})
	if err != nil {
		t.Fatal(err)
	}
	if len(m.Entries) != 2 || m.Entries[1].Parents[0] != "1.0.0" {
		t.Fatalf("manifest=%+v", m)
	}
	if _, err = Verify(d); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(){"sql": func() { _ = os.WriteFile(filepath.Join(d, files[0].Name), []byte("CREATE TABLE x(id bigint);"), 0600) }, "manifest": func() {
		raw, _ := os.ReadFile(filepath.Join(d, ManifestFile))
		raw[len(raw)-2] ^= 1
		_ = os.WriteFile(filepath.Join(d, ManifestFile), raw, 0600)
	}} {
		t.Run(name, func(t *testing.T) {
			Update(d, UpdateRequest{Files: files})
			mutate()
			if _, e := Verify(d); e == nil {
				t.Fatal("tamper accepted")
			}
		})
	}
}
func TestRejectsCollisionsTraversalAndUnauthorizedFork(t *testing.T) {
	cases := [][]File{{{Name: "V1__a.sql", SQL: []byte("SELECT 1;")}, {Name: "v1__A.sql", SQL: []byte("SELECT 1;")}}, {{Name: "../V1__a.sql", SQL: []byte("SELECT 1;")}}, {{Name: "V1__a.sql", SQL: []byte("SELECT 1;")}, {Name: "V2__b.sql", SQL: []byte("SELECT 2;"), Parents: []string{"0.0.0"}}}}
	for _, files := range cases {
		if _, e := build(files); e == nil {
			t.Fatal("invalid candidate accepted")
		}
	}
}
func TestConcurrentWritersRemainVerifiable(t *testing.T) {
	d := t.TempDir()
	_ = os.Chmod(d, 0700)
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			_, _ = Update(d, UpdateRequest{Files: []File{{Name: "V1__init.sql", SQL: []byte("SELECT 1;\n")}}})
		}(i)
	}
	wg.Wait()
	if _, e := Verify(d); e != nil {
		t.Fatal(e)
	}
}
func TestSnapshotAndCompareSwap(t *testing.T) {
	d := t.TempDir()
	_ = os.Chmod(d, 0755)
	files := []File{{Name: "V1__init.sql", SQL: []byte("SELECT 1;\n")}}
	m, e := Update(d, UpdateRequest{Files: files})
	if e != nil {
		t.Fatal(e)
	}
	s, e := LoadSnapshot(d)
	if e != nil || s.Manifest.Digest != m.Digest || string(s.Files[files[0].Name]) != string(files[0].SQL) {
		t.Fatalf("snapshot=%+v err=%v", s, e)
	}
	if _, e = Update(d, UpdateRequest{Files: files, ExpectedManifestDigest: "sha256:" + strings.Repeat("0", 64)}); e == nil {
		t.Fatal("stale CAS accepted")
	}
}
func TestConflictIsTyped(t *testing.T) {
	_, e := build([]File{{Name: "bad", SQL: []byte("x")}})
	if e == nil {
		t.Fatal()
	}
	var c *ConflictError
	if errors.As(e, &c) && c.Guidance == "" {
		t.Fatal("missing guidance")
	}
}
