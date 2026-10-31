package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"os"
	"path/filepath"
	"sync"
	"syscall"
)

const maxInputFileBytes = 32 << 20
const maxInputTotalBytes = 128 << 20
const maxInputFiles = 4096

// InputFile identifies bytes actually consumed by a local input loader. It
// contains no absolute checkout path, file content, timestamp, or credential.
type InputFile struct {
	Path   string `json:"path"`
	Digest string `json:"digest"`
	Size   int    `json:"size"`
}

type capturedInput struct {
	record InputFile
	data   []byte
}

// InputFiles is an explicitly opened, read-only project filesystem. ReadFile
// and ReadDir can be passed to source.HCLLoader so imports use the same bounded
// filesystem as root sources. Repeated reads return the captured bytes, which
// prevents a loader from interpreting different contents for the same path.
// Close must be called by the owner. No secret resolver or network is involved.
type InputFiles struct {
	mu       sync.Mutex
	ctx      context.Context
	root     *os.Root
	path     string
	project  string
	identity ProjectIdentity
	files    map[string]capturedInput
	total    int
}

func (c *Config) OpenInputFiles(ctx context.Context) (*InputFiles, error) {
	identity, err := c.Identity()
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	path, err := filepath.Abs(filepath.Dir(c.path))
	if err != nil {
		return nil, c.problem("", "project directory is unavailable")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return nil, c.problem("", "project directory is unavailable")
	}
	return &InputFiles{ctx: ctx, root: root, path: path, project: c.path, identity: identity, files: map[string]capturedInput{}}, nil
}

func (f *InputFiles) RootPath() string { return f.path }

func (f *InputFiles) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.root == nil {
		return nil
	}
	err := f.root.Close()
	f.root = nil
	return err
}

func (f *InputFiles) failure(message string) error {
	return &LocatedError{Code: "invalid_config", Field: "/inputs", Range: SourceRange{Filename: f.project, StartLine: 1, StartColumn: 1, EndLine: 1, EndColumn: 1}, Message: message}
}

func (f *InputFiles) relative(name string) (string, error) {
	if filepath.IsAbs(name) {
		var err error
		name, err = filepath.Rel(f.path, name)
		if err != nil {
			return "", f.failure("input path must remain inside the project")
		}
	}
	if !localProjectPath(name) {
		return "", f.failure("input path must remain inside the project")
	}
	return normalizedPath(name), nil
}

func (f *InputFiles) open(name string) (*os.File, error) {
	if err := f.ctx.Err(); err != nil {
		return nil, err
	}
	if f.root == nil {
		return nil, f.failure("input reader is closed")
	}
	// Nonblocking open lets the regular-file check refuse a FIFO immediately.
	// Root confines symlinks during the open itself, not a preceding path check.
	file, err := f.root.OpenFile(name, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return nil, f.failure("local input cannot be opened within the project")
	}
	return file, nil
}

func (f *InputFiles) ReadFile(name string) ([]byte, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, err := f.relative(name)
	if err != nil {
		return nil, err
	}
	if err := f.ctx.Err(); err != nil {
		return nil, err
	}
	if f.root == nil {
		return nil, f.failure("input reader is closed")
	}
	if cached, ok := f.files[name]; ok {
		return bytes.Clone(cached.data), nil
	}
	if len(f.files) >= maxInputFiles {
		return nil, f.failure("project exceeds 4096 input files")
	}
	raw, err := f.readRegular(name)
	if err != nil {
		return nil, err
	}
	if len(raw) > maxInputTotalBytes-f.total {
		return nil, f.failure("project inputs exceed 128 MiB")
	}
	digest := sha256.Sum256(raw)
	f.files[name] = capturedInput{record: InputFile{Path: name, Digest: "sha256:" + hex.EncodeToString(digest[:]), Size: len(raw)}, data: raw}
	f.total += len(raw)
	return bytes.Clone(raw), nil
}

func (f *InputFiles) readRegular(name string) ([]byte, error) {
	file, err := f.open(name)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return nil, f.failure("input must be an available regular file")
	}
	if info.Size() > maxInputFileBytes {
		return nil, f.failure("input file exceeds 32 MiB")
	}
	raw, err := io.ReadAll(io.LimitReader(file, maxInputFileBytes+1))
	if err != nil {
		return nil, f.failure("input file cannot be read")
	}
	if len(raw) > maxInputFileBytes {
		return nil, f.failure("input file exceeds 32 MiB")
	}
	if err := f.ctx.Err(); err != nil {
		return nil, err
	}
	return raw, nil
}

func (f *InputFiles) ReadDir(name string) ([]os.DirEntry, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, err := f.relative(name)
	if err != nil {
		return nil, err
	}
	directory, err := f.open(name)
	if err != nil {
		return nil, err
	}
	defer directory.Close()
	entries, err := directory.ReadDir(maxInputFiles + 1)
	if err != nil && err != io.EOF {
		return nil, f.failure("input directory cannot be read")
	}
	if len(entries) > maxInputFiles {
		return nil, f.failure("input directory exceeds 4096 entries")
	}
	return entries, nil
}

// OpenDirectory returns a read-only directory handle confined at open time.
// The caller closes it. This supports migration snapshot inspection without
// reopening an unchecked absolute pathname or creating a migration lock.
func (f *InputFiles) OpenDirectory(name string) (*os.File, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	name, err := f.relative(name)
	if err != nil {
		return nil, err
	}
	directory, err := f.open(name)
	if err != nil {
		return nil, err
	}
	info, err := directory.Stat()
	if err != nil || !info.IsDir() {
		directory.Close()
		return nil, f.failure("input must be an available directory")
	}
	return directory, nil
}

// Records returns stable, project-relative content identities for files read so
// far. Source order is bound separately by the configuration; import and test
// declaration ordering is bound by the containing file bytes.
func (f *InputFiles) Records() []InputFile {
	f.mu.Lock()
	defer f.mu.Unlock()
	result := make([]InputFile, 0, len(f.files))
	for _, name := range sortedKeys(f.files) {
		result = append(result, f.files[name].record)
	}
	return result
}
