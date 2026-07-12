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
	"unicode/utf8"

	"autosql/pkg/source"
	"golang.org/x/sys/unix"
)

const (
	ManifestVersion = "autosql.migration-directory/v1"
	LegacyVersion   = "autosql.migration-directory/v0"
	ManifestFile    = ".autosql-manifest.json"
	maxFile         = 8 << 20
	maxManifest     = 16 << 20
	genDir          = ".autosql-generations"
	lockFile        = ".autosql.lock"
	journalFile     = ".autosql-journal.json"
)

var (
	ErrInvalid  = errors.New("invalid migration directory")
	ErrConflict = errors.New("migration directory conflict")
)

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
	n := [3]uint64{}
	for i, x := range m[1:4] {
		if x != "" {
			v, err := strconv.ParseUint(x, 10, 64)
			if err != nil {
				return Version{}, ErrInvalid
			}
			n[i] = v
		}
	}
	if m[4] != "" {
		for _, id := range strings.Split(m[4], ".") {
			if id == "" {
				return Version{}, fmt.Errorf("%w: empty prerelease identifier", ErrInvalid)
			}
			numeric := strings.IndexFunc(id, func(r rune) bool { return r < '0' || r > '9' }) == -1
			if numeric {
				if len(id) > 1 && id[0] == '0' {
					return Version{}, fmt.Errorf("%w: prerelease numeric leading zero", ErrInvalid)
				}
				if _, e := strconv.ParseUint(id, 10, 64); e != nil {
					return Version{}, fmt.Errorf("%w: prerelease numeric overflow", ErrInvalid)
				}
			}
		}
	}
	return Version{n[0], n[1], n[2], m[4]}, nil
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
	va, wa := strings.Split(v.Pre, "."), strings.Split(w.Pre, ".")
	for i := 0; i < len(va) && i < len(wa); i++ {
		vn, ve := strconv.ParseUint(va[i], 10, 64)
		wn, we := strconv.ParseUint(wa[i], 10, 64)
		if ve == nil && we == nil {
			if vn < wn {
				return -1
			}
			if vn > wn {
				return 1
			}
			continue
		}
		if ve == nil {
			return -1
		}
		if we == nil {
			return 1
		}
		if c := strings.Compare(va[i], wa[i]); c != 0 {
			return c
		}
	}
	if len(va) < len(wa) {
		return -1
	}
	if len(va) > len(wa) {
		return 1
	}
	return 0
}

type TransactionMode string

const (
	TransactionAuto      TransactionMode = "auto"
	TransactionRequired  TransactionMode = "required"
	TransactionForbidden TransactionMode = "forbidden"
)

type Directives struct {
	Transaction       TransactionMode `json:"transaction"`
	PlanDigest        string          `json:"plan_digest,omitempty"`
	CheckDigest       string          `json:"check_digest,omitempty"`
	BundleDigest      string          `json:"bundle_digest,omitempty"`
	CheckBundleDigest string          `json:"check_bundle_digest,omitempty"`
}
type Statement struct {
	Ordinal int    `json:"ordinal"`
	Line    int    `json:"line"`
	Column  int    `json:"column"`
	Digest  string `json:"digest"`
}
type Migration struct {
	File            string      `json:"file"`
	Version         string      `json:"version"`
	Name            string      `json:"name"`
	SQLDigest       string      `json:"sql_digest"`
	Directives      Directives  `json:"directives"`
	DirectiveDigest string      `json:"directive_digest"`
	Statements      []Statement `json:"statements"`
	BoundaryDigest  string      `json:"boundary_digest"`
	Parents         []string    `json:"parents"`
	NonLinear       bool        `json:"nonlinear,omitempty"`
	ChainDigest     string      `json:"chain_digest"`
}
type Manifest struct {
	Version    string      `json:"version"`
	Generation string      `json:"generation"`
	Entries    []Migration `json:"entries"`
	Digest     string      `json:"digest"`
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

// Ops supports deterministic fault injection for every durability boundary.
// Nil functions use the system implementation.
type Ops struct {
	Write    func(fd int, p []byte) (int, error)
	Fsync    func(fd int) error
	Renameat func(olddirfd int, oldpath string, newdirfd int, newpath string) error
}

func (o Ops) write(fd int, p []byte) (int, error) {
	if o.Write != nil {
		return o.Write(fd, p)
	}
	return unix.Write(fd, p)
}
func (o Ops) sync(fd int) error {
	if o.Fsync != nil {
		return o.Fsync(fd)
	}
	return unix.Fsync(fd)
}
func (o Ops) rename(a int, ap string, b int, bp string) error {
	if o.Renameat != nil {
		return o.Renameat(a, ap, b, bp)
	}
	return unix.Renameat(a, ap, b, bp)
}

var fileRE = regexp.MustCompile(`^(?:V)?([0-9]+(?:\.[0-9]+){0,2}(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?)__([-A-Za-z0-9][-.A-Za-z0-9_]*)\.sql$`)
var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

func digest(domain string, b []byte) string {
	x := sha256.Sum256(append([]byte("autosql.migrate."+domain+"/v1\x00"), b...))
	return "sha256:" + hex.EncodeToString(x[:])
}
func canonical(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err)
	}
	return b
}
func validDigest(s string) bool { return s == "" || digestRE.MatchString(s) }

