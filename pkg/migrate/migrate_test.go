package migrate

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func trustedDir(t *testing.T) string {
	t.Helper()
	d := t.TempDir()
	if e := os.Chmod(d, 0700); e != nil {
		t.Fatal(e)
	}
	return d
}
func baseFiles() []File {
	return []File{
		{Name: "V1__init.sql", SQL: []byte("-- autosql:transaction=required\n-- autosql:plan-digest=sha256:" + strings.Repeat("1", 64) + "\nCREATE TABLE t(id bigint);\n")},
		{Name: "1.1__index.sql", SQL: []byte("-- autosql:check-bundle-digest=sha256:" + strings.Repeat("2", 64) + "\nCREATE INDEX i ON t(id);\n")},
	}
}

func TestUpdateVerifySnapshotAndTypedMetadata(t *testing.T) {
	d := trustedDir(t)
	files := baseFiles()
	m, e := Update(d, UpdateRequest{Files: files})
	if e != nil {
		t.Fatal(e)
	}
	if len(m.Entries) != 2 || m.Entries[1].Parents[0] != "1.0.0" || m.Entries[0].Directives.Transaction != TransactionRequired || len(m.Entries[0].Statements) != 1 || m.Entries[1].Directives.CheckBundleDigest == "" || m.Entries[1].ChainDigest == "" {
		t.Fatalf("manifest=%+v", m)
	}
	s, e := LoadSnapshot(d)
	if e != nil || !bytes.Equal(s.Files[files[0].Name], files[0].SQL) {
		t.Fatalf("snapshot=%+v err=%v", s, e)
	}
	if _, e = Verify(d); e != nil {
		t.Fatal(e)
	}
}

