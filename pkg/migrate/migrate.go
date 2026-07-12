// Package migrate manages a tamper-evident, transactional SQL migration directory.
package migrate

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"unicode/utf8"

	"autosql/pkg/source"
)

const ManifestVersion = "autosql.migration-directory/v1"
const ManifestFile = ".autosql-manifest.json"
const maxFile = 8 << 20

var ErrInvalid = errors.New("invalid migration directory")
var ErrConflict = errors.New("migration directory conflict")

type ConflictError struct{ Code, Path, Guidance string }

func (e *ConflictError) Error() string {
	return "migration directory " + e.Code + ": " + e.Path + "; " + e.Guidance
}
func (e *ConflictError) Unwrap() error           { return ErrConflict }
func conflict(code, path, guidance string) error { return &ConflictError{code, path, guidance} }

type Version struct {
	Major, Minor, Patch uint64
	Pre                 string
}

var versionRE = regexp.MustCompile(`^(0|[1-9][0-9]*)(?:\.(0|[1-9][0-9]*))?(?:\.(0|[1-9][0-9]*))?(?:-([0-9A-Za-z][0-9A-Za-z.-]*))?$`)

func ParseVersion(s string) (Version, error) {
	m := versionRE.FindStringSubmatch(s)
	if m == nil {
		return Version{}, fmt.Errorf("%w: version", ErrInvalid)
	}
	nums := []uint64{}
	for _, x := range m[1:4] {
		if x == "" {
			nums = append(nums, 0)
			continue
		}
		n, e := strconv.ParseUint(x, 10, 64)
		if e != nil {
			return Version{}, ErrInvalid
		}
		nums = append(nums, n)
	}
	return Version{nums[0], nums[1], nums[2], m[4]}, nil
}
func (v Version) String() string {
	s := fmt.Sprintf("%d.%d.%d", v.Major, v.Minor, v.Patch)
	if v.Pre != "" {
		s += "-" + v.Pre
	}
	return s
}
func (v Version) Compare(w Version) int {
	for _, p := range [][2]uint64{{v.Major, w.Major}, {v.Minor, w.Minor}, {v.Patch, w.Patch}} {
		if p[0] < p[1] {
			return -1
		}
		if p[0] > p[1] {
			return 1
		}
	}
	if v.Pre == w.Pre {
		return 0
	}
	if v.Pre == "" {
		return 1
	}
	if w.Pre == "" {
		return -1
	}
	return strings.Compare(v.Pre, w.Pre)
}

type Directive struct {
	Name  string `json:"name"`
	Value string `json:"value"`
	Line  int    `json:"line"`
}
type Migration struct {
	File            string   `json:"file"`
	Version         string   `json:"version"`
	Name            string   `json:"name"`
	SQLDigest       string   `json:"sql_digest"`
	DirectiveDigest string   `json:"directive_digest"`
	BoundaryDigest  string   `json:"boundary_digest"`
	Parents         []string `json:"parents"`
	NonLinear       bool     `json:"nonlinear,omitempty"`
}
type Manifest struct {
	Version string      `json:"version"`
	Entries []Migration `json:"entries"`
	Digest  string      `json:"digest"`
}
type File struct {
	Name      string
	SQL       []byte
	Parents   []string
	NonLinear bool
}
type UpdateRequest struct {
	Files                  []File
	ManifestVersion        string
	ExpectedManifestDigest string
}
type Snapshot struct {
	Manifest      Manifest
	ManifestBytes []byte
	Files         map[string][]byte
}

var fileRE = regexp.MustCompile(`^(?:V)?([0-9]+(?:\.[0-9]+){0,2}(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?)__([-A-Za-z0-9][-.A-Za-z0-9_]*)\.sql$`)