func parseDirectives(sql []byte) (Directives, error) {
	d := Directives{Transaction: TransactionAuto}
	seen := map[string]bool{}
	header := true
	for i, line := range strings.Split(string(sql), "\n") {
		t := strings.TrimSpace(line)
		if t == "" || (header && strings.HasPrefix(t, "--") && !strings.HasPrefix(t, "-- autosql:")) {
			continue
		}
		if !strings.HasPrefix(t, "-- autosql:") {
			header = false
			continue
		}
		if !header {
			return d, fmt.Errorf("%w: directive outside header at line %d", ErrInvalid, i+1)
		}
		kv := strings.TrimSpace(strings.TrimPrefix(t, "-- autosql:"))
		k, v, ok := strings.Cut(kv, "=")
		k = strings.ReplaceAll(strings.TrimSpace(k), "_", "-")
		v = strings.TrimSpace(v)
		if !ok || seen[k] {
			return d, fmt.Errorf("%w: duplicate or malformed directive at line %d", ErrInvalid, i+1)
		}
		seen[k] = true
		switch k {
		case "transaction":
			d.Transaction = TransactionMode(v)
			if d.Transaction != TransactionAuto && d.Transaction != TransactionRequired && d.Transaction != TransactionForbidden {
				return d, fmt.Errorf("%w: transaction directive", ErrInvalid)
			}
		case "plan-digest":
			d.PlanDigest = v
		case "check-digest":
			d.CheckDigest = v
		case "bundle-digest":
			d.BundleDigest = v
		case "check-bundle-digest":
			d.CheckBundleDigest = v
		default:
			return d, fmt.Errorf("%w: unknown directive %q", ErrInvalid, k)
		}
	}
	if !validDigest(d.PlanDigest) || !validDigest(d.CheckDigest) || !validDigest(d.BundleDigest) || !validDigest(d.CheckBundleDigest) {
		return d, fmt.Errorf("%w: directive digest", ErrInvalid)
	}
	return d, nil
}
func parseFile(f File) (Migration, error) {
	name, sql := f.Name, f.SQL
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
	dirs, e := parseDirectives(sql)
	if e != nil {
		return Migration{}, e
	}
	split, e := source.SplitSQL(name, string(sql))
	if e != nil {
		return Migration{}, e
	}
	stmts := make([]Statement, len(split))
	for i, s := range split {
		stmts[i] = Statement{i + 1, s.Position.Line, s.Position.Column, digest("statement", []byte(s.SQL))}
	}
	dd := digest("directives", canonical(dirs))
	bd := digest("boundaries", canonical(stmts))
	parents := append([]string(nil), f.Parents...)
	return Migration{File: name, Version: v.String(), Name: m[2], SQLDigest: digest("sql", sql), Directives: dirs, DirectiveDigest: dd, Statements: stmts, BoundaryDigest: bd, Parents: parents, NonLinear: f.NonLinear}, nil
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
		e, err := parseFile(f)
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
		c := a.Compare(b)
		if c == 0 {
			return entries[i].File < entries[j].File
		}
		return c < 0
	})
	byVersion := map[string]int{}
	for i := range entries {
		byVersion[entries[i].Version] = i
	}
	for i := range entries {
		e := &entries[i]
		if i == 0 {
			if len(e.Parents) > 0 {
				return Manifest{}, conflict("root_parent", e.File, "remove root parents")
			}
			e.Parents = []string{}
		} else if len(e.Parents) == 0 {
			e.Parents = []string{entries[i-1].Version}
		}
		seenParent := map[string]bool{}
		for _, p := range e.Parents {
			if seenParent[p] {
				return Manifest{}, conflict("duplicate_parent", e.File, "list every parent exactly once")
			}
			seenParent[p] = true
			pi, ok := byVersion[p]
			if !ok {
				return Manifest{}, conflict("missing_parent", p, "restore the parent migration or repair the declared graph")
			}
			if pi >= i {
				return Manifest{}, conflict("cyclic_or_forward_parent", e.File, "parents must precede their child")
			}
		}
		linear := i == 0 && len(e.Parents) == 0 || i > 0 && len(e.Parents) == 1 && e.Parents[0] == entries[i-1].Version
		if !linear && (!e.NonLinear || len(e.Parents) < 1) {
			return Manifest{}, conflict("nonlinear_chain", e.File, "declare nonlinear=true with explicit existing parents")
		}
		if linear && e.NonLinear {
			return Manifest{}, conflict("spurious_nonlinear", e.File, "remove nonlinear authorization from a linear edge")
		}
		parentChains := make([]string, len(e.Parents))
		for j, p := range e.Parents {
			parentChains[j] = entries[byVersion[p]].ChainDigest
		}
		sort.Strings(parentChains)
		material := struct {
			Version, File, SQL, Directives, Boundaries string
			Parents, ParentChains                      []string
			NonLinear                                  bool
		}{e.Version, e.File, e.SQLDigest, e.DirectiveDigest, e.BoundaryDigest, append([]string(nil), e.Parents...), parentChains, e.NonLinear}
		e.ChainDigest = digest("chain", canonical(material))
	}
	// The generation is derived from all semantic and byte digests, not wall time.
	seed := struct {
		Version string
		Entries []Migration
	}{ManifestVersion, entries}
	g := strings.TrimPrefix(digest("generation", canonical(seed)), "sha256:")
	man := Manifest{Version: ManifestVersion, Generation: g, Entries: entries}
	copy := man
	copy.Digest = ""
	man.Digest = digest("manifest", canonical(copy))
	return man, nil
}

