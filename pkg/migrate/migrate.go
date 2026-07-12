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
	Open     func(path string, flags int, mode uint32) (int, error)
	Openat   func(dirfd int, path string, flags int, mode uint32) (int, error)
	Close    func(fd int) error
	Mkdirat  func(dirfd int, path string, mode uint32) error
	Unlinkat func(dirfd int, path string, flags int) error
	Flock    func(fd int, how int) error
	Write    func(fd int, p []byte) (int, error)
	Fsync    func(fd int) error
	Renameat func(olddirfd int, oldpath string, newdirfd int, newpath string) error
}

func (o Ops) open(path string, flags int, mode uint32) (int, error) {
	for {
		var fd int
		var e error
		if o.Open != nil {
			fd, e = o.Open(path, flags, mode)
		} else {
			fd, e = unix.Open(path, flags, mode)
		}
		if e != unix.EINTR {
			return fd, e
		}
	}
}
func (o Ops) openat(fd int, path string, flags int, mode uint32) (int, error) {
	for {
		var n int
		var e error
		if o.Openat != nil {
			n, e = o.Openat(fd, path, flags, mode)
		} else {
			n, e = unix.Openat(fd, path, flags, mode)
		}
		if e != unix.EINTR {
			return n, e
		}
	}
}
func (o Ops) close(fd int) error {
	if o.Close != nil {
		return o.Close(fd)
	}
	return unix.Close(fd)
}
func (o Ops) mkdirat(fd int, path string, mode uint32) error {
	if o.Mkdirat != nil {
		return o.Mkdirat(fd, path, mode)
	}
	return unix.Mkdirat(fd, path, mode)
}
func (o Ops) unlinkat(fd int, path string, flags int) error {
	if o.Unlinkat != nil {
		return o.Unlinkat(fd, path, flags)
	}
	return unix.Unlinkat(fd, path, flags)
}
func (o Ops) flock(fd int, how int) error {
	for {
		var e error
		if o.Flock != nil {
			e = o.Flock(fd, how)
		} else {
			e = unix.Flock(fd, how)
		}
		if e != unix.EINTR {
			return e
		}
	}
}

func (o Ops) write(fd int, p []byte) (int, error) {
	if o.Write != nil {
		return o.Write(fd, p)
	}
	return unix.Write(fd, p)
}
func (o Ops) sync(fd int) error {
	for {
		var e error
		if o.Fsync != nil {
			e = o.Fsync(fd)
		} else {
			e = unix.Fsync(fd)
		}
		if e != unix.EINTR {
			return e
		}
	}
}
func (o Ops) rename(a int, ap string, b int, bp string) error {
	if o.Renameat != nil {
		for {
			e := o.Renameat(a, ap, b, bp)
			if e != unix.EINTR {
				return e
			}
		}
	}
	for {
		e := unix.Renameat(a, ap, b, bp)
		if e != unix.EINTR {
			return e
		}
	}
}

var fileRE = regexp.MustCompile(`^(?:V)?([0-9]+(?:\.[0-9]+){0,2}(?:-[0-9A-Za-z][0-9A-Za-z.-]*)?)__([-A-Za-z0-9][-.A-Za-z0-9_]*)\.sql$`)
var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
var generationRE = regexp.MustCompile(`^[0-9a-f]{64}$`)

