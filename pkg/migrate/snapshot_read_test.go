package migrate

import (
	"bytes"
	"os"
	"path/filepath"
	"reflect"
	"syscall"
	"testing"
)

func inspectSnapshotFixture(t *testing.T) (string, *os.File, Manifest) {
	t.Helper()
	dir := trustedDir(t)
	m, err := Update(dir, UpdateRequest{Files: baseFiles()})
	if err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { file.Close() })
	return dir, file, m
}

func TestInspectSnapshotDoesNotWriteOrChangeCallerOffset(t *testing.T) {
	dir, file, m := inspectSnapshotFixture(t)
	if err := os.Remove(filepath.Join(dir, lockFile)); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.Chmod(dir, 0700) })
	before, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	// Exhaust the caller's iterator; inspection must use a new description.
	if _, err := file.ReadDir(-1); err != nil {
		t.Fatal(err)
	}
	for range 2 {
		snapshot, err := InspectSnapshot(file)
		if err != nil || !reflect.DeepEqual(snapshot.Manifest, m) || !bytes.Equal(snapshot.Files[baseFiles()[0].Name], baseFiles()[0].SQL) {
			t.Fatalf("read-only capture: %v %+v", err, snapshot)
		}
	}
	after, err := os.ReadDir(dir)
	if err != nil || len(before) != len(after) {
		t.Fatalf("inspection changed directory entries: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, lockFile)); !os.IsNotExist(err) {
		t.Fatalf("inspection created a lock: %v", err)
	}
}

func TestInspectSnapshotPinsOpenedDirectoryAndRetainsTamperChecks(t *testing.T) {
	dir, file, original := inspectSnapshotFixture(t)
	moved := dir + "-moved"
	if err := os.Rename(dir, moved); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(moved) })
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := Update(dir, UpdateRequest{Files: []File{{Name: "V1__other.sql", SQL: []byte("SELECT 7;")}}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := InspectSnapshot(file)
	if err != nil || snapshot.Manifest.Digest != original.Digest {
		t.Fatalf("reopened replaced path: %v", err)
	}
	if err := os.WriteFile(filepath.Join(moved, "V9__untracked.sql"), []byte("SELECT 9;"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := InspectSnapshot(file); err == nil {
		t.Fatal("repeated inspection skipped untracked SQL")
	}
}

func TestInspectSnapshotRefusesUnsafeFiles(t *testing.T) {
	for _, kind := range []string{"modified", "missing", "symlink", "fifo", "extra", "unsafe-directory"} {
		t.Run(kind, func(t *testing.T) {
			dir, directory, m := inspectSnapshotFixture(t)
			file := filepath.Join(dir, genDir, m.Generation, m.Entries[0].File)
			var err error
			switch kind {
			case "modified":
				err = os.WriteFile(file, []byte("SELECT 9;"), 0600)
			case "missing", "symlink", "fifo":
				if err = os.Remove(file); err != nil {
					t.Fatal(err)
				}
				if kind == "symlink" {
					err = os.Symlink("../../"+ManifestFile, file)
				} else if kind == "fifo" {
					err = syscall.Mkfifo(file, 0600)
				}
			case "extra":
				err = os.WriteFile(filepath.Join(dir, genDir, m.Generation, "extra"), nil, 0600)
			case "unsafe-directory":
				err = os.Chmod(dir, 0777)
			}
			if err != nil {
				t.Fatal(err)
			}
			if _, err := InspectSnapshot(directory); err == nil {
				t.Fatal("unsafe migration snapshot accepted")
			}
		})
	}
}

func TestPrefixSnapshotReconstructsGenerationBeforePublication(t *testing.T) {
	dir := trustedDir(t)
	first, err := Update(dir, UpdateRequest{Files: baseFiles()[:1]})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Update(dir, UpdateRequest{Files: baseFiles(), ExpectedManifestDigest: first.Digest}); err != nil {
		t.Fatal(err)
	}
	full, err := LoadSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	prefix, err := PrefixSnapshot(full, 1)
	if err != nil || !reflect.DeepEqual(prefix.Manifest, first) {
		t.Fatalf("prefix did not reproduce original generation: %v", err)
	}
	empty, err := PrefixSnapshot(full, 0)
	want, _ := build(nil)
	if err != nil || empty.Manifest.Digest != want.Digest {
		t.Fatalf("empty generation mismatch: %v", err)
	}
	prefix.Files[first.Entries[0].File][0] = '!'
	if _, err := PrefixSnapshot(full, 1); err != nil {
		t.Fatal("returned prefix aliased original bytes")
	}
	// A valid prefix cannot hide tampering in the subsequently published entry.
	full.Files[full.Manifest.Entries[1].File][0] = '!'
	if _, err := PrefixSnapshot(full, 1); err == nil {
		t.Fatal("prefix skipped complete snapshot verification")
	}
}

func TestPrefixSnapshotRefusesMalformedAndExtraInputs(t *testing.T) {
	for _, mutate := range []func(*Snapshot){
		func(s *Snapshot) { s.Manifest.Digest = "invalid" },
		func(s *Snapshot) { s.ManifestBytes = append(s.ManifestBytes, ' ') },
		func(s *Snapshot) { s.Files["untracked"] = []byte("extra") },
		func(s *Snapshot) { delete(s.Files, s.Manifest.Entries[0].File) },
	} {
		_, directory, _ := inspectSnapshotFixture(t)
		s, err := InspectSnapshot(directory)
		if err != nil {
			t.Fatal(err)
		}
		mutate(&s)
		if _, err := PrefixSnapshot(s, 0); err == nil {
			t.Fatal("invalid snapshot received a prefix identity")
		}
	}
}

func TestPrefixSnapshotRequiresNamedEmptySQLFile(t *testing.T) {
	dir := trustedDir(t)
	if _, err := Update(dir, UpdateRequest{Files: []File{{Name: "V1__empty.sql"}}}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := LoadSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	delete(snapshot.Files, "V1__empty.sql")
	snapshot.Files["extra"] = nil
	if _, err := PrefixSnapshot(snapshot, 0); err == nil {
		t.Fatal("matching byte content and file count hid a missing named file")
	}
}