// rejectDuplicates validates the entire JSON token stream, including nested objects.
func rejectDuplicates(raw []byte) error {
	d := json.NewDecoder(bytes.NewReader(raw))
	d.UseNumber()
	var walk func() error
	walk = func() error {
		t, e := d.Token()
		if e != nil {
			return e
		}
		switch x := t.(type) {
		case json.Delim:
			switch x {
			case '{':
				seen := map[string]bool{}
				for d.More() {
					k, e := d.Token()
					if e != nil {
						return e
					}
					ks, ok := k.(string)
					if !ok || seen[ks] {
						return fmt.Errorf("%w: duplicate JSON key %q", ErrInvalid, ks)
					}
					seen[ks] = true
					if e = walk(); e != nil {
						return e
					}
				}
				_, e = d.Token()
				return e
			case '[':
				for d.More() {
					if e := walk(); e != nil {
						return e
					}
				}
				_, e = d.Token()
				return e
			}
		}
		return nil
	}
	if e := walk(); e != nil {
		return e
	}
	if _, e := d.Token(); e != io.EOF {
		return ErrInvalid
	}
	return nil
}
func strictManifest(raw []byte) (Manifest, error) {
	if len(raw) > maxManifest || !utf8.Valid(raw) {
		return Manifest{}, ErrInvalid
	}
	if e := rejectDuplicates(raw); e != nil {
		return Manifest{}, e
	}
	var m Manifest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&m); e != nil {
		return m, e
	}
	if m.Version != ManifestVersion {
		return m, conflict("manifest_version", m.Version, "run MigrateManifest explicitly")
	}
	if m.Generation == "" || strings.ContainsAny(m.Generation, "/\\") || len(m.Generation) != 64 {
		return m, ErrInvalid
	}
	copy := m
	copy.Digest = ""
	if digest("manifest", canonical(copy)) != m.Digest {
		return m, conflict("manifest_digest", ManifestFile, "restore or explicitly regenerate the manifest")
	}
	if !bytes.Equal(raw, canonical(m)) {
		return m, conflict("noncanonical_manifest", ManifestFile, "restore canonical manifest bytes")
	}
	return m, nil
}