func TestTamperAdditionRemovalEditReorderAndCanonicalJSON(t *testing.T) {
	t.Run("root-addition", func(t *testing.T) {
		d := trustedDir(t)
		_, _ = Update(d, UpdateRequest{Files: baseFiles()})
		if e := os.WriteFile(filepath.Join(d, "V9__foreign.sql"), []byte("SELECT 9"), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := Verify(d); e == nil {
			t.Fatal("addition accepted")
		}
	})
	for _, tc := range []struct {
		name   string
		mutate func(string, Manifest) error
	}{
		{"edit", func(d string, m Manifest) error {
			return os.WriteFile(filepath.Join(d, genDir, m.Generation, m.Entries[0].File), []byte("SELECT 2"), 0600)
		}},
		{"remove", func(d string, m Manifest) error {
			return os.Remove(filepath.Join(d, genDir, m.Generation, m.Entries[0].File))
		}},
		{"generation-addition", func(d string, m Manifest) error {
			return os.WriteFile(filepath.Join(d, genDir, m.Generation, "V9__foreign.sql"), []byte("SELECT 9"), 0600)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := trustedDir(t)
			m, _ := Update(d, UpdateRequest{Files: baseFiles()})
			if e := tc.mutate(d, m); e != nil {
				t.Fatal(e)
			}
			if _, e := Verify(d); e == nil {
				t.Fatal("tamper accepted")
			}
		})
	}
	t.Run("manifest-reorder", func(t *testing.T) {
		d := trustedDir(t)
		m, _ := Update(d, UpdateRequest{Files: baseFiles()})
		m.Entries[0], m.Entries[1] = m.Entries[1], m.Entries[0]
		c := m
		c.Digest = ""
		m.Digest = digest("manifest", canonical(c))
		if e := os.WriteFile(filepath.Join(d, ManifestFile), canonical(m), 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := Verify(d); e == nil {
			t.Fatal("semantic reorder accepted")
		}
	})
	t.Run("duplicate-json", func(t *testing.T) {
		raw := []byte(`{"version":"x","version":"y"}`)
		if _, e := strictManifest(raw); e == nil {
			t.Fatal("duplicate key accepted")
		}
	})
	t.Run("noncanonical-json", func(t *testing.T) {
		d := trustedDir(t)
		_, _ = Update(d, UpdateRequest{Files: baseFiles()})
		p := filepath.Join(d, ManifestFile)
		raw, _ := os.ReadFile(p)
		raw = append([]byte(" \n"), raw...)
		if e := os.WriteFile(p, raw, 0600); e != nil {
			t.Fatal(e)
		}
		if _, e := Load(d); e == nil {
			t.Fatal("noncanonical accepted")
		}
	})
}

func TestDeterminismAndInputOrder(t *testing.T) {
	a := baseFiles()
	b := []File{a[1], a[0]}
	ma, e := build(a)
	if e != nil {
		t.Fatal(e)
	}
	mb, e := build(b)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(canonical(ma), canonical(mb)) {
		t.Fatalf("nondeterministic\n%s\n%s", canonical(ma), canonical(mb))
	}
	for i := 0; i < 100; i++ {
		m, _ := build(a)
		if m.Digest != ma.Digest || m.Generation != ma.Generation {
			t.Fatal("unstable output")
		}
	}
}

func TestCheckpointMetadataDeterministicAfterLongHistory(t *testing.T) {
	files := make([]File, 0, 102)
	for i := 1; i <= 101; i++ {
		files = append(files, File{Name: fmt.Sprintf("V%d__revision.sql", i), SQL: []byte(fmt.Sprintf("SELECT %d;", i))})
	}
	fp := "sha256:" + strings.Repeat("a", 64)
	files = append(files, File{Name: "V102__checkpoint.sql", SQL: []byte("SELECT 102;"), Kind: "checkpoint", CoveredFrom: "1.0.0", CoveredTo: "101.0.0", SchemaFingerprint: fp, DataPolicy: "schema_only"})
	want, err := build(files)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 120; i++ {
		got, e := build(append([]File(nil), files...))
		if e != nil || got.Digest != want.Digest || got.Generation != want.Generation {
			t.Fatalf("iteration %d is not deterministic: %v", i, e)
		}
	}
	e := want.Entries[len(want.Entries)-1]
	if e.Kind != "checkpoint" || e.CoveredFrom != "1.0.0" || e.CoveredTo != "101.0.0" || e.SchemaFingerprint != fp || e.DataPolicy != "schema_only" {
		t.Fatalf("checkpoint binding lost: %+v", e)
	}
}

func TestCheckpointASTSideEffectClassification(t *testing.T) {
	mutating := []string{"-- hidden\n INSERT INTO t VALUES(1)", "WITH changed AS (DELETE FROM t RETURNING *) SELECT * FROM changed", "MERGE INTO t USING s ON false WHEN NOT MATCHED THEN INSERT VALUES(1)", "TRUNCATE t", "SELECT setval('s', 4)", "DO $$ BEGIN PERFORM 1; END $$", "CREATE FUNCTION f() RETURNS void LANGUAGE sql AS $$ SELECT 1 $$"}
	for _, q := range mutating {
		got, err := checkpointSideEffects("x.sql", []byte(q))
		if err != nil || !got {
			t.Fatalf("side effect not classified %q: %v", q, err)
		}
	}
	for _, q := range []string{"  -- comment\nCREATE TABLE t(id bigint)", "ALTER TABLE t ADD COLUMN n text", "CREATE SCHEMA empty"} {
		got, err := checkpointSideEffects("x.sql", []byte(q))
		if err != nil || got {
			t.Fatalf("schema statement classified as data %q: %v", q, err)
		}
	}
}

func TestSemverPrereleaseOrderingAndValidation(t *testing.T) {
	order := []string{"1.0.0-1", "1.0.0-alpha", "1.0.0-alpha.1", "1.0.0-alpha.beta", "1.0.0-beta", "1.0.0"}
	for i := 1; i < len(order); i++ {
		a, e := ParseVersion(order[i-1])
		if e != nil {
			t.Fatal(e)
		}
		b, e := ParseVersion(order[i])
		if e != nil {
			t.Fatal(e)
		}
		if a.Compare(b) >= 0 {
			t.Fatalf("%s !< %s", order[i-1], order[i])
		}
	}
	for _, bad := range []string{"1.0.0-01", "1.0.0-a..b", "1.0.0-a."} {
		if _, e := ParseVersion(bad); e == nil {
			t.Fatalf("accepted %q", bad)
		}
	}
}

func TestGraphValidationAndChainBinding(t *testing.T) {
	linear := []File{{Name: "V1__a.sql", SQL: []byte("SELECT 1;")}, {Name: "V2__b.sql", SQL: []byte("SELECT 2;")}, {Name: "V3__c.sql", SQL: []byte("SELECT 3;")}}
	bad := [][]File{
		{{Name: "V1__a.sql", SQL: []byte("SELECT 1;"), Parents: []string{"9.0.0"}}},
		{{Name: "V1__a.sql", SQL: []byte("SELECT 1;")}, {Name: "V2__b.sql", SQL: []byte("SELECT 2;"), Parents: []string{"2.0.0"}, NonLinear: true}},
		{{Name: "V1__a.sql", SQL: []byte("SELECT 1;")}, {Name: "V2__b.sql", SQL: []byte("SELECT 2;"), Parents: []string{"1.0.0", "1.0.0"}, NonLinear: true}},
	}
	for _, f := range bad {
		if _, e := build(f); e == nil {
			t.Fatal("invalid graph accepted")
		}
	}
	fork := append([]File(nil), linear...)
	fork[2].Parents = []string{"1.0.0"}
	fork[2].NonLinear = true
	m, e := build(fork)
	if e != nil {
		t.Fatal(e)
	}
	if !m.Entries[2].NonLinear || m.Entries[2].Parents[0] != "1.0.0" {
		t.Fatal("graph not persisted")
	}
	changed := append([]File(nil), fork...)
	changed[0].SQL = []byte("SELECT 10;")
	m2, _ := build(changed)
	if m.Entries[2].ChainDigest == m2.Entries[2].ChainDigest {
		t.Fatal("ancestor tamper absent from chain")
	}
	merge := append([]File(nil), linear...)
	merge[2].Parents = []string{"2.0.0", "1.0.0"}
	merge[2].NonLinear = true
	a, e := build(merge)
	if e != nil {
		t.Fatal(e)
	}
	merge[2].Parents = []string{"1.0.0", "2.0.0"}
	b, e := build(merge)
	if e != nil {
		t.Fatal(e)
	}
	if !bytes.Equal(canonical(a), canonical(b)) {
		t.Fatal("parent input order changed canonical history")
	}
}

func TestStrictManifestRejectsStructurallyRehashedTamper(t *testing.T) {
	m, e := build(baseFiles())
	if e != nil {
		t.Fatal(e)
	}
	cases := []func(*Manifest){func(x *Manifest) { x.Generation = strings.Repeat("A", 64) }, func(x *Manifest) { x.Entries[0].SQLDigest = "sha256:" + strings.Repeat("0", 64) }, func(x *Manifest) { x.Entries[0].Statements[0].Ordinal = 2 }, func(x *Manifest) { x.Entries[1].Parents = []string{"1.0.0", "1.0.0"} }, func(x *Manifest) { x.Entries[0], x.Entries[1] = x.Entries[1], x.Entries[0] }}
	for i, mut := range cases {
		x := m
		x.Entries = append([]Migration(nil), m.Entries...)
		mut(&x)
		c := x
		c.Digest = ""
		x.Digest = digest("manifest", canonical(c))
		if _, e := strictManifest(canonical(x)); e == nil {
			t.Fatalf("structural tamper %d accepted", i)
		}
	}
}

func TestUpdateRejectsTamperedCurrentBeforeChangedCandidate(t *testing.T) {
	d := trustedDir(t)
	files := baseFiles()
	m, e := Update(d, UpdateRequest{Files: files})
	if e != nil {
		t.Fatal(e)
	}
	if e = os.WriteFile(filepath.Join(d, genDir, m.Generation, files[0].Name), []byte("SELECT 404"), 0600); e != nil {
		t.Fatal(e)
	}
	next := append([]File(nil), files...)
	next = append(next, File{Name: "V2__next.sql", SQL: []byte("SELECT 2")})
	if _, e = Update(d, UpdateRequest{Files: next, ExpectedManifestDigest: m.Digest}); e == nil {
		t.Fatal("changed candidate concealed current tamper")
	}
}

func TestDirectiveValidationAndDigestBoundaries(t *testing.T) {
	valid := File{Name: "V1__a.sql", SQL: []byte("-- autosql:transaction=forbidden\nSELECT 1; SELECT 2;\n")}
	a, e := build([]File{valid})
	if e != nil {
		t.Fatal(e)
	}
	if len(a.Entries[0].Statements) != 2 || a.Entries[0].Statements[0].Ordinal != 1 {
		t.Fatal("statement boundaries missing")
	}
	variants := [][]byte{
		[]byte("-- autosql:transaction=nope\nSELECT 1"),
		[]byte("-- autosql:unknown=x\nSELECT 1"),
		[]byte("-- autosql:transaction=auto\n-- autosql:transaction=required\nSELECT 1"),
		[]byte("-- autosql:plan-digest=no\nSELECT 1"),
		[]byte("SELECT 1;\n-- autosql:transaction=required"),
	}
	for _, sql := range variants {
		if _, e := build([]File{{Name: "V1__a.sql", SQL: sql}}); e == nil {
			t.Fatalf("invalid directive accepted: %s", sql)
		}
	}
	b, _ := build([]File{{Name: valid.Name, SQL: []byte("-- autosql:transaction=forbidden\nSELECT 1;\nSELECT 2;\n")}})
	if a.Entries[0].BoundaryDigest == b.Entries[0].BoundaryDigest {
		t.Fatal("boundary reorder/position not bound")
	}
}

func TestCASRequiredAndConcurrentWriters(t *testing.T) {
	d := trustedDir(t)
	first, e := Update(d, UpdateRequest{Files: baseFiles()})
	if e != nil {
		t.Fatal(e)
	}
	if _, e = Update(d, UpdateRequest{Files: baseFiles()}); e == nil {
		t.Fatal("missing CAS accepted")
	}
	if _, e = Update(d, UpdateRequest{Files: baseFiles(), ExpectedManifestDigest: "sha256:" + strings.Repeat("0", 64)}); e == nil {
		t.Fatal("stale CAS accepted")
	}
	var wg sync.WaitGroup
	wins := 0
	var mu sync.Mutex
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			f := baseFiles()
			f = append(f, File{Name: "V2__writer" + string(rune('a'+i)) + ".sql", SQL: []byte("SELECT 3;")})
			if _, e := Update(d, UpdateRequest{Files: f, ExpectedManifestDigest: first.Digest}); e == nil {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}(i)
	}
	wg.Wait()
	if wins != 1 {
		t.Fatalf("writers committed=%d", wins)
	}
	if _, e = Verify(d); e != nil {
		t.Fatal(e)
	}
}

func TestCASNoopStillDetectsGenerationTamper(t *testing.T) {
	d := trustedDir(t)
	files := baseFiles()
	m, e := Update(d, UpdateRequest{Files: files})
	if e != nil {
		t.Fatal(e)
	}
	p := filepath.Join(d, genDir, m.Generation, files[0].Name)
	if e = os.WriteFile(p, []byte("SELECT 99"), 0600); e != nil {
		t.Fatal(e)
	}
	if _, e = Update(d, UpdateRequest{Files: files, ExpectedManifestDigest: m.Digest}); e == nil {
		t.Fatal("same-digest update concealed tamper")
	}
}

func TestNoFollowModesLinksAndUnicodeSize(t *testing.T) {
	t.Run("symlink-root", func(t *testing.T) {
		real := trustedDir(t)
		link := filepath.Join(t.TempDir(), "link")
		if e := os.Symlink(real, link); e != nil {
			t.Fatal(e)
		}
		if _, e := Update(link, UpdateRequest{Files: baseFiles()}); e == nil {
			t.Fatal("symlink root accepted")
		}
	})
	t.Run("unsafe-root-mode", func(t *testing.T) {
		d := trustedDir(t)
		_ = os.Chmod(d, 0777)
		if _, e := Update(d, UpdateRequest{Files: baseFiles()}); e == nil {
			t.Fatal("unsafe root accepted")
		}
	})
	t.Run("hardlink", func(t *testing.T) {
		d := trustedDir(t)
		m, _ := Update(d, UpdateRequest{Files: baseFiles()})
		p := filepath.Join(d, genDir, m.Generation, m.Entries[0].File)
		if e := os.Link(p, p+".link"); e != nil {
			t.Fatal(e)
		}
		if _, e := Verify(d); e == nil {
			t.Fatal("linked file accepted")
		}
	})
	t.Run("unicode", func(t *testing.T) {
		if _, e := build([]File{{Name: "V1__café.sql", SQL: []byte("SELECT 1")}}); e == nil {
			t.Fatal("ambiguous unicode name accepted")
		}
		if _, e := build([]File{{Name: "V1__a.sql", SQL: []byte{0xff}}}); e == nil {
			t.Fatal("invalid utf8 accepted")
		}
	})
	t.Run("size", func(t *testing.T) {
		if _, e := build([]File{{Name: "V1__a.sql", SQL: bytes.Repeat([]byte("x"), maxFile+1)}}); e == nil {
			t.Fatal("oversize accepted")
		}
	})
}

func TestCheckedWriteAndFsyncFailuresNeverExposeMixedSnapshot(t *testing.T) {
	for fail := 1; fail <= 12; fail++ {
		t.Run(string(rune('a'+fail)), func(t *testing.T) {
			d := trustedDir(t)
			old, e := Update(d, UpdateRequest{Files: baseFiles()})
			if e != nil {
				t.Fatal(e)
			}
			next := baseFiles()
			next = append(next, File{Name: "V2__next.sql", SQL: []byte("SELECT 3;")})
			calls := 0
			hit := false
			boom := errors.New("injected")
			ops := Ops{Write: func(fd int, p []byte) (int, error) {
				calls++
				if calls == fail {
					hit = true
					return 0, boom
				}
				if len(p) > 1 {
					return unix.Write(fd, p[:len(p)/2])
				}
				return unix.Write(fd, p)
			}, Fsync: func(fd int) error {
				calls++
				if calls == fail {
					hit = true
					return boom
				}
				return unix.Fsync(fd)
			}, Renameat: func(a int, ap string, b int, bp string) error {
				calls++
				if calls == fail {
					hit = true
					return boom
				}
				return unix.Renameat(a, ap, b, bp)
			}}
			_, _ = UpdateWithOps(d, UpdateRequest{Files: next, ExpectedManifestDigest: old.Digest}, ops)
			if !hit {
				t.Fatalf("fault boundary %d was not reached", fail)
			}
			s, e := LoadSnapshot(d)
			if e != nil {
				t.Fatalf("mixed/unreadable state after boundary %d: %v", fail, e)
			}
			if s.Manifest.Digest != old.Digest && len(s.Manifest.Entries) != 3 {
				t.Fatalf("neither old nor new: %+v", s.Manifest)
			}
			if _, e = Update(d, UpdateRequest{Files: next, ExpectedManifestDigest: old.Digest}); e != nil {
				t.Fatalf("authorized resume %d: %v", fail, e)
			}
			if s, e = LoadSnapshot(d); e != nil || len(s.Manifest.Entries) != 3 {
				t.Fatalf("resume not new: %v %+v", e, s.Manifest)
			}
		})
	}
	if e := writeAll(1, []byte("x"), Ops{Write: func(int, []byte) (int, error) { return 0, nil }}); !errors.Is(e, io.ErrShortWrite) {
		t.Fatal("zero write accepted")
	}
}

func TestRecoveryPreservesUnpublishedGeneration(t *testing.T) {
	d := trustedDir(t)
	old, _ := Update(d, UpdateRequest{Files: baseFiles()})
	next := baseFiles()
	next = append(next, File{Name: "V2__next.sql", SQL: []byte("SELECT 3")})
	calls := 0
	_, e := UpdateWithOps(d, UpdateRequest{Files: next, ExpectedManifestDigest: old.Digest}, Ops{Renameat: func(a int, ap string, b int, bp string) error { calls++; return errors.New("stop before publish") }})
	if e == nil {
		t.Fatal("fault did not fire")
	}
	if e = Recover(d); e != nil {
		t.Fatal(e)
	}
	s, e := LoadSnapshot(d)
	if e != nil || len(s.Manifest.Entries) != 3 {
		t.Fatalf("authorized candidate not recovered: %v %+v", e, s.Manifest)
	}
}

func TestV0ToV1AtomicMigration(t *testing.T) {
	d := legacyFixture(t)
	m, e := MigrateManifest(d, LegacyVersion)
	if e != nil {
		t.Fatal(e)
	}
	if m.Version != ManifestVersion {
		t.Fatal("not upgraded")
	}
	if _, e = Verify(d); e != nil {
		t.Fatal(e)
	}
	if _, e = os.Stat(filepath.Join(d, "V1__legacy.sql")); !os.IsNotExist(e) {
		t.Fatal("legacy root SQL was not atomically retired")
	}
	if _, e = os.Stat(filepath.Join(d, ManifestFile+".v0")); e != nil {
		t.Fatal("legacy fixture not retained")
	}
	if again, e := MigrateManifest(d, LegacyVersion); e != nil || again.Digest != m.Digest {
		t.Fatalf("idempotent reentry: %v", e)
	}
}

func legacyFixture(t *testing.T) string {
	t.Helper()
	d := trustedDir(t)
	sql := []byte("SELECT 1;\n")
	if e := os.WriteFile(filepath.Join(d, "V1__legacy.sql"), sql, 0600); e != nil {
		t.Fatal(e)
	}
	type le struct {
		File      string   `json:"file"`
		Parents   []string `json:"parents,omitempty"`
		NonLinear bool     `json:"nonlinear,omitempty"`
	}
	type lm struct {
		Version string `json:"version"`
		Entries []le   `json:"entries"`
		Digest  string `json:"digest"`
	}
	old := lm{Version: LegacyVersion, Entries: []le{{File: "V1__legacy.sql"}}}
	c := old
	c.Digest = ""
	old.Digest = digest("manifest-v0", canonical(c))
	if e := os.WriteFile(filepath.Join(d, ManifestFile), canonical(old), 0600); e != nil {
		t.Fatal(e)
	}
	return d
}

func TestV0MigrationCrashBoundariesRecoverAndReenter(t *testing.T) {
	cases := []struct {
		name string
		ops  func(*bool) Ops
	}{
		{"publish", func(hit *bool) Ops {
			return Ops{Renameat: func(a int, ap string, b int, bp string) error { *hit = true; return errors.New("publish") }}
		}},
		{"cleanup", func(hit *bool) Ops {
			n := 0
			return Ops{Unlinkat: func(fd int, p string, f int) error {
				n++
				if strings.HasSuffix(p, ".sql") {
					*hit = true
					return errors.New("cleanup")
				}
				return unix.Unlinkat(fd, p, f)
			}}
		}},
		{"fsync", func(hit *bool) Ops {
			n := 0
			return Ops{Fsync: func(fd int) error {
				n++
				if n == 7 {
					*hit = true
					return errors.New("fsync")
				}
				return unix.Fsync(fd)
			}}
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			d := legacyFixture(t)
			hit := false
			_, e := MigrateManifestWithOps(d, LegacyVersion, tc.ops(&hit))
			if e == nil || !hit {
				t.Fatalf("fault not reached: %v", e)
			}
			if _, e = MigrateManifest(d, LegacyVersion); e != nil {
				t.Fatalf("reentry: %v", e)
			}
			if _, e = LoadSnapshot(d); e != nil {
				t.Fatal(e)
			}
			if _, e = os.Stat(filepath.Join(d, "V1__legacy.sql")); !os.IsNotExist(e) {
				t.Fatal("legacy cleanup incomplete")
			}
		})
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

func TestMigrationSubprocessHelper(t *testing.T) {
	mode := os.Getenv("AUTOSQL_MIGRATE_HELPER")
	if mode == "" {
		return
	}
	d := os.Getenv("AUTOSQL_MIGRATE_DIR")
	switch mode {
	case "writer":
		next := baseFiles()
		next = append(next, File{Name: "V2__winner.sql", SQL: []byte("SELECT 2")})
		if _, e := Update(d, UpdateRequest{Files: next, ExpectedManifestDigest: os.Getenv("AUTOSQL_MIGRATE_EXPECTED")}); e != nil {
			os.Exit(3)
		}
		os.Exit(0)
	case "holder":
		fd, e := unix.Open(filepath.Join(d, lockFile), unix.O_RDWR, 0)
		if e != nil {
			os.Exit(4)
		}
		if unix.Flock(fd, unix.LOCK_EX) != nil {
			os.Exit(5)
		}
		_ = os.WriteFile(os.Getenv("AUTOSQL_MIGRATE_READY"), []byte("ready"), 0600)
		time.Sleep(time.Minute)
	case "reader":
		for i := 0; i < 200; i++ {
			s, e := LoadSnapshot(d)
			if e != nil || (len(s.Manifest.Entries) != 2 && len(s.Manifest.Entries) != 3) {
				os.Exit(6)
			}
		}
		os.Exit(0)
	}
}

func helperCommand(mode, dir string, extra ...string) *exec.Cmd {
	args := []string{"-test.run=TestMigrationSubprocessHelper"}
	c := exec.Command(os.Args[0], args...)
	c.Env = append(os.Environ(), "AUTOSQL_MIGRATE_HELPER="+mode, "AUTOSQL_MIGRATE_DIR="+dir)
	c.Env = append(c.Env, extra...)
	return c
}

func TestSubprocessCASWinnerLoser(t *testing.T) {
	d := trustedDir(t)
	m, e := Update(d, UpdateRequest{Files: baseFiles()})
	if e != nil {
		t.Fatal(e)
	}
	env := "AUTOSQL_MIGRATE_EXPECTED=" + m.Digest
	a, b := helperCommand("writer", d, env), helperCommand("writer", d, env)
	if e = a.Start(); e != nil {
		t.Fatal(e)
	}
	if e = b.Start(); e != nil {
		t.Fatal(e)
	}
	ea, eb := a.Wait(), b.Wait()
	wins := 0
	if ea == nil {
		wins++
	}
	if eb == nil {
		wins++
	}
	if wins != 1 {
		t.Fatalf("wins=%d errors=%v,%v", wins, ea, eb)
	}
	if _, e = Verify(d); e != nil {
		t.Fatal(e)
	}
}

func TestKilledLockHolderReleasesKernelLock(t *testing.T) {
	d := trustedDir(t)
	m, e := Update(d, UpdateRequest{Files: baseFiles()})
	if e != nil {
		t.Fatal(e)
	}
	ready := filepath.Join(t.TempDir(), "ready")
	c := helperCommand("holder", d, "AUTOSQL_MIGRATE_READY="+ready)
	if e = c.Start(); e != nil {
		t.Fatal(e)
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		if _, e = os.Stat(ready); e == nil {
			break
		}
		if time.Now().After(deadline) {
			_ = c.Process.Kill()
			t.Fatal("holder not ready")
		}
		time.Sleep(10 * time.Millisecond)
	}
	if e = c.Process.Kill(); e != nil {
		t.Fatal(e)
	}
	_ = c.Wait()
	next := baseFiles()
	next = append(next, File{Name: "V2__after_kill.sql", SQL: []byte("SELECT 2")})
	if _, e = Update(d, UpdateRequest{Files: next, ExpectedManifestDigest: m.Digest}); e != nil {
		t.Fatal(e)
	}
}

func TestSubprocessSharedReadersSeeOnlyOldOrNew(t *testing.T) {
	d := trustedDir(t)
	m, e := Update(d, UpdateRequest{Files: baseFiles()})
	if e != nil {
		t.Fatal(e)
	}
	readers := make([]*exec.Cmd, 4)
	for i := range readers {
		readers[i] = helperCommand("reader", d)
		if e = readers[i].Start(); e != nil {
			t.Fatal(e)
		}
	}
	next := baseFiles()
	next = append(next, File{Name: "V2__new.sql", SQL: []byte("SELECT 2")})
	if _, e = Update(d, UpdateRequest{Files: next, ExpectedManifestDigest: m.Digest}); e != nil {
		t.Fatal(e)
	}
	for _, c := range readers {
		if e = c.Wait(); e != nil {
			t.Fatal(e)
		}
	}
}

func TestEveryInjectedStorageOperationIsReachedAndRecoverable(t *testing.T) {
	boom := errors.New("injected")
	makers := map[string]func(*bool) Ops{
		"open": func(h *bool) Ops {
			return Ops{Open: func(string, int, uint32) (int, error) { *h = true; return -1, boom }}
		},
		"openat": func(h *bool) Ops {
			return Ops{Openat: func(int, string, int, uint32) (int, error) { *h = true; return -1, boom }}
		},
		"close": func(h *bool) Ops {
			return Ops{Close: func(fd int) error {
				e := unix.Close(fd)
				if !*h {
					*h = true
					return boom
				}
				return e
			}}
		},
		"mkdir":  func(h *bool) Ops { return Ops{Mkdirat: func(int, string, uint32) error { *h = true; return boom }} },
		"unlink": func(h *bool) Ops { return Ops{Unlinkat: func(int, string, int) error { *h = true; return boom }} },
		"lock":   func(h *bool) Ops { return Ops{Flock: func(int, int) error { *h = true; return boom }} },
		"write":  func(h *bool) Ops { return Ops{Write: func(int, []byte) (int, error) { *h = true; return 0, boom }} },
		"fsync":  func(h *bool) Ops { return Ops{Fsync: func(int) error { *h = true; return boom }} },
		"rename": func(h *bool) Ops {
			return Ops{Renameat: func(int, string, int, string) error { *h = true; return boom }}
		},
	}
	for name, mk := range makers {
		t.Run(name, func(t *testing.T) {
			d := trustedDir(t)
			old, e := Update(d, UpdateRequest{Files: baseFiles()})
			if e != nil {
				t.Fatal(e)
			}
			next := baseFiles()
			next = append(next, File{Name: "V2__next.sql", SQL: []byte("SELECT 2")})
			hit := false
			_, _ = UpdateWithOps(d, UpdateRequest{Files: next, ExpectedManifestDigest: old.Digest}, mk(&hit))
			if !hit {
				t.Fatal("fault not reached")
			}
			_ = Recover(d)
			s, e := LoadSnapshot(d)
			if e != nil {
				t.Fatal(e)
			}
			if len(s.Manifest.Entries) == 2 {
				if _, e = Update(d, UpdateRequest{Files: next, ExpectedManifestDigest: old.Digest}); e != nil {
					t.Fatal(e)
				}
			} else if len(s.Manifest.Entries) != 3 {
				t.Fatalf("mixed entries=%d", len(s.Manifest.Entries))
			}
			if _, e = Verify(d); e != nil {
				t.Fatal(e)
			}
		})
	}
}

func TestEINTRWriteAndFsyncAreRetried(t *testing.T) {
	d := trustedDir(t)
	wc, sc := 0, 0
	ops := Ops{Write: func(fd int, p []byte) (int, error) {
		wc++
		if wc == 1 {
			return 0, unix.EINTR
		}
		return unix.Write(fd, p)
	}, Fsync: func(fd int) error {
		sc++
		if sc == 1 {
			return unix.EINTR
		}
		return unix.Fsync(fd)
	}}
	if _, e := UpdateWithOps(d, UpdateRequest{Files: baseFiles()}, ops); e != nil {
		t.Fatal(e)
	}
	if wc < 2 || sc < 2 {
		t.Fatal("EINTR not retried")
	}
}
