package dbtest

import (
	"errors"
	"io"
	"os"

	"golang.org/x/sys/unix"
)

const credentialFilename = "connection.url"

func (f *leaseFile) writeCredential(value string) error {
	if !validLeaseCredential(value, f.record) {
		return ErrLease
	}
	next := f.snapshot()
	next.CredentialRef = credentialFilename
	if err := f.save(next); err != nil {
		return err
	}
	fd, err := unix.Openat(f.root, credentialFilename, unix.O_CREAT|unix.O_EXCL|unix.O_WRONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0600)
	if err != nil {
		return ErrLease
	}
	if err := writeSyncedLeaseFile(fd, []byte(value)); err != nil {
		_ = unix.Unlinkat(f.root, credentialFilename, 0)
		_ = unix.Fsync(f.root)
		return err
	}
	if err := unix.Fsync(f.root); err != nil {
		return ErrLease
	}
	return nil
}

func validLeaseCredential(value string, record leaseRecord) bool {
	u, endpoint, err := parseFactoryURL(value, true)
	if err != nil || endpoint.database != record.resourceName() || u.User.Username() != record.resourceName() {
		return false
	}
	password, present := u.User.Password()
	return present && leaseContainerID.MatchString(password) // generated 256-bit hex secret
}

func (f *leaseFile) removeCredential() error {
	fd, err := unix.Openat(f.root, credentialFilename, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return ErrLease
	}
	file := os.NewFile(uintptr(fd), credentialFilename)
	defer file.Close()
	if err := validateLeaseFD(fd, false); err != nil {
		return err
	}
	raw, err := io.ReadAll(io.LimitReader(file, 64*1024+1))
	if err != nil || len(raw) > 64*1024 || !validLeaseCredential(string(raw), f.record) {
		return ErrLease
	}
	if err := unix.Unlinkat(f.root, credentialFilename, 0); err != nil {
		return ErrLease
	}
	if err := unix.Fsync(f.root); err != nil {
		return ErrLease
	}
	return nil
}
