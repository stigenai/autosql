package artifact

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"golang.org/x/sys/unix"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var ErrCollision = errors.New("artifact digest collision")
var ErrNotFound = errors.New("artifact not found")

type Registry interface {
	Put(context.Context, VerifiedArtifact) error
	Get(context.Context, string) (Artifact, error)
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
