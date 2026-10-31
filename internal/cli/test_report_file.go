package cli

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"syscall"
)

var errTestReportWrite = errors.New("JUnit report could not be published")

func writeTestReportAtomic(destination string, data []byte) error {
	directory, name := filepath.Split(destination)
	if directory == "" {
		directory = "."
	}
	if err := os.MkdirAll(directory, 0700); err != nil {
		return errTestReportWrite
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return errTestReportWrite
	}
	defer root.Close()
	if err := checkTestReportDestination(root, name); err != nil {
		return err
	}
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return errTestReportWrite
	}
	temporary := ".autosql-junit-" + hex.EncodeToString(nonce[:])
	file, err := root.OpenFile(temporary, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return errTestReportWrite
	}
	defer root.Remove(temporary)
	if err := flushTestReport(file, data); err != nil {
		return err
	}
	return publishTestReport(root, temporary, name)
}

func checkTestReportDestination(root *os.Root, name string) error {
	info, err := root.Lstat(name)
	if errors.Is(err, os.ErrNotExist) && name != "" {
		return nil
	}
	if err != nil || !info.Mode().IsRegular() {
		return errTestReportWrite
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || stat.Nlink != 1 {
		return errTestReportWrite
	}
	return nil
}

func flushTestReport(file *os.File, data []byte) error {
	_, writeErr := file.Write(data)
	syncErr := file.Sync()
	closeErr := file.Close()
	if errors.Join(writeErr, syncErr, closeErr) != nil {
		return errTestReportWrite
	}
	return nil
}

func publishTestReport(root *os.Root, temporary, destination string) error {
	if err := checkTestReportDestination(root, destination); err != nil {
		return err
	}
	if err := root.Rename(temporary, destination); err != nil {
		return errTestReportWrite
	}
	directory, err := root.Open(".")
	if err != nil {
		return errTestReportWrite
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return errTestReportWrite
	}
	return nil
}
