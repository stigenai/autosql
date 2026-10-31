package config

import (
	"bytes"
	"context"
	"crypto/sha256"
	"os"
	"path/filepath"
)

// CaptureConfiguration binds the original parsed file bytes to the same
// confined capture as test/schema inputs, including the parse-to-open gap.
func (c *Config) CaptureConfiguration(files *InputFiles) error {
	identity, err := c.Identity()
	if err != nil || files == nil || files.identity != identity {
		return c.problem("/inputs", "input capture does not match the project")
	}
	raw, err := files.ReadFile(filepath.Base(c.path))
	if err != nil || sha256.Sum256(raw) != c.sourceDigest {
		return c.problem("/inputs", "project configuration changed after parsing")
	}
	return nil
}

// VerifyCurrent compares captured bytes with fresh, confined reads. It does not
// update the snapshot and is safe for concurrent test cases. A moved/replaced
// project root is refused so verification cannot silently follow a stale tree.
func (f *InputFiles) VerifyCurrent(ctx context.Context) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.root == nil {
		return f.failure("input reader is closed")
	}
	pinned, err := f.root.Stat(".")
	if err != nil {
		return f.failure("project root is unavailable")
	}
	current, err := os.Stat(f.path)
	if err != nil || !os.SameFile(pinned, current) {
		return f.failure("project root changed after capture")
	}
	for _, name := range sortedKeys(f.files) {
		if err := ctx.Err(); err != nil {
			return err
		}
		raw, err := f.readRegular(name)
		if err != nil || !bytes.Equal(raw, f.files[name].data) {
			return f.failure("project input changed after capture")
		}
	}
	return ctx.Err()
}
