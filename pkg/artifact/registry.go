package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

var ErrCollision = errors.New("artifact digest collision")
var ErrNotFound = errors.New("artifact not found")

type Registry interface {
	Put(context.Context, Artifact) error
	Get(context.Context, string) (Artifact, error)
}
type MemoryRegistry struct {
	mu sync.RWMutex
	m  map[string][]byte
}

func NewMemoryRegistry() *MemoryRegistry { return &MemoryRegistry{m: map[string][]byte{}} }
func (r *MemoryRegistry) Put(_ context.Context, a Artifact) error {
	if e := a.validateStored(); e != nil {
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
	return Parse(b)
}

type LocalRegistry struct {
	Dir string
	mu  sync.Mutex
}

func (r *LocalRegistry) Put(_ context.Context, a Artifact) error {
	if e := a.validateStored(); e != nil {
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
	if e = os.MkdirAll(r.Dir, 0700); e != nil {
		return e
	}
	if e = os.Chmod(r.Dir, 0700); e != nil {
		return e
	}
	if e = trustedDir(r.Dir); e != nil {
		return e
	}
	if created {
		if parent, pe := os.Open(filepath.Dir(r.Dir)); pe == nil {
			e = parent.Sync()
			parent.Close()
			if e != nil {
				return fail("registry", ErrInvalid)
			}
		}
	}
	lockPath := filepath.Join(r.Dir, ".lock")
	if _, le := os.Lstat(lockPath); le == nil {
		if e = trustedFile(lockPath); e != nil {
			return e
		}
	}
	lock, e := os.OpenFile(lockPath, os.O_CREATE|os.O_RDWR, 0600)
	if e != nil {
		return fail("registry", ErrInvalid)
	}
	defer lock.Close()
	if info, le := lock.Stat(); le != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		return fail("registry", ErrInvalid)
	}
	if e = syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); e != nil {
		return fail("registry", ErrInvalid)
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN)
	path := filepath.Join(r.Dir, a.Digest+".json")
	if _, se := os.Lstat(path); se == nil {
		if e = trustedFile(path); e != nil {
			return e
		}
	}
	if old, e := os.ReadFile(path); e == nil {
		if string(old) == string(b) {
			return nil
		}
		return ErrCollision
	}
	tmp, e := os.CreateTemp(r.Dir, ".artifact-*")
	if e != nil {
		return e
	}
	name := tmp.Name()
	defer os.Remove(name)
	if e = tmp.Chmod(0600); e == nil {
		_, e = tmp.Write(b)
	}
	if e == nil {
		e = tmp.Sync()
	}
	if closeErr := tmp.Close(); e == nil {
		e = closeErr
	}
	if e != nil {
		return e
	}
	if e = os.Rename(name, path); e != nil {
		return e
	}
	dir, e := os.Open(r.Dir)
	if e == nil {
		e = dir.Sync()
		dir.Close()
	}
	return e
}
func (r *LocalRegistry) Get(_ context.Context, d string) (Artifact, error) {
	if !digestPattern.MatchString(d) {
		return Artifact{}, fail("digest", ErrInvalid)
	}
	if e := trustedDir(r.Dir); e != nil {
		return Artifact{}, e
	}
	path := filepath.Join(r.Dir, d+".json")
	info, e := os.Lstat(path)
	if os.IsNotExist(e) {
		return Artifact{}, ErrNotFound
	}
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || info.Sys().(*syscall.Stat_t).Nlink != 1 {
		return Artifact{}, fail("registry", ErrInvalid)
	}
	b, e := os.ReadFile(path)
	if os.IsNotExist(e) {
		return Artifact{}, ErrNotFound
	}
	if e != nil {
		return Artifact{}, e
	}
	a, e := Parse(b)
	if e != nil {
		return Artifact{}, e
	}
	if a.Digest != d {
		return Artifact{}, fmt.Errorf("%w: filename digest", ErrCollision)
	}
	if e = a.validateStored(); e != nil {
		return Artifact{}, e
	}
	return a, nil
}
func trustedFile(path string) error {
	info, e := os.Lstat(path)
	if e != nil || !info.Mode().IsRegular() || info.Mode().Perm() != 0600 {
		return fail("registry", ErrInvalid)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Uid != uint32(os.Geteuid()) || stat.Nlink != 1 {
		return fail("registry", ErrInvalid)
	}
	return nil
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
