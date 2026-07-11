package artifact

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
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
	b, e := a.MarshalCanonical()
	if e != nil {
		return e
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if e = os.MkdirAll(r.Dir, 0700); e != nil {
		return e
	}
	if e = os.Chmod(r.Dir, 0700); e != nil {
		return e
	}
	path := filepath.Join(r.Dir, a.Digest+".json")
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
	b, e := os.ReadFile(filepath.Join(r.Dir, d+".json"))
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
	return a, nil
}