func digest(domain string, b []byte) string {
	x := sha256.Sum256(append([]byte("autosql.migrate."+domain+"/v1\x00"), b...))
	return "sha256:" + hex.EncodeToString(x[:])
}
func parseFile(name string, sql []byte, parents []string, nonlinear bool) (Migration, error) {
	if len(sql) > maxFile || !utf8.Valid(sql) || bytes.IndexByte(sql, 0) >= 0 {
		return Migration{}, fmt.Errorf("%w: SQL encoding", ErrInvalid)
	}
	if filepath.Base(name) != name || strings.Contains(name, "..") || strings.ContainsAny(name, "/\\") {
		return Migration{}, conflict("traversal", name, "use a base filename")
	}
	m := fileRE.FindStringSubmatch(name)
	if m == nil {
		return Migration{}, fmt.Errorf("%w: filename %q", ErrInvalid, name)
	}
	v, e := ParseVersion(m[1])
	if e != nil {
		return Migration{}, e
	}
	statements, e := source.SplitSQL(name, string(sql))
	if e != nil {
		return Migration{}, e
	}
	bounds := make([]string, len(statements))
	for i, s := range statements {
		bounds[i] = fmt.Sprintf("%d:%d:%s", s.Position.Line, s.Position.Column, digest("statement", []byte(s.SQL)))
	}
	var dirs []Directive
	for i, line := range strings.Split(string(sql), "\n") {
		t := strings.TrimSpace(line)
		if strings.HasPrefix(t, "-- autosql:") {
			kv := strings.TrimSpace(strings.TrimPrefix(t, "-- autosql:"))
			k, v, ok := strings.Cut(kv, "=")
			if !ok || strings.TrimSpace(k) == "" {
				return Migration{}, fmt.Errorf("%w: directive", ErrInvalid)
			}
			dirs = append(dirs, Directive{strings.TrimSpace(k), strings.TrimSpace(v), i + 1})
		}
	}
	db, _ := json.Marshal(dirs)
	return Migration{File: name, Version: v.String(), Name: m[2], SQLDigest: digest("sql", sql), DirectiveDigest: digest("directives", db), BoundaryDigest: digest("boundaries", []byte(strings.Join(bounds, "\x00"))), Parents: append([]string(nil), parents...), NonLinear: nonlinear}, nil
}
func build(files []File) (Manifest, error) {
	seenName := map[string]bool{}
	seenVersion := map[string]bool{}
	entries := make([]Migration, 0, len(files))
	for _, f := range files {
		fold := strings.ToLower(f.Name)
		if seenName[fold] {
			return Manifest{}, conflict("case_collision", f.Name, "rename the colliding migration")
		}
		seenName[fold] = true
		e, err := parseFile(f.Name, f.SQL, f.Parents, f.NonLinear)
		if err != nil {
			return Manifest{}, err
		}
		if seenVersion[e.Version] {
			return Manifest{}, conflict("version_collision", e.Version, "assign a unique canonical version")
		}
		seenVersion[e.Version] = true
		entries = append(entries, e)
	}
	sort.Slice(entries, func(i, j int) bool {
		a, _ := ParseVersion(entries[i].Version)
		b, _ := ParseVersion(entries[j].Version)
		return a.Compare(b) < 0
	})
	for i := range entries {
		if i == 0 {
			if len(entries[i].Parents) > 0 {
				return Manifest{}, conflict("root_parent", entries[i].File, "remove root parents")
			}
			continue
		}
		expected := entries[i-1].Version
		if len(entries[i].Parents) == 0 {
			entries[i].Parents = []string{expected}
		}
		if len(entries[i].Parents) != 1 || entries[i].Parents[0] != expected {
			if !entries[i].NonLinear || len(entries[i].Parents) < 2 {
				return Manifest{}, conflict("nonlinear_chain", entries[i].File, "declare nonlinear=true and explicit parents")
			}
		}
	}
	man := Manifest{Version: ManifestVersion, Entries: entries}
	raw, _ := json.Marshal(man)
	man.Digest = digest("manifest", raw)
	return man, nil
}
func strictManifest(raw []byte) (Manifest, error) {
	if len(raw) > maxFile || !utf8.Valid(raw) {
		return Manifest{}, ErrInvalid
	}
	var m Manifest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&m); e != nil {
		return m, e
	}
	var x any
	if d.Decode(&x) != io.EOF {
		return m, ErrInvalid
	}
	if m.Version != ManifestVersion {
		return m, conflict("manifest_version", m.Version, "run MigrateManifest explicitly")
	}
	copy := m
	copy.Digest = ""
	b, _ := json.Marshal(copy)
	if digest("manifest", b) != m.Digest {
		return m, conflict("manifest_digest", ManifestFile, "restore or explicitly regenerate the manifest")
	}
	return m, nil
}
func Load(dir string) (Manifest, error) {
	raw, e := os.ReadFile(filepath.Join(dir, ManifestFile))
	if e != nil {
		return Manifest{}, e
	}
	return strictManifest(raw)
}
func LoadSnapshot(dir string) (Snapshot, error) {
	lock, e := os.OpenFile(filepath.Join(dir, ".autosql.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return Snapshot{}, e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_SH); e != nil {
		return Snapshot{}, e
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	raw, e := os.ReadFile(filepath.Join(dir, ManifestFile))
	if e != nil {
		return Snapshot{}, e
	}
	m, e := strictManifest(raw)
	if e != nil {
		return Snapshot{}, e
	}
	fs, e := readFiles(dir)
	if e != nil {
		return Snapshot{}, e
	}
	out := Snapshot{Manifest: m, ManifestBytes: append([]byte(nil), raw...), Files: map[string][]byte{}}
	for _, f := range fs {
		out.Files[f.Name] = append([]byte(nil), f.SQL...)
	}
	return out, nil
}

// Recover completes only a demonstrably published transaction. Partial or
// foreign bytes are never guessed at or deleted; callers receive typed dirty
// state guidance and must restore the last trusted snapshot.
func Recover(dir string) error {
	lock, e := os.OpenFile(filepath.Join(dir, ".autosql.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		return e
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	journal := filepath.Join(dir, ".autosql-journal")
	raw, e := os.ReadFile(journal)
	if os.IsNotExist(e) {
		return nil
	}
	if e != nil {
		return e
	}
	var j struct {
		ManifestDigest string `json:"manifest_digest"`
	}
	if json.Unmarshal(raw, &j) != nil || j.ManifestDigest == "" {
		return conflict("dirty_journal", journal, "restore the last trusted snapshot")
	}
	m, e := Load(dir)
	if e != nil || m.Digest != j.ManifestDigest {
		return conflict("incomplete_transaction", dir, "restore the last trusted snapshot; foreign bytes were preserved")
	}
	if e = os.Remove(journal); e != nil {
		return e
	}
	dh, e := os.Open(dir)
	if e == nil {
		e = dh.Sync()
		_ = dh.Close()
	}
	return e
}
func Verify(dir string) (Manifest, error) {
	stored, e := Load(dir)
	if e != nil {
		return Manifest{}, e
	}
	files, e := readFiles(dir)
	if e != nil {
		return Manifest{}, e
	}
	actual, e := build(files)
	if e != nil {
		return Manifest{}, e
	}
	a, _ := json.Marshal(stored)
	b, _ := json.Marshal(actual)
	if !bytes.Equal(a, b) {
		return Manifest{}, conflict("content_changed", dir, "restore the byte-exact migration set or perform an authorized update")
	}
	return stored, nil
}
func readFiles(dir string) ([]File, error) {
	es, e := os.ReadDir(dir)
	if e != nil {
		return nil, e
	}
	var out []File
	for _, x := range es {
		if !strings.HasSuffix(strings.ToLower(x.Name()), ".sql") {
			continue
		}
		info, e := x.Info()
		if e != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0022 != 0 || info.Sys() != nil && info.Sys().(*syscall.Stat_t).Nlink != 1 {
			return nil, conflict("unsafe_file", x.Name(), "use one regular non-linked file without group/world write")
		}
		raw, e := os.ReadFile(filepath.Join(dir, x.Name()))
		if e != nil {
			return nil, e
		}
		out = append(out, File{Name: x.Name(), SQL: raw})
	}
	return out, nil
}

func Update(dir string, req UpdateRequest) (Manifest, error) {
	if req.ManifestVersion != "" && req.ManifestVersion != ManifestVersion {
		return Manifest{}, conflict("manifest_version", req.ManifestVersion, "migrate explicitly")
	}
	st, e := os.Lstat(dir)
	if e != nil || !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0022 != 0 {
		return Manifest{}, conflict("unsafe_directory", dir, "use a trusted non-symlink directory")
	}
	lock, e := os.OpenFile(filepath.Join(dir, ".autosql.lock"), os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return Manifest{}, e
	}
	defer lock.Close()
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		return Manifest{}, e
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	if req.ExpectedManifestDigest != "" {
		current, e := Load(dir)
		if e != nil && !os.IsNotExist(e) {
			return Manifest{}, e
		}
		if current.Digest != req.ExpectedManifestDigest {
			return Manifest{}, conflict("compare_and_swap", dir, "reload the snapshot and retry")
		}
	}
	man, e := build(req.Files)
	if e != nil {
		return Manifest{}, e
	}
	stage, e := os.MkdirTemp(dir, ".autosql-stage-")
	if e != nil {
		return Manifest{}, e
	}
	defer os.RemoveAll(stage)
	for _, f := range req.Files {
		p := filepath.Join(stage, f.Name)
		if e = os.WriteFile(p, f.SQL, 0644); e != nil {
			return Manifest{}, e
		}
		h, _ := os.Open(p)
		_ = h.Sync()
		_ = h.Close()
	}
	raw, _ := json.Marshal(man)
	journal := filepath.Join(dir, ".autosql-journal")
	jr, _ := json.Marshal(map[string]any{"manifest_digest": man.Digest, "files": man.Entries})
	if e = os.WriteFile(journal, jr, 0600); e != nil {
		return Manifest{}, e
	}
	jh, _ := os.Open(journal)
	_ = jh.Sync()
	_ = jh.Close()
	if e = os.WriteFile(filepath.Join(stage, ManifestFile), raw, 0644); e != nil {
		return Manifest{}, e
	}
	current, _ := readFiles(dir)
	for _, f := range current {
		if e = os.Remove(filepath.Join(dir, f.Name)); e != nil {
			return Manifest{}, e
		}
	}
	for _, f := range req.Files {
		if e = os.Rename(filepath.Join(stage, f.Name), filepath.Join(dir, f.Name)); e != nil {
			return Manifest{}, e
		}
	}
	if e = os.Rename(filepath.Join(stage, ManifestFile), filepath.Join(dir, ManifestFile)); e != nil {
		return Manifest{}, e
	}
	dh, _ := os.Open(dir)
	_ = dh.Sync()
	_ = dh.Close()
	if e = os.Remove(journal); e != nil {
		return Manifest{}, e
	}
	dh, _ = os.Open(dir)
	_ = dh.Sync()
	_ = dh.Close()
	return man, nil
}
func MigrateManifest(dir string, from string) (Manifest, error) {
	if from == ManifestVersion {
		return Verify(dir)
	}
	return Manifest{}, conflict("unsupported_manifest_migration", from, "only explicit supported migrations may change manifest versions")
}
