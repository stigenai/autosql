package operator

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sync"
)

// FileStore persists reconciliation records atomically. Production operators
// should place the file on a durable mounted volume; it is intentionally a
// small adapter so the reconciliation core remains independent of Kubernetes.
type FileStore struct {
	mu      sync.Mutex
	path    string
	records map[string]Record
}

func NewFileStore(path string) (*FileStore, error) {
	if path == "" {
		return nil, errors.New("operator state path is required")
	}
	s := &FileStore{path: path, records: map[string]Record{}}
	raw, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(raw, &s.records); err != nil {
		return nil, err
	}
	return s, nil
}

func (s *FileStore) Load(key string) (Record, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	r, ok := s.records[key]
	return r, ok
}

func (s *FileStore) Save(key string, record Record) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.records[key] = record
	raw, err := json.Marshal(s.records)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(s.path), 0700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(s.path), ".operator-state-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)
	if err = tmp.Chmod(0600); err == nil {
		_, err = tmp.Write(raw)
	}
	if closeErr := tmp.Close(); err == nil {
		err = closeErr
	}
	if err != nil {
		return err
	}
	return os.Rename(tmpName, s.path)
}
