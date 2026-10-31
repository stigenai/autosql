package dbtest

import (
	"os"
	"path/filepath"
	"time"
)

// LeaseInfo is a credential-free snapshot for lifecycle output and recovery.
// It is not deletion authority: cleanup always checks the live creation record.
type LeaseInfo struct {
	Path          string           `json:"lease_path"`
	RunID         string           `json:"run_id"`
	CaseID        string           `json:"case_id"`
	State         string           `json:"state"`
	PostgresMajor int              `json:"postgres_major"`
	Deadline      time.Time        `json:"deadline"`
	Resources     []ResourceInfo   `json:"resources"`
	CleanupErrors []CleanupFailure `json:"cleanup_errors,omitempty"`
}

type ResourceInfo struct {
	Kind  string `json:"kind"`
	Name  string `json:"name"`
	State string `json:"state"`
}

type CleanupFailure struct {
	Kind     string `json:"kind"`
	Category string `json:"category"`
}

func (f *leaseFile) info() LeaseInfo {
	r := f.snapshot()
	info := LeaseInfo{Path: f.path, RunID: r.RunID, CaseID: r.CaseID, State: r.State, PostgresMajor: r.Factory.PostgresMajor, Deadline: r.Deadline, Resources: []ResourceInfo{}}
	for _, resource := range r.Resources {
		info.Resources = append(info.Resources, ResourceInfo{resource.Kind, resource.Name, resource.State})
	}
	for _, failure := range r.CleanupErrors {
		info.CleanupErrors = append(info.CleanupErrors, CleanupFailure{failure.Kind, failure.Category})
	}
	return info
}

func (d *DisposableTarget) Info() LeaseInfo {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lease.info()
}

// InspectLease requires an unlocked, intact, owner-only lease. It does not
// resolve any secrets, connect to a database, or claim live resource existence.
func InspectLease(path string) (LeaseInfo, error) {
	f, err := openLeaseFile(path)
	if err != nil {
		return LeaseInfo{}, err
	}
	defer f.close()
	return f.info(), nil
}

// PrepareLeaseDirectory creates a private runtime directory when absent and
// refuses existing paths with unsafe ownership or permissions. It never chmods
// an existing directory or follows a final-component symlink.
func PrepareLeaseDirectory(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", ErrLease
	}
	if err := os.MkdirAll(absolute, 0700); err != nil {
		return "", ErrLease
	}
	fd, err := openLeaseDirectory(absolute)
	if err != nil {
		return "", err
	}
	// Use os.File solely to close this validated descriptor.
	if err := os.NewFile(uintptr(fd), absolute).Close(); err != nil {
		return "", ErrLease
	}
	return absolute, nil
}
