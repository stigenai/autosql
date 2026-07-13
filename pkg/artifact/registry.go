package artifact

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"syscall"
	"time"
)

var ErrCollision = errors.New("artifact digest collision")
var ErrNotFound = errors.New("artifact not found")

type Registry interface {
	Put(context.Context, VerifiedArtifact) error
	Get(context.Context, string) (Artifact, error)
}

// Action identifies an authorization boundary in the managed registry. Read,
// push, and promotion are intentionally separate capabilities.
type Action string

const (
	ActionRead      Action = "artifact.read"
	ActionPush      Action = "artifact.push"
	ActionPromotion Action = "artifact.promote"
)

// Authorizer is called before every managed-registry operation.
type Authorizer interface {
	Authorize(context.Context, Action, string, string) error
}

type allowAllAuthorizer struct{}

func (allowAllAuthorizer) Authorize(context.Context, Action, string, string) error { return nil }

// IntegrityManifest binds exact canonical bytes to an artifact digest and
// declares the attestations required by the receiving registry.
type IntegrityManifest struct {
	Version              string   `json:"version"`
	ArtifactDigest       string   `json:"artifact_digest"`
	CanonicalBytesDigest string   `json:"canonical_bytes_digest"`
	RequiredAttestations []string `json:"required_attestations,omitempty"`
}

const integrityManifestVersion = "autosql.artifact.integrity/v1"

func NewIntegrityManifest(v VerifiedArtifact, required []string) (IntegrityManifest, error) {
	a, err := v.forRegistry()
	if err != nil {
		return IntegrityManifest{}, err
	}
	b, err := a.MarshalCanonical()
	if err != nil {
		return IntegrityManifest{}, err
	}
	h := sha256.Sum256(b)
	return IntegrityManifest{Version: integrityManifestVersion, ArtifactDigest: a.Digest, CanonicalBytesDigest: "sha256:" + hex.EncodeToString(h[:]), RequiredAttestations: append([]string(nil), required...)}, nil
}

func (m IntegrityManifest) verify(v VerifiedArtifact, required []string) error {
	if m.Version != integrityManifestVersion || !digestPattern.MatchString(m.ArtifactDigest) || !digestPattern.MatchString(m.CanonicalBytesDigest) {
		return fail("integrity_manifest", ErrInvalid)
	}
	a, err := v.forRegistry()
	if err != nil {
		return err
	}
	if a.Digest != m.ArtifactDigest {
		return fail("integrity_digest", ErrInvalid)
	}
	b, err := a.MarshalCanonical()
	if err != nil {
		return fail("integrity_bytes", ErrInvalid)
	}
	h := sha256.Sum256(b)
	if "sha256:"+hex.EncodeToString(h[:]) != m.CanonicalBytesDigest {
		return fail("integrity_bytes", ErrInvalid)
	}
	seen := map[string]bool{}
	for _, s := range m.RequiredAttestations {
		if s == "" || seen[s] {
			return fail("integrity_attestations", ErrInvalid)
		}
		seen[s] = true
	}
	for _, s := range required {
		if !seen[s] {
			return fail("required_attestation", ErrInvalid)
		}
	}
	attested := map[string]bool{}
	for _, x := range a.ValidationAttestations {
		attested[x.Stage] = true
	}
	if a.EditProvenance != nil {
		for _, x := range a.EditProvenance.Attestations {
			attested[x.Stage] = true
		}
	}
	for _, s := range m.RequiredAttestations {
		if !attested[s] {
			return fail("required_attestation", ErrInvalid)
		}
	}
	for _, s := range required {
		if !attested[s] {
			return fail("required_attestation", ErrInvalid)
		}
	}
	return nil
}

type PushRequest struct {
	Artifact VerifiedArtifact
	Manifest IntegrityManifest
	Actor    string
}

