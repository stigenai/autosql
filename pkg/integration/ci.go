// Package integration contains portable delivery contracts used by CI systems.
// The contracts deliberately keep migration execution in the autosql CLI: a
// native action or a generic container is only a thin, unprivileged wrapper.
package integration

import (
	"errors"
	"fmt"
	"regexp"
	"strings"
)

var digestRE = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)

var (
	ErrUnpinnedImage = errors.New("container image must be pinned by digest")
	ErrCacheMismatch = errors.New("cache artifact digest does not match requested digest")
)

// Image describes a reproducible, verifiable execution image.
type Image struct {
	Name         string `json:"name" yaml:"name"`
	Digest       string `json:"digest" yaml:"digest"`
	Version      string `json:"version" yaml:"version"`
	Signature    string `json:"signature" yaml:"signature"`
	SBOM         string `json:"sbom" yaml:"sbom"`
	Scanned      bool   `json:"scanned" yaml:"scanned"`
	Reproducible bool   `json:"reproducible" yaml:"reproducible"`
}

func (i Image) Validate() error {
	if strings.TrimSpace(i.Name) == "" || !digestRE.MatchString(i.Digest) || strings.TrimSpace(i.Version) == "" {
		return ErrUnpinnedImage
	}
	if strings.TrimSpace(i.Signature) == "" || strings.TrimSpace(i.SBOM) == "" || !i.Scanned || !i.Reproducible {
		return fmt.Errorf("image %s must include signature, SBOM, scan, and reproducibility evidence", i.Name)
	}
	return nil
}

// CacheRecord binds a cache entry to the exact image/artifact digest.
type CacheRecord struct{ Key, Digest string }

func VerifyCache(record CacheRecord, expectedDigest string) error {
	if record.Key == "" || !digestRE.MatchString(record.Digest) || record.Digest != expectedDigest {
		return ErrCacheMismatch
	}
	return nil
}

type Trust string

const (
	UntrustedPR          Trust = "untrusted-pr"
	PrivilegedDeployment Trust = "privileged-deployment"
)

type Stage struct {
	Name        string   `json:"name" yaml:"name"`
	Trust       Trust    `json:"trust" yaml:"trust"`
	Credentials []string `json:"credentials,omitempty" yaml:"credentials,omitempty"`
}

func (s Stage) Validate() error {
	if s.Name == "" || (s.Trust != UntrustedPR && s.Trust != PrivilegedDeployment) {
		return errors.New("stage must identify a valid trust boundary")
	}
	if s.Trust == UntrustedPR && len(s.Credentials) > 0 {
		return errors.New("untrusted PR stage cannot receive credentials")
	}
	return nil
}

// Command is the same interface exposed by reusable actions and generic shell containers.
type Command struct {
	Name  string   `json:"name" yaml:"name"`
	Args  []string `json:"args" yaml:"args"`
	Image Image    `json:"image" yaml:"image"`
}

func (c Command) Validate() error {
	if c.Name == "" {
		return errors.New("command name is required")
	}
	return c.Image.Validate()
}
