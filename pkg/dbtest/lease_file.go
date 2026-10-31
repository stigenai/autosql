package dbtest

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"slices"
	"time"

	"golang.org/x/sys/unix"
)

const leaseFilename = "lease.json"

type leaseEnvelope struct {
	Record leaseRecord `json:"record"`
	Digest string      `json:"digest"`
}

// leaseFile pins an owner-only directory and holds an exclusive process lock.
// Creation, update and cleanup use descriptor-relative operations. Callers must
// close it; failed persistence must stop provisioning before any further SQL.
type leaseFile struct {
	root, lock int
	path       string
	record     leaseRecord
}

func createLease(rootPath string, factory leaseFactory, deadline time.Time) (*leaseFile, error) {
	if err := factory.validate(); err != nil {
		return nil, err
	}
	run, err := randomLeaseID()
	if err != nil {
		return nil, err
	}
	caseID, err := randomLeaseID()
	if err != nil {
		return nil, err
	}
	record := leaseRecord{Version: leaseVersion, RunID: run, CaseID: caseID, Factory: factory, State: "allocating", CreatedAt: time.Now().UTC(), Deadline: deadline.UTC()}
	if err := record.validate(); err != nil {
		return nil, err
	}
	return createLeaseFile(rootPath, record)
}

func createLeaseFile(rootPath string, record leaseRecord) (*leaseFile, error) {
	rootPath, err := filepath.Abs(rootPath)
	if err != nil {
		return nil, ErrLease
	}
	parent, err := openLeaseDirectory(rootPath)
	if err != nil {
		return nil, err
	}
	defer unix.Close(parent)
	name := record.RunID + "-" + record.CaseID
	if err := unix.Mkdirat(parent, name, 0700); err != nil {
		return nil, ErrLease
	}
	if err := unix.Fsync(parent); err != nil {
		return nil, ErrLease
	}
	root, err := unix.Openat(parent, name, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, ErrLease
	}
	file, err := lockLeaseDirectory(root, filepath.Join(rootPath, name, leaseFilename))
	if err != nil {
		return nil, err
	}
	if err := file.write(record); err != nil {
		file.close()
		return nil, err
	}
	file.record = record
	return file, nil
}

func openLeaseFile(path string) (*leaseFile, error) {
	path, err := filepath.Abs(path)
	if err != nil {
		return nil, ErrLease
	}
	if filepath.Base(path) != leaseFilename {
		return nil, ErrLease
	}
	root, err := openLeaseDirectory(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	file, err := lockLeaseDirectory(root, path)
	if err != nil {
		return nil, err
	}
	record, err := file.read()
	if err != nil {
		file.close()
		return nil, err
	}
	if filepath.Base(filepath.Dir(path)) != record.RunID+"-"+record.CaseID {
		file.close()
		return nil, ErrLease
	}
	file.record = record
	return file, nil
}

func (f *leaseFile) close() {
	if f.lock >= 0 {
		_ = unix.Flock(f.lock, unix.LOCK_UN)
		_ = unix.Close(f.lock)
		f.lock = -1
	}
	if f.root >= 0 {
		_ = unix.Close(f.root)
		f.root = -1
	}
}

func (f *leaseFile) save(next leaseRecord) error {
	current, err := f.read()
	if err != nil || !reflect.DeepEqual(current, f.record) || !leaseTransitions[current.State][next.State] {
		return ErrLease
	}
	if !sameLeaseAllocation(current, next) || next.validate() != nil || !leaseResourcesAdvance(current, next) {
		return ErrLease
	}
	if err := f.write(next); err != nil {
		return err
	}
	f.record = cloneLeaseRecord(next)
	return nil
}

func (f *leaseFile) snapshot() leaseRecord { return cloneLeaseRecord(f.record) }

func cloneLeaseRecord(record leaseRecord) leaseRecord {
	record.Resources = slices.Clone(record.Resources)
	record.CleanupErrors = slices.Clone(record.CleanupErrors)
	return record
}

func sameLeaseAllocation(a, b leaseRecord) bool {
	return a.Version == b.Version && a.RunID == b.RunID && a.CaseID == b.CaseID && a.Factory == b.Factory && a.CreatedAt.Equal(b.CreatedAt) && a.Deadline.Equal(b.Deadline)
}

func (f *leaseFile) read() (leaseRecord, error) {
	fd, err := unix.Openat(f.root, leaseFilename, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return leaseRecord{}, ErrLease
	}
	file := os.NewFile(uintptr(fd), leaseFilename)
	defer file.Close()
	if err := validateLeaseFD(fd, false); err != nil {
		return leaseRecord{}, err
	}
	raw, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil || len(raw) > 64*1024 {
		return leaseRecord{}, ErrLease
	}
	var envelope leaseEnvelope
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&envelope); err != nil {
		return leaseRecord{}, ErrLease
	}
	canonical, _ := encodeLease(envelope.Record)
	if !bytes.Equal(raw, canonical) || envelope.Record.validate() != nil {
		return leaseRecord{}, ErrLease
	}
	return envelope.Record, nil
}

func encodeLease(record leaseRecord) ([]byte, error) {
	raw, err := json.Marshal(record)
	if err != nil {
		return nil, ErrLease
	}
	digest := sha256.Sum256(append([]byte(leaseVersion+"\x00"), raw...))
	return json.Marshal(leaseEnvelope{Record: record, Digest: "sha256:" + hex.EncodeToString(digest[:])})
}

func (f *leaseFile) write(record leaseRecord) error {
	raw, err := encodeLease(record)
	if err != nil || len(raw) > 64*1024 {
		return ErrLease
	}
	id, err := randomLeaseID()
	if err != nil {
		return err
	}
	name := ".lease-" + id
	fd, err := unix.Openat(f.root, name, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrLease
	}
	defer unix.Unlinkat(f.root, name, 0)
	if err := writeSyncedLeaseFile(fd, raw); err != nil {
		return err
	}
	if err := unix.Renameat(f.root, name, f.root, leaseFilename); err != nil {
		return ErrLease
	}
	if err := unix.Fsync(f.root); err != nil {
		return ErrLease
	}
	return nil
}

func writeSyncedLeaseFile(fd int, raw []byte) error {
	file := os.NewFile(uintptr(fd), leaseFilename)
	n, err := file.Write(raw)
	if err == nil && n == len(raw) {
		err = file.Sync()
	}
	closeErr := file.Close()
	if err != nil || n != len(raw) || closeErr != nil {
		return ErrLease
	}
	return nil
}

func openLeaseDirectory(path string) (int, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return -1, ErrLease
	}
	if err := validateLeaseFD(fd, true); err != nil {
		unix.Close(fd)
		return -1, err
	}
	return fd, nil
}

func validateLeaseFD(fd int, directory bool) error {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil || stat.Mode&0077 != 0 || int(stat.Uid) != os.Geteuid() {
		return ErrLease
	}
	if directory {
		if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
			return ErrLease
		}
	} else if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		return ErrLease
	}
	return nil
}

func lockLeaseDirectory(root int, path string) (*leaseFile, error) {
	fd, err := unix.Openat(root, ".lock", unix.O_CREAT|unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		unix.Close(root)
		return nil, ErrLease
	}
	file := &leaseFile{root: root, lock: fd, path: path}
	if validateLeaseFD(root, true) != nil || validateLeaseFD(fd, false) != nil || unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB) != nil {
		file.close()
		return nil, ErrLease
	}
	return file, nil
}