// TagRecord is an append-only record of a mutable tag pointer.
type TagRecord struct {
	Sequence       uint64    `json:"sequence"`
	Tag            string    `json:"tag"`
	PreviousDigest string    `json:"previous_digest,omitempty"`
	Digest         string    `json:"digest"`
	Actor          string    `json:"actor"`
	At             time.Time `json:"at"`
}

var tagPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._/-]{0,127}$`)

// ManagedRegistry adds validated pushes, mutable tags, tag history, and
// authorization boundaries to an immutable Registry implementation.
type ManagedRegistry struct {
	Store                Registry
	Authorizer           Authorizer
	RequiredAttestations []string
	mu                   sync.RWMutex
	tags                 map[string]TagRecord
	history              map[string][]TagRecord
}

func NewManagedRegistry(store Registry, auth Authorizer, required []string) *ManagedRegistry {
	if auth == nil {
		auth = allowAllAuthorizer{}
	}
	return &ManagedRegistry{Store: store, Authorizer: auth, RequiredAttestations: append([]string(nil), required...), tags: map[string]TagRecord{}, history: map[string][]TagRecord{}}
}

func (r *ManagedRegistry) Push(ctx context.Context, req PushRequest) error {
	if r == nil || r.Store == nil {
		return fail("registry", ErrInvalid)
	}
	if err := r.Authorizer.Authorize(ctx, ActionPush, req.Actor, req.Manifest.ArtifactDigest); err != nil {
		return err
	}
	if err := req.Manifest.verify(req.Artifact, r.RequiredAttestations); err != nil {
		return err
	}
	return r.Store.Put(ctx, req.Artifact)
}

// Read enforces read authorization independently of push and promotion.
func (r *ManagedRegistry) Read(ctx context.Context, digest, actor string) (Artifact, error) {
	if r == nil || r.Store == nil {
		return Artifact{}, fail("registry", ErrInvalid)
	}
	if err := r.Authorizer.Authorize(ctx, ActionRead, actor, digest); err != nil {
		return Artifact{}, err
	}
	return r.Store.Get(ctx, digest)
}

func (r *ManagedRegistry) Promote(ctx context.Context, tag, digest, actor string) error {
	if r == nil || r.Store == nil || !tagPattern.MatchString(tag) || !digestPattern.MatchString(digest) {
		return fail("tag", ErrInvalid)
	}
	if err := r.Authorizer.Authorize(ctx, ActionPromotion, actor, tag+"@"+digest); err != nil {
		return err
	}
	if _, err := r.Store.Get(ctx, digest); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	prev := r.tags[tag].Digest
	record := TagRecord{Sequence: uint64(len(r.history[tag]) + 1), Tag: tag, PreviousDigest: prev, Digest: digest, Actor: actor, At: time.Now().UTC()}
	r.tags[tag] = record
	r.history[tag] = append(r.history[tag], record)
	return nil
}

func (r *ManagedRegistry) ResolveTag(ctx context.Context, tag, actor string) (Artifact, TagRecord, error) {
	if r == nil || !tagPattern.MatchString(tag) {
		return Artifact{}, TagRecord{}, fail("tag", ErrInvalid)
	}
	if err := r.Authorizer.Authorize(ctx, ActionRead, actor, "tag:"+tag); err != nil {
		return Artifact{}, TagRecord{}, err
	}
	r.mu.RLock()
	record, ok := r.tags[tag]
	r.mu.RUnlock()
	if !ok {
		return Artifact{}, TagRecord{}, ErrNotFound
	}
	a, err := r.Store.Get(ctx, record.Digest)
	return a, record, err
}

func (r *ManagedRegistry) TagHistory(ctx context.Context, tag, actor string) ([]TagRecord, error) {
	if r == nil || !tagPattern.MatchString(tag) {
		return nil, fail("tag", ErrInvalid)
	}
	if err := r.Authorizer.Authorize(ctx, ActionRead, actor, "tag:"+tag); err != nil {
		return nil, err
	}
	r.mu.RLock()
	defer r.mu.RUnlock()
	if _, ok := r.tags[tag]; !ok {
		return nil, ErrNotFound
	}
	return append([]TagRecord(nil), r.history[tag]...), nil
}

func (m IntegrityManifest) MarshalJSON() ([]byte, error) {
	type alias IntegrityManifest
	return json.Marshal(alias(m))
}

type MemoryRegistry struct {
	mu sync.RWMutex
	m  map[string][]byte
}

func NewMemoryRegistry() *MemoryRegistry { return &MemoryRegistry{m: map[string][]byte{}} }
func (r *MemoryRegistry) Put(_ context.Context, v VerifiedArtifact) error {
	a, e := v.forRegistry()
	if e != nil {
		return e
	}
	b, e := a.MarshalCanonical()
	if e != nil {
		return e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if old, ok := r.m[a.Digest]; ok && string(old) != string(b) {
		return ErrCollision
	}
	r.m[a.Digest] = append([]byte(nil), b...)
	return nil
}
func (r *MemoryRegistry) Get(_ context.Context, d string) (Artifact, error) {
	if !digestPattern.MatchString(d) {
		return Artifact{}, fail("digest", ErrInvalid)
	}
	r.mu.RLock()
	b, ok := r.m[d]
	r.mu.RUnlock()
	if !ok {
		return Artifact{}, ErrNotFound
	}
	a, e := Parse(b)
	if e != nil {
		return Artifact{}, e
	}
	if e = a.validateStored(); e != nil {
		return Artifact{}, e
	}
	return a, nil
}

type LocalRegistry struct {
	Dir string
	mu  sync.Mutex
}

func (r *LocalRegistry) Put(_ context.Context, v VerifiedArtifact) error {
	a, e := v.forRegistry()
	if e != nil {
		return e
	}
	b, e := a.MarshalCanonical()
	if e != nil {
		return e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	_, statErr := os.Lstat(r.Dir)
	created := os.IsNotExist(statErr)
	if created {
		if e = os.MkdirAll(r.Dir, 0700); e != nil {
			return fail("registry_io", ErrInvalid)
		}
		if e = os.Chmod(r.Dir, 0700); e != nil {
			return fail("registry_io", ErrInvalid)
		}
		parent, pe := os.Open(filepath.Dir(r.Dir))
		if pe != nil {
			return fail("registry_io", ErrInvalid)
		}
		e = parent.Sync()
		closeErr := parent.Close()
		if e != nil || closeErr != nil {
			return fail("registry_io", ErrInvalid)
		}
	}
	if e = trustedDir(r.Dir); e != nil {
		return e
	}
	dfd, e := openTrustedDir(r.Dir)
	if e != nil {
		return e
	}
	defer unix.Close(dfd)
	lockfd, e := unix.Openat(dfd, ".lock", unix.O_CREAT|unix.O_RDWR|unix.O_NOFOLLOW, 0600)
	if e != nil {
		return fail("registry_io", ErrInvalid)
	}
	defer unix.Close(lockfd)
	if e = trustedFD(lockfd, 0600); e != nil {
		return e
	}
	if e = unix.Flock(lockfd, unix.LOCK_EX); e != nil {
		return fail("registry_io", ErrInvalid)
	}
	defer unix.Flock(lockfd, unix.LOCK_UN)
	target := a.Digest + ".json"
	if fd, oe := unix.Openat(dfd, target, unix.O_RDONLY|unix.O_NOFOLLOW, 0); oe == nil {
		defer unix.Close(fd)
		if e = trustedFD(fd, 0600); e != nil {
			return e
		}
		old, e := readFD(fd)
		if e != nil {
			return e
		}
		if string(old) == string(b) {
			return nil
		}
		return ErrCollision
	}
	random := make([]byte, 16)
	if _, e = rand.Read(random); e != nil {
		return fail("registry_io", ErrInvalid)
	}
	tmp := ".artifact-" + hex.EncodeToString(random)
	tfd, e := unix.Openat(dfd, tmp, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_NOFOLLOW, 0600)
	if e != nil {
		return fail("registry_io", ErrInvalid)
	}
	cleanup := true
	defer func() {
		unix.Close(tfd)
		if cleanup {
			unix.Unlinkat(dfd, tmp, 0)
		}
	}()
	if e = trustedFD(tfd, 0600); e != nil {
		return e
	}
	if _, e = unix.Write(tfd, b); e != nil {
		return fail("registry_io", ErrInvalid)
	}
	if e = unix.Fsync(tfd); e != nil {
		return fail("registry_io", ErrInvalid)
	}
	if e = unix.Linkat(dfd, tmp, dfd, target, 0); e != nil {
		return fail("registry_collision", ErrCollision)
	}
	if e = unix.Unlinkat(dfd, tmp, 0); e != nil {
		return fail("registry_io", ErrInvalid)
	}
	cleanup = false
	if e = unix.Fsync(dfd); e != nil {
		return fail("registry_io", ErrInvalid)
	}
	return nil
}
func (r *LocalRegistry) Get(_ context.Context, d string) (Artifact, error) {
	if !digestPattern.MatchString(d) {
		return Artifact{}, fail("digest", ErrInvalid)
	}
	dfd, e := openTrustedDir(r.Dir)
	if e != nil {
		return Artifact{}, e
	}
	defer unix.Close(dfd)
	fd, e := unix.Openat(dfd, d+".json", unix.O_RDONLY|unix.O_NOFOLLOW, 0)
	if e != nil {
		return Artifact{}, fail("registry_not_found", ErrNotFound)
	}
	defer unix.Close(fd)
	if e = trustedFD(fd, 0600); e != nil {
		return Artifact{}, e
	}
	b, e := readFD(fd)
	if e != nil {
		return Artifact{}, e
	}
	a, e := Parse(b)
	if e != nil {
		return Artifact{}, e
	}
	if a.Digest != d {
		return Artifact{}, fail("registry_collision", ErrCollision)
	}
	if e = a.validateStored(); e != nil {
		return Artifact{}, e
	}
	return a, nil
}
func trustedDir(path string) error {
	info, e := os.Lstat(path)
	if e != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 || info.Mode().Perm() != 0700 {
		return fail("registry", ErrInvalid)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) {
		return fail("registry", ErrInvalid)
	}
	return nil
}
func openTrustedDir(path string) (int, error) {
	fd, e := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_NOFOLLOW, 0)
	if e != nil {
		return -1, fail("registry_io", ErrInvalid)
	}
	if e = trustedFD(fd, 0700); e != nil {
		unix.Close(fd)
		return -1, e
	}
	return fd, nil
}
func trustedFD(fd int, mode uint32) error {
	var s unix.Stat_t
	if e := unix.Fstat(fd, &s); e != nil {
		return fail("registry_trust", ErrInvalid)
	}
	kind := s.Mode & unix.S_IFMT
	if s.Uid != uint32(os.Geteuid()) || (kind == unix.S_IFREG && s.Nlink != 1) || (kind != unix.S_IFREG && kind != unix.S_IFDIR) || uint32(s.Mode)&0777 != mode {
		return fail("registry_trust", ErrInvalid)
	}
	return nil
}
func readFD(fd int) ([]byte, error) {
	const limit = 4 << 20
	result := make([]byte, 0, 4096)
	buffer := make([]byte, 32<<10)
	for {
		n, err := unix.Read(fd, buffer)
		if n > 0 {
			if len(result)+n > limit {
				return nil, fail("registry_io", ErrInvalid)
			}
			result = append(result, buffer[:n]...)
		}
		if err == unix.EINTR {
			continue
		}
		if err != nil {
			return nil, fail("registry_io", ErrInvalid)
		}
		if n == 0 {
			return result, nil
		}
	}
}