type chainMaterial struct {
	Version, File, SQL, Directives, Boundaries string
	Parents, ParentChains                      []string
	NonLinear                                  bool
}

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
	sort.Strings(parents)
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
		material := chainMaterial{e.Version, e.File, e.SQLDigest, e.DirectiveDigest, e.BoundaryDigest, append([]string(nil), e.Parents...), parentChains, e.NonLinear}
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
	if !generationRE.MatchString(m.Generation) {
		return m, ErrInvalid
	}
	if e := validateManifestStructure(m); e != nil {
		return m, e
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

func validateManifestStructure(m Manifest) error {
	seenFile, seenVersion := map[string]bool{}, map[string]bool{}
	byVersion := map[string]int{}
	for i := range m.Entries {
		e := &m.Entries[i]
		if filepath.Base(e.File) != e.File || strings.Contains(e.File, "..") || strings.ContainsAny(e.File, "/\\") {
			return ErrInvalid
		}
		fm := fileRE.FindStringSubmatch(e.File)
		if fm == nil {
			return ErrInvalid
		}
		v, err := ParseVersion(fm[1])
		if err != nil || v.String() != e.Version || fm[2] != e.Name {
			return ErrInvalid
		}
		fold := strings.ToLower(e.File)
		if seenFile[fold] || seenVersion[e.Version] {
			return conflict("duplicate_entry", e.File, "restore unique sorted manifest entries")
		}
		seenFile[fold] = true
		seenVersion[e.Version] = true
		byVersion[e.Version] = i
		if i > 0 {
			pv, _ := ParseVersion(m.Entries[i-1].Version)
			if pv.Compare(v) >= 0 {
				return conflict("entry_order", e.File, "restore canonical version ordering")
			}
		}
		if !digestRE.MatchString(e.SQLDigest) || !digestRE.MatchString(e.DirectiveDigest) || !digestRE.MatchString(e.BoundaryDigest) || !digestRE.MatchString(e.ChainDigest) {
			return ErrInvalid
		}
		if e.Directives.Transaction != TransactionAuto && e.Directives.Transaction != TransactionRequired && e.Directives.Transaction != TransactionForbidden {
			return ErrInvalid
		}
		if !validDigest(e.Directives.PlanDigest) || !validDigest(e.Directives.CheckDigest) || !validDigest(e.Directives.BundleDigest) || !validDigest(e.Directives.CheckBundleDigest) {
			return ErrInvalid
		}
		if digest("directives", canonical(e.Directives)) != e.DirectiveDigest {
			return conflict("directive_digest", e.File, "restore canonical directive metadata")
		}
		for j, s := range e.Statements {
			if s.Ordinal != j+1 || s.Line < 1 || s.Column < 1 || !digestRE.MatchString(s.Digest) {
				return ErrInvalid
			}
		}
		if digest("boundaries", canonical(e.Statements)) != e.BoundaryDigest {
			return conflict("boundary_digest", e.File, "restore canonical statement boundaries")
		}
		for j, p := range e.Parents {
			if _, err := ParseVersion(p); err != nil {
				return ErrInvalid
			}
			if j > 0 && e.Parents[j-1] >= p {
				return conflict("parent_order", e.File, "sort and deduplicate parent versions")
			}
		}
	}
	for i := range m.Entries {
		e := &m.Entries[i]
		if i == 0 && len(e.Parents) != 0 || i > 0 && len(e.Parents) == 0 {
			return ErrInvalid
		}
		seen := map[string]bool{}
		parentChains := make([]string, len(e.Parents))
		for j, p := range e.Parents {
			pi, ok := byVersion[p]
			if !ok || pi >= i || seen[p] {
				return ErrInvalid
			}
			seen[p] = true
			parentChains[j] = m.Entries[pi].ChainDigest
		}
		sort.Strings(parentChains)
		linear := i == 0 || len(e.Parents) == 1 && e.Parents[0] == m.Entries[i-1].Version
		if linear == e.NonLinear {
			return ErrInvalid
		}
		material := chainMaterial{e.Version, e.File, e.SQLDigest, e.DirectiveDigest, e.BoundaryDigest, append([]string(nil), e.Parents...), parentChains, e.NonLinear}
		if digest("chain", canonical(material)) != e.ChainDigest {
			return conflict("chain_digest", e.File, "restore the canonical ancestry chain")
		}
	}
	seed := struct {
		Version string
		Entries []Migration
	}{ManifestVersion, m.Entries}
	if strings.TrimPrefix(digest("generation", canonical(seed)), "sha256:") != m.Generation {
		return conflict("generation_digest", m.Generation, "restore the canonical generation")
	}
	return nil
}

func openRoot(dir string) (int, error) {
	return openRootWithOps(dir, Ops{})
}
func openRootWithOps(dir string, ops Ops) (int, error) {
	fd, e := ops.open(dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return -1, conflict("unsafe_directory", dir, "use a trusted non-symlink directory")
	}
	var st unix.Stat_t
	if e = unix.Fstat(fd, &st); e != nil {
		_ = ops.close(fd)
		return -1, e
	}
	if st.Mode&unix.S_IFMT != unix.S_IFDIR || st.Mode&0022 != 0 || int(st.Uid) != os.Geteuid() {
		_ = ops.close(fd)
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
	return openLockWithOps(root, exclusive, Ops{})
}
func openLockWithOps(root int, exclusive bool, ops Ops) (int, error) {
	fd, e := ops.openat(root, lockFile, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if e != nil {
		return -1, e
	}
	var st unix.Stat_t
	if e = unix.Fstat(fd, &st); e != nil || st.Mode&unix.S_IFMT != unix.S_IFREG || st.Mode&0077 != 0 || st.Nlink != 1 || int(st.Uid) != os.Geteuid() {
		_ = ops.close(fd)
		return -1, conflict("unsafe_lock", lockFile, "restore the private owner-controlled lock")
	}
	how := unix.LOCK_SH
	if exclusive {
		how = unix.LOCK_EX
	}
	if e = ops.flock(fd, how); e != nil {
		_ = ops.close(fd)
		return -1, e
	}
	return fd, nil
}
func closeLock(fd int)                 { _ = unix.Flock(fd, unix.LOCK_UN); _ = unix.Close(fd) }
func closeLockWithOps(fd int, ops Ops) { _ = ops.flock(fd, unix.LOCK_UN); _ = ops.close(fd) }
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
	files, e := verifyGenerationAt(root, m)
	if e != nil {
		return Snapshot{}, e
	}
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
	return Snapshot{Manifest: m, ManifestBytes: append([]byte(nil), raw...), Files: files}, nil
}

func verifyGenerationAt(root int, m Manifest) (map[string][]byte, error) {
	g, e := openGeneration(root, m.Generation)
	if e != nil {
		return nil, e
	}
	defer unix.Close(g)
	expected := map[string]bool{}
	for _, entry := range m.Entries {
		expected[entry.File] = true
	}
	names, e := namesAt(g)
	if e != nil {
		return nil, e
	}
	for _, name := range names {
		if !expected[name] {
			return nil, conflict("untracked_file", name, "restore the exact published generation")
		}
	}
	out := map[string][]byte{}
	candidate := make([]File, 0, len(m.Entries))
	for _, entry := range m.Entries {
		b, e := readAt(g, entry.File, maxFile)
		if e != nil {
			return nil, e
		}
		out[entry.File] = b
		candidate = append(candidate, File{Name: entry.File, SQL: b, Parents: append([]string(nil), entry.Parents...), NonLinear: entry.NonLinear})
	}
	actual, e := build(candidate)
	if e != nil {
		return nil, e
	}
	if !bytes.Equal(canonical(m), canonical(actual)) {
		return nil, conflict("content_changed", m.Generation, "restore the byte-exact migration generation or perform an authorized update")
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
	return s.Manifest, nil
}

type journal struct {
	Version        int      `json:"version"`
	Kind           string   `json:"kind"`
	ExpectedDigest string   `json:"expected_digest"`
	Manifest       Manifest `json:"manifest"`
	Cleanup        []string `json:"cleanup"`
}
type legacyEntry struct {
	File      string   `json:"file"`
	Parents   []string `json:"parents,omitempty"`
	NonLinear bool     `json:"nonlinear,omitempty"`
}
type legacyManifest struct {
	Version string        `json:"version"`
	Entries []legacyEntry `json:"entries"`
	Digest  string        `json:"digest"`
}

func parseLegacyManifest(raw []byte) (legacyManifest, error) {
	if e := rejectDuplicates(raw); e != nil {
		return legacyManifest{}, e
	}
	var old legacyManifest
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&old); e != nil || old.Version != LegacyVersion || !bytes.Equal(raw, canonical(old)) {
		return old, ErrInvalid
	}
	c := old
	c.Digest = ""
	if digest("manifest-v0", canonical(c)) != old.Digest {
		return old, conflict("manifest_digest", ManifestFile, "restore the canonical legacy manifest")
	}
	seen := map[string]bool{}
	for _, x := range old.Entries {
		if filepath.Base(x.File) != x.File || !fileRE.MatchString(x.File) || seen[strings.ToLower(x.File)] {
			return old, ErrInvalid
		}
		seen[strings.ToLower(x.File)] = true
	}
	return old, nil
}

func parseJournal(raw []byte) (journal, error) {
	if e := rejectDuplicates(raw); e != nil {
		return journal{}, e
	}
	var j journal
	d := json.NewDecoder(bytes.NewReader(raw))
	d.DisallowUnknownFields()
	if e := d.Decode(&j); e != nil {
		return j, e
	}
	if j.Version != 2 || (j.Kind != "update" && j.Kind != "v0-migration") || !bytes.Equal(raw, canonical(j)) {
		return j, ErrInvalid
	}
	if j.ExpectedDigest != "" && !digestRE.MatchString(j.ExpectedDigest) {
		return j, ErrInvalid
	}
	if e := validateManifestStructure(j.Manifest); e != nil {
		return j, e
	}
	c := j.Manifest
	c.Digest = ""
	if digest("manifest", canonical(c)) != j.Manifest.Digest {
		return j, ErrInvalid
	}
	seen := map[string]bool{}
	for _, p := range j.Cleanup {
		if filepath.Base(p) != p || strings.Contains(p, "..") || seen[p] {
			return j, ErrInvalid
		}
		seen[p] = true
	}
	return j, nil
}

func writeAll(fd int, p []byte, ops Ops) error {
	for len(p) > 0 {
		n, e := ops.write(fd, p)
		if n > 0 {
			p = p[n:]
		}
		if e != nil {
			if errors.Is(e, unix.EINTR) {
				continue
			}
			return e
		}
		if n == 0 {
			return io.ErrShortWrite
		}
	}
	return nil
}
func writeAt(parent int, name string, p []byte, mode uint32, ops Ops) error {
	fd, e := ops.openat(parent, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, mode)
	if e != nil {
		return e
	}
	ok := false
	defer func() {
		if fd >= 0 {
			_ = ops.close(fd)
		}
		if !ok {
			_ = ops.unlinkat(parent, name, 0)
		}
	}()
	if e = writeAll(fd, p, ops); e != nil {
		return e
	}
	if e = ops.sync(fd); e != nil {
		return e
	}
	if e = ops.close(fd); e != nil {
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
func mkdirAt(parent int, name string, mode uint32, ops Ops) error {
	e := ops.mkdirat(parent, name, mode)
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
	root, e := openRootWithOps(dir, ops)
	if e != nil {
		return Manifest{}, e
	}
	defer ops.close(root)
	lock, e := openLockWithOps(root, true, ops)
	if e != nil {
		return Manifest{}, e
	}
	defer closeLockWithOps(lock, ops)
	return updateLocked(root, dir, req, ops, false, nil)
}

func updateLocked(root int, dir string, req UpdateRequest, ops Ops, allowLegacy bool, cleanup []string) (Manifest, error) {
	man, e := build(req.Files)
	if e != nil {
		return Manifest{}, e
	}
	if len(canonical(man)) > maxManifest {
		return Manifest{}, fmt.Errorf("%w: manifest size", ErrInvalid)
	}
	if jr, je := readAt(root, journalFile, maxManifest); je == nil {
		j, pe := parseJournal(jr)
		if pe != nil {
			return Manifest{}, conflict("dirty_journal", journalFile, "retain and inspect the exact journal")
		}
		kind := "update"
		if allowLegacy {
			kind = "v0-migration"
		}
		if j.Manifest.Digest != man.Digest || j.ExpectedDigest != req.ExpectedManifestDigest || j.Kind != kind {
			return Manifest{}, conflict("pending_transaction", journalFile, "resume with the exact authorized candidate and expected digest")
		}
		if e = recoverLockedWithOps(root, ops); e != nil {
			return Manifest{}, e
		}
		if _, e = verifyGenerationAt(root, man); e != nil {
			return Manifest{}, e
		}
		return man, nil
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
		if _, e = snapshotAt(root); e != nil {
			return Manifest{}, conflict("current_snapshot_invalid", dir, "restore the trusted current snapshot before updating")
		}
		if req.ExpectedManifestDigest == "" {
			return Manifest{}, conflict("compare_and_swap_required", dir, "reload the snapshot and provide its digest")
		}
		if current.Digest != req.ExpectedManifestDigest {
			return Manifest{}, conflict("compare_and_swap", dir, "reload the snapshot and retry")
		}
	} else if req.ExpectedManifestDigest != "" && !legacy {
		return Manifest{}, conflict("compare_and_swap", dir, "the directory has no published manifest")
	}
	if e = mkdirAt(root, genDir, 0700, ops); e != nil {
		return Manifest{}, e
	}
	base, e := ops.openat(root, genDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return Manifest{}, e
	}
	defer ops.close(base)
	if e = validateDirFD(base, genDir); e != nil {
		return Manifest{}, e
	}
	if e = ops.mkdirat(base, man.Generation, 0700); e != nil && e != unix.EEXIST {
		return Manifest{}, e
	}
	g, e := ops.openat(base, man.Generation, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if e != nil {
		return Manifest{}, e
	}
	defer ops.close(g)
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
	kind := "update"
	if allowLegacy {
		kind = "v0-migration"
	}
	j := journal{2, kind, req.ExpectedManifestDigest, man, append([]string(nil), cleanup...)}
	jr := canonical(j)
	tmp := ManifestFile + "." + man.Generation + ".new"
	if e = ensureFileAt(root, tmp, canonical(man), 0600, ops); e != nil {
		return Manifest{}, e
	}
	if e = writeAt(root, journalFile, jr, 0600, ops); e != nil {
		return Manifest{}, e
	}
	if e = ops.sync(root); e != nil {
		return Manifest{}, e
	}
	if e = ops.rename(root, tmp, root, ManifestFile); e != nil {
		return Manifest{}, e
	}
	if e = ops.sync(root); e != nil {
		return Manifest{}, e
	}
	for _, p := range cleanup {
		if e = ops.unlinkat(root, p, 0); e != nil && !errors.Is(e, unix.ENOENT) {
			return Manifest{}, e
		}
	}
	if e = ops.sync(root); e != nil {
		return Manifest{}, e
	}
	if e = ops.unlinkat(root, journalFile, 0); e != nil {
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
	return RecoverWithOps(dir, Ops{})
}
func RecoverWithOps(dir string, ops Ops) error {
	root, e := openRootWithOps(dir, ops)
	if e != nil {
		return e
	}
	defer ops.close(root)
	lock, e := openLockWithOps(root, true, ops)
	if e != nil {
		return e
	}
	defer closeLockWithOps(lock, ops)
	return recoverLockedWithOps(root, ops)
}

func recoverLockedWithOps(root int, ops Ops) error {
	raw, e := readAt(root, journalFile, maxFile)
	if errors.Is(e, unix.ENOENT) {
		return nil
	}
	if e != nil {
		return e
	}
	j, e := parseJournal(raw)
	if e != nil {
		return conflict("dirty_journal", journalFile, "inspect and retain the exact journal")
	}
	if _, e = verifyGenerationAt(root, j.Manifest); e != nil {
		return e
	}
	published := false
	if m, _, me := loadAt(root); me == nil {
		if m.Digest == j.Manifest.Digest {
			published = true
		} else if m.Digest != j.ExpectedDigest {
			return conflict("recovery_cas", ManifestFile, "current manifest no longer matches the journal precondition")
		} else if _, e = snapshotAt(root); e != nil {
			return conflict("current_snapshot_invalid", ManifestFile, "restore the journal's trusted precondition snapshot")
		}
	} else if j.Kind == "v0-migration" {
		oldRaw, re := readAt(root, ManifestFile, maxManifest)
		if re != nil {
			return re
		}
		old, pe := parseLegacyManifest(oldRaw)
		if pe != nil || old.Digest != j.ExpectedDigest {
			return conflict("recovery_cas", ManifestFile, "legacy manifest no longer matches the journal precondition")
		}
	} else if !(errors.Is(me, unix.ENOENT) && j.ExpectedDigest == "") {
		return conflict("recovery_cas", ManifestFile, "current manifest no longer matches the journal precondition")
	}
	if !published { // The durable journal is the exact authorization to finish its manifest-last rename.
		tmp := ManifestFile + "." + j.Manifest.Generation + ".new"
		tr, e := readAt(root, tmp, maxManifest)
		if e != nil || !bytes.Equal(tr, canonical(j.Manifest)) {
			return conflict("missing_staged_manifest", tmp, "restore the exact staged manifest from the journal candidate")
		}
		if e = ops.rename(root, tmp, root, ManifestFile); e != nil {
			return e
		}
		if e = ops.sync(root); e != nil {
			return e
		}
	}
	for _, p := range j.Cleanup {
		if e = ops.unlinkat(root, p, 0); e != nil && !errors.Is(e, unix.ENOENT) {
			return e
		}
	}
	if e = ops.sync(root); e != nil {
		return e
	}
	if e = ops.unlinkat(root, journalFile, 0); e != nil {
		return e
	}
	return ops.sync(root)
}

// MigrateManifest upgrades the explicit legacy v0 root-file format atomically.
// The v0 manifest uses {version,entries:[{file,parents,nonlinear}],digest}; its
// digest is sha256 over the canonical object with an empty digest field.
func MigrateManifest(dir, from string) (Manifest, error) {
	return MigrateManifestWithOps(dir, from, Ops{})
}
func MigrateManifestWithOps(dir, from string, ops Ops) (Manifest, error) {
	if from == ManifestVersion {
		return Verify(dir)
	}
	if from != LegacyVersion {
		return Manifest{}, conflict("unsupported_manifest_migration", from, "only explicit supported migrations may change manifest versions")
	}
	root, e := openRootWithOps(dir, ops)
	if e != nil {
		return Manifest{}, e
	}
	defer ops.close(root)
	lock, e := openLockWithOps(root, true, ops)
	if e != nil {
		return Manifest{}, e
	}
	defer closeLockWithOps(lock, ops)
	if _, je := readAt(root, journalFile, maxManifest); je == nil {
		if e = recoverLockedWithOps(root, ops); e != nil {
			return Manifest{}, e
		}
		s, e := snapshotAt(root)
		if e != nil {
			return Manifest{}, e
		}
		return s.Manifest, nil
	} else if !errors.Is(je, unix.ENOENT) {
		return Manifest{}, je
	}
	if _, _, me := loadAt(root); me == nil {
		s, e := snapshotAt(root)
		if e != nil {
			return Manifest{}, e
		}
		return s.Manifest, nil
	}
	raw, e := readAt(root, ManifestFile, maxManifest)
	if e != nil {
		return Manifest{}, e
	}
	old, e := parseLegacyManifest(raw)
	if e != nil {
		return Manifest{}, e
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
	if e = ensureFileAt(root, ManifestFile+".v0", raw, 0600, ops); e != nil {
		return Manifest{}, e
	}
	cleanup := make([]string, len(old.Entries))
	for i := range old.Entries {
		cleanup[i] = old.Entries[i].File
	}
	man, e := updateLocked(root, dir, UpdateRequest{Files: files, ExpectedManifestDigest: old.Digest}, ops, true, cleanup)
	if e != nil {
		return Manifest{}, e
	}
	return man, nil
}