func openRoot(dir string) (int, error) {
	fd, e := unix.Open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return -1, conflict("unsafe_directory", dir, "use a trusted non-symlink directory")
	}
	var st unix.Stat_t
	if e = unix.Fstat(fd, &st); e != nil {
		unix.Close(fd)
		return -1, e
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0022 != 0 || int(st.Uid) != os.Geteuid() {
		unix.Close(fd)
		return -1, conflict("unsafe_directory", dir, "use an owner-controlled directory without group/world write")
	}
	return fd, nil
}
func openRegularAt(parent int, name string) (int, error) {
	if filepath.Base(name) != name || strings.Contains(name, "..") {
		return -1, ErrInvalid
	}
	fd, e := unix.Openat(parent, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return -1, e
	}
	var st unix.Stat_t
	if e = unix.Fstat(fd, &st); e != nil {
		unix.Close(fd)
		return -1, e
	}
	if st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0022 != 0 || st.Nlink != 1 || int(st.Uid) != os.Geteuid() {
		unix.Close(fd)
		return -1, conflict("unsafe_file", name, "use one owner-controlled regular non-linked file")
	}
	return fd, nil
}
func readFD(fd int, limit int) ([]byte, error) {
	f := os.NewFile(uintptr(fd), "")
	defer f.Close()
	r := io.LimitReader(f, int64(limit)+1)
	b, e := io.ReadAll(r)
	if e == nil && len(b) > limit {
		return nil, ErrInvalid
	}
	return b, e
}
func readAt(parent int, name string, limit int) ([]byte, error) {
	fd, e := openRegularAt(parent, name)
	if e != nil {
		return nil, e
	}
	return readFD(fd, limit)
}
func openLock(root int, exclusive bool) (int, error) {
	fd, e := unix.Openat(root, lockFile, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if e != nil {
		return -1, e
	}
	var st unix.Stat_t
	if e = unix.Fstat(fd, &st); e != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0077 != 0 || st.Nlink != 1 || int(st.Uid) != os.Geteuid() {
		unix.Close(fd)
		return -1, conflict("unsafe_lock", lockFile, "restore the private owner-controlled lock")
	}
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	if e = unix.Flock(fd, how); e != nil {
		unix.Close(fd)
		return -1, e
	}
	return fd, nil
}
func closeLock(fd int) { _ = unix.Flock(fd, unix.LOCK_UN); _ = unix.Close(fd) }
func loadAt(root int) (Manifest, []byte, error) {
	raw, e := readAt(root, ManifestFile, maxManifest)
	if e != nil {
		return Manifest{}, nil, e
	}
	m, e := strictManifest(raw)
	return m, raw, e
}
func openGeneration(root int, g string) (int, error) {
	base, e := unix.Openat(root, genDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return -1, e
	}
	if e = validateDirFD(base, genDir); e != nil {
		unix.Close(base)
		return -1, e
	}
	defer unix.Close(base)
	fd, e := unix.Openat(base, g, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return -1, e
	}
	if e = validateDirFD(fd, g); e != nil {
		unix.Close(fd)
		return -1, e
	}
	return fd, nil
}

func validateDirFD(fd int, name string) error {
	var st unix.Stat_t
	if e := unix.Fstat(fd, &st); e != nil {
		return e
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0022 != 0 || int(st.Uid) != os.Geteuid() {
		return conflict("unsafe_generation", name, "restore the immutable owner-controlled generation")
	}
	return nil
}
func snapshotAt(root int) (Snapshot, error) {
	m, raw, e := loadAt(root)
	if e != nil {
		return Snapshot{}, e
	}
	g, e := openGeneration(root, m.Generation)
	if e != nil {
		return Snapshot{}, e
	}
	defer unix.Close(g)
	// SQL outside the published generation is ambiguous/tampered input. Within
	// the generation, every regular SQL file must be represented exactly once.
	if names, er := namesAt(root); er != nil {
		return Snapshot{}, er
	} else {
		for _, name := range names {
			if strings.HasSuffix(strings.ToLower(name), ".sql") {
				return Snapshot{}, conflict("untracked_file", name, "remove root SQL and use an authorized generation update")
			}
		}
	}
	expected := map[string]bool{}
	for _, entry := range m.Entries {
		expected[entry.File] = true
	}
	if names, er := namesAt(g); er != nil {
		return Snapshot{}, er
	} else {
		for _, name := range names {
			if strings.HasSuffix(strings.ToLower(name), ".sql") && !expected[name] {
				return Snapshot{}, conflict("untracked_file", name, "restore the exact published generation")
			}
		}
	}
	out := Snapshot{Manifest: m, ManifestBytes: append([]byte(nil), raw...), Files: map[string][]byte{}}
	for _, entry := range m.Entries {
		b, e := readAt(g, entry.File, maxFile)
		if e != nil {
			return Snapshot{}, e
		}
		out.Files[entry.File] = b
	}
	return out, nil
}

func namesAt(fd int) ([]string, error) {
	dup, e := unix.Dup(fd)
	if e != nil {
		return nil, e
	}
	f := os.NewFile(uintptr(dup), "directory")
	defer f.Close()
	entries, e := f.ReadDir(-1)
	if e != nil {
		return nil, e
	}
	names := make([]string, len(entries))
	for i := range entries {
		names[i] = entries[i].Name()
	}
	return names, nil
}
func Load(dir string) (Manifest, error) {
	root, e := openRoot(dir)
	if e != nil {
		return Manifest{}, e
	}
	defer unix.Close(root)
	l, e := openLock(root, false)
	if e != nil {
		return Manifest{}, e
	}
	defer closeLock(l)
	m, _, e := loadAt(root)
	return m, e
}
func LoadSnapshot(dir string) (Snapshot, error) {
	root, e := openRoot(dir)
	if e != nil {
		return Snapshot{}, e
	}
	defer unix.Close(root)
	l, e := openLock(root, false)
	if e != nil {
		return Snapshot{}, e
	}
	defer closeLock(l)
	return snapshotAt(root)
}
func Verify(dir string) (Manifest, error) {
	s, e := LoadSnapshot(dir)
	if e != nil {
		return Manifest{}, e
	}
	files := make([]File, 0, len(s.Manifest.Entries))
	for _, entry := range s.Manifest.Entries {
		files = append(files, File{Name: entry.File, SQL: s.Files[entry.File], Parents: append([]string(nil), entry.Parents...), NonLinear: entry.NonLinear})
	}
	actual, e := build(files)
	if e != nil {
		return Manifest{}, e
	}
	if !bytes.Equal(canonical(s.Manifest), canonical(actual)) {
		return Manifest{}, conflict("content_changed", dir, "restore the byte-exact migration generation or perform an authorized update")
	}
	return s.Manifest, nil
}

type journal struct {
	Version        int    `json:"version"`
	Generation     string `json:"generation"`
	ManifestDigest string `json:"manifest_digest"`
}

func writeAll(fd int, p []byte, ops Ops) error {
	for len(p) > 0 {
		n, e := ops.write(fd, p)
		if n > 0 {
			p = p[n:]
		}
		if e != nil {
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
func writeAt(parent int, name string, p []byte, mode uint32, ops Ops) error {
	fd, e := unix.Openat(parent, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	if e != nil {
		return e
	}
	ok := false
	defer func() {
		if fd >= 0 {
			_ = unix.Close(fd)
		}
		if !ok {
			_ = unix.Unlinkat(parent, name, 0)
		}
	}()
	if e = writeAll(fd, p, ops); e != nil {
		return e
	}
	if e = ops.sync(fd); e != nil {
		return e
	}
	if e = unix.Close(fd); e != nil {
		fd = -1
		return e
	}
	fd = -1
	ok = true
	return nil
}

func ensureFileAt(parent int, name string, p []byte, mode uint32, ops Ops) error {
	e := writeAt(parent, name, p, mode, ops)
	if e == nil {
		return nil
	}
	if !errors.Is(e, unix.EEXIST) {
		return e
	}
	got, re := readAt(parent, name, len(p))
	if re != nil {
		return re
	}
	if !bytes.Equal(got, p) {
		return conflict("stale_staging_file", name, "retain and inspect the foreign staging bytes")
	}
	return nil
}
func mkdirAt(parent int, name string, mode uint32) error {
	e := unix.Mkdirat(parent, name, mode)
	if e == unix.EEXIST {
		return nil
	}
	return e
}

// Update atomically publishes an immutable generation. Once a directory exists,
// ExpectedManifestDigest is mandatory; an empty value is only the create CAS.
func Update(dir string, req UpdateRequest) (Manifest, error) { return UpdateWithOps(dir, req, Ops{}) }
func UpdateWithOps(dir string, req UpdateRequest, ops Ops) (Manifest, error) {
	if req.ManifestVersion != "" && req.ManifestVersion != ManifestVersion {
		return Manifest{}, conflict("manifest_version", req.ManifestVersion, "migrate explicitly")
	}
	root, e := openRoot(dir)
	if e != nil {
		return Manifest{}, e
	}
	defer unix.Close(root)
	lock, e := openLock(root, true)
	if e != nil {
		return Manifest{}, e
	}
	defer closeLock(lock)
	return updateLocked(root, dir, req, ops, false)
}

func updateLocked(root int, dir string, req UpdateRequest, ops Ops, allowLegacy bool) (Manifest, error) {
	if _, je := readAt(root, journalFile, maxFile); je == nil {
		if e := recoverLocked(root); e != nil {
			return Manifest{}, e
		}
	} else if !errors.Is(je, unix.ENOENT) {
		return Manifest{}, je
	}
	current, _, ce := loadAt(root)
	exists := ce == nil
	legacy := allowLegacy && ce != nil
	if ce != nil && !errors.Is(ce, unix.ENOENT) && !legacy {
		return Manifest{}, ce
	}
	if exists {
		if req.ExpectedManifestDigest == "" {
			return Manifest{}, conflict("compare_and_swap_required", dir, "reload the snapshot and provide its digest")
		}
		if current.Digest != req.ExpectedManifestDigest {
			return Manifest{}, conflict("compare_and_swap", dir, "reload the snapshot and retry")
		}
	} else if req.ExpectedManifestDigest != "" && !legacy {
		return Manifest{}, conflict("compare_and_swap", dir, "the directory has no published manifest")
	}
	man, e := build(req.Files)
	if e != nil {
		return Manifest{}, e
	}
	if len(canonical(man)) > maxManifest {
		return Manifest{}, fmt.Errorf("%w: manifest size", ErrInvalid)
	}
	if e = mkdirAt(root, genDir, 0700); e != nil {
		return Manifest{}, e
	}
	base, e := unix.Openat(root, genDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return Manifest{}, e
	}
	defer unix.Close(base)
	if e = validateDirFD(base, genDir); e != nil {
		return Manifest{}, e
	}
	if e = unix.Mkdirat(base, man.Generation, 0700); e != nil && e != unix.EEXIST {
		return Manifest{}, e
	}
	g, e := unix.Openat(base, man.Generation, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return Manifest{}, e
	}
	defer unix.Close(g)
	if e = validateDirFD(g, man.Generation); e != nil {
		return Manifest{}, e
	}
	// Existing identical deterministic generations are verified, never overwritten.
	for _, f := range req.Files {
		existing, re := readAt(g, f.Name, maxFile)
		if re == nil {
			if !bytes.Equal(existing, f.SQL) {
				return Manifest{}, conflict("generation_collision", f.Name, "restore the deterministic generation")
			}
			continue
		}
		if !errors.Is(re, unix.ENOENT) {
			return Manifest{}, re
		}
		if e = writeAt(g, f.Name, f.SQL, 0600, ops); e != nil {
			return Manifest{}, e
		}
	}
	want := map[string]bool{}
	for _, f := range req.Files {
		want[f.Name] = true
	}
	gnames, e := namesAt(g)
	if e != nil {
		return Manifest{}, e
	}
	for _, name := range gnames {
		if !want[name] {
			return Manifest{}, conflict("generation_collision", name, "remove foreign bytes from the unpublished generation")
		}
	}
	if e = ops.sync(g); e != nil {
		return Manifest{}, e
	}
	if e = ops.sync(base); e != nil {
		return Manifest{}, e
	}
	j := journal{1, man.Generation, man.Digest}
	jr := canonical(j)
	if e = writeAt(root, journalFile, jr, 0600, ops); e != nil {
		return Manifest{}, e
	}
	if e = ops.sync(root); e != nil {
		return Manifest{}, e
	}
	tmp := ManifestFile + "." + man.Generation + ".new"
	if e = ensureFileAt(root, tmp, canonical(man), 0600, ops); e != nil {
		return Manifest{}, e
	}
	if e = ops.rename(root, tmp, root, ManifestFile); e != nil {
		return Manifest{}, e
	}
	if e = ops.sync(root); e != nil {
		return Manifest{}, e
	}
	if e = unix.Unlinkat(root, journalFile, 0); e != nil {
		return Manifest{}, e
	}
	if e = ops.sync(root); e != nil {
		return Manifest{}, e
	}
	return man, nil
}

// Recover removes only a canonical journal whose referenced generation and
// manifest are demonstrably published. An unpublished valid generation is
// retained for forensic inspection and retry; no migration bytes are deleted.
func Recover(dir string) error {
	root, e := openRoot(dir)
	if e != nil {
		return e
	}
	defer unix.Close(root)
	lock, e := openLock(root, true)
	if e != nil {
		return e
	}
	defer closeLock(lock)
	return recoverLocked(root)
}

func recoverLocked(root int) error {
	raw, e := readAt(root, journalFile, maxFile)
	if errors.Is(e, unix.ENOENT) {
		return nil
	}
	if e != nil {
		return e
	}
	if e = rejectDuplicates(raw); e != nil {
		return conflict("dirty_journal", journalFile, "inspect and restore the last trusted snapshot")
	}
	var j journal
	if json.Unmarshal(raw, &j) != nil || j.Version != 1 || len(j.Generation) != 64 || !digestRE.MatchString(j.ManifestDigest) || !bytes.Equal(raw, canonical(j)) {
		return conflict("dirty_journal", journalFile, "inspect and restore the last trusted snapshot")
	}
	m, _, me := loadAt(root)
	if me != nil || m.Digest != j.ManifestDigest || m.Generation != j.Generation {
		return conflict("unpublished_generation", j.Generation, "retry the authorized update or retain the generation for forensic recovery")
	}
	if e = unix.Unlinkat(root, journalFile, 0); e != nil {
		return e
	}
	return unix.Fsync(root)
}

// MigrateManifest upgrades the explicit legacy v0 root-file format atomically.
// The v0 manifest uses {version,entries:[{file,parents,nonlinear}],digest}; its
// digest is sha256 over the canonical object with an empty digest field.
func MigrateManifest(dir, from string) (Manifest, error) {
	if from == ManifestVersion {
		return Verify(dir)
	}
	if from != LegacyVersion {
		return Manifest{}, conflict("unsupported_manifest_migration", from, "only explicit supported migrations may change manifest versions")
	}
	root, e := openRoot(dir)
	if e != nil {
		return Manifest{}, e
	}
	defer unix.Close(root)
	lock, e := openLock(root, true)
	if e != nil {
		return Manifest{}, e
	}
	defer closeLock(lock)
	raw, e := readAt(root, ManifestFile, maxManifest)
	if e != nil {
		return Manifest{}, e
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
	if e = rejectDuplicates(raw); e != nil {
		return Manifest{}, e
	}
	var old lm
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if d.Decode(&old) != nil || old.Version != LegacyVersion || !bytes.Equal(raw, canonical(old)) {
		return Manifest{}, ErrInvalid
	}
	oc := old
	oc.Digest = ""
	if digest("manifest-v0", canonical(oc)) != old.Digest {
		return Manifest{}, conflict("manifest_digest", ManifestFile, "restore the legacy manifest")
	}
	files := make([]File, len(old.Entries))
	for i, x := range old.Entries {
		b, e := readAt(root, x.File, maxFile)
		if e != nil {
			return Manifest{}, e
		}
		files[i] = File{x.File, b, x.Parents, x.NonLinear}
	}
	// Preserve an immutable fixture before publishing; the active v0 manifest
	// remains continuously readable until the single v1 rename.
	if e = ensureFileAt(root, ManifestFile+".v0", raw, 0600, Ops{}); e != nil {
		return Manifest{}, e
	}
	man, e := updateLocked(root, dir, UpdateRequest{Files: files}, Ops{}, true)
	if e != nil {
		return Manifest{}, e
	}
	// Readers take the same lock, so legacy root files disappear as part of the
	// same logical publication and cannot be mistaken for a mixed generation.
	for _, x := range old.Entries {
		if e = unix.Unlinkat(root, x.File, 0); e != nil {
			return Manifest{}, e
		}
	}
	if e = unix.Fsync(root); e != nil {
		return Manifest{}, e
	}
	return man, nil
}
