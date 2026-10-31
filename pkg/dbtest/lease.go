package dbtest

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"path/filepath"
	"regexp"
	"time"

	"autosql/pkg/secret"
)

const leaseVersion = "autosql.development-lease/v1"

var (
	ErrLease            = errors.New("development lease invalid or unavailable")
	ErrOwnership        = errors.New("development resource ownership could not be verified")
	leaseID             = regexp.MustCompile(`^[a-f0-9]{32}$`)
	leaseServerIdentity = regexp.MustCompile(`^[^/@?#\s]+(?:/[0-9]{1,3}\]?:[0-9]+)?/[^/@?#\s]+#[0-9]+$`)
	leaseImage          = regexp.MustCompile(`^[a-zA-Z0-9][a-zA-Z0-9._:/-]*@sha256:[a-f0-9]{64}$`)
	leaseImageID        = regexp.MustCompile(`^sha256:[a-f0-9]{64}$`)
	leaseDaemonID       = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9:-]{7,127}$`)
	leaseContainerID    = regexp.MustCompile(`^[a-f0-9]{64}$`)
)

// A lease is evidence for a subsequent live ownership check, never permission
// to drop a resource by name. It contains references and identities, not keys,
// passwords, connection URLs with credentials, or arbitrary backend errors.
type leaseRecord struct {
	Version       string          `json:"version"`
	RunID         string          `json:"run_id"`
	CaseID        string          `json:"case_id"`
	Factory       leaseFactory    `json:"factory"`
	State         string          `json:"state"`
	Resources     []leaseResource `json:"resources"`
	CredentialRef string          `json:"credential_ref,omitempty"`
	CreatedAt     time.Time       `json:"created_at"`
	Deadline      time.Time       `json:"deadline"`
	CleanupErrors []leaseFailure  `json:"cleanup_errors,omitempty"`
}

type leaseFactory struct {
	Mode           string           `json:"mode"`
	Name           string           `json:"name"`
	Admin          secret.Reference `json:"admin,omitempty"`
	AdminIdentity  string           `json:"admin_identity,omitempty"`
	DaemonID       string           `json:"daemon_id,omitempty"`
	Endpoint       string           `json:"endpoint,omitempty"`
	Image          string           `json:"image,omitempty"`
	PostgresMajor  int              `json:"postgres_major"`
	CleanupTimeout string           `json:"cleanup_timeout,omitempty"`
}

type leaseResource struct {
	Kind    string `json:"kind"`
	Name    string `json:"name"`
	Marker  string `json:"marker"`
	State   string `json:"state"`
	OID     uint32 `json:"oid,omitempty"`
	ID      string `json:"id,omitempty"`
	ImageID string `json:"image_id,omitempty"`
}

type leaseFailure struct {
	Kind     string `json:"kind"`
	Category string `json:"category"`
}

func randomLeaseID() (string, error) {
	var raw [16]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", ErrLease
	}
	return hex.EncodeToString(raw[:]), nil
}

func (l leaseRecord) resourceName() string { return "autosql_test_" + l.CaseID }
func (l leaseRecord) marker() string       { return leaseVersion + ":" + l.RunID + ":" + l.CaseID }

func (l leaseRecord) validate() error {
	if !l.validIdentity() {
		return ErrLease
	}
	if _, known := leaseTransitions[l.State]; !known {
		return ErrLease
	}
	if l.CredentialRef != "" && l.CredentialRef != credentialFilename {
		return ErrLease
	}
	if err := l.Factory.validate(); err != nil {
		return err
	}
	if err := l.validateResources(); err != nil {
		return err
	}
	return l.validateState()
}

func (l leaseRecord) validIdentity() bool {
	return l.Version == leaseVersion && leaseID.MatchString(l.RunID) && leaseID.MatchString(l.CaseID) && !l.CreatedAt.IsZero() && l.Deadline.After(l.CreatedAt)
}

func (f leaseFactory) validate() error {
	if f.Name == "" || len(f.Name) > 128 || f.PostgresMajor < 14 || f.PostgresMajor > 18 {
		return ErrLease
	}
	if _, err := f.cleanupTimeout(); err != nil {
		return err
	}
	return f.validateMode()
}

func (f leaseFactory) cleanupTimeout() (time.Duration, error) {
	if f.CleanupTimeout == "" {
		return 10 * time.Second, nil
	}
	value, err := time.ParseDuration(f.CleanupTimeout)
	if err != nil || value <= 0 {
		return 0, ErrLease
	}
	return value, nil
}

func (f leaseFactory) validateMode() error {
	switch f.Mode {
	case "existing_admin":
		return f.validateAdminMode()
	case "container":
		return f.validateContainerMode()
	default:
		return ErrLease
	}
}
func (f leaseFactory) validateAdminMode() error {
	if f.Admin.Validate() != nil || !leaseServerIdentity.MatchString(f.AdminIdentity) || f.Endpoint != "" || f.Image != "" || f.DaemonID != "" {
		return ErrLease
	}
	return nil
}
func (f leaseFactory) validateContainerMode() error {
	if !leaseDaemonEndpoint(f.Endpoint) || !leaseImage.MatchString(f.Image) || f.Admin != "" || f.AdminIdentity != "" || !leaseDaemonID.MatchString(f.DaemonID) {
		return ErrLease
	}
	return nil
}

func leaseDaemonEndpoint(value string) bool {
	u, err := url.Parse(value)
	if err != nil || u.User != nil || u.RawQuery != "" || u.Fragment != "" {
		return false
	}
	switch u.Scheme {
	case "unix":
		return u.Host == "" && filepath.IsAbs(u.Path)
	case "tcp":
		return u.Hostname() != "" && u.Port() != "" && u.Path == ""
	default:
		return false
	}
}

func (l leaseRecord) validateResources() error {
	if len(l.Resources) > 3 || len(l.CleanupErrors) > 4 {
		return ErrLease
	}
	seen := map[string]bool{}
	for _, resource := range l.Resources {
		if seen[resource.Kind] || resource.Name != l.resourceName() || resource.Marker != l.marker() {
			return ErrLease
		}
		if err := resource.validate(); err != nil {
			return err
		}
		seen[resource.Kind] = true
	}
	if err := l.validateResourceModes(seen); err != nil {
		return err
	}
	return l.validateFailures(seen)
}

func (l leaseRecord) validateFailures(seen map[string]bool) error {
	if l.State == "cleanup_failed" && len(l.CleanupErrors) == 0 {
		return ErrLease
	}
	for _, failure := range l.CleanupErrors {
		if !seen[failure.Kind] && failure.Kind != "credentials" || !cleanupCategories[failure.Category] {
			return ErrLease
		}
	}
	return nil
}

var cleanupCategories = map[string]bool{"ownership": true, "connection": true, "timeout": true, "remove": true, "persist": true}

func (r leaseResource) validate() error {
	if r.State != "planned" && r.State != "acquired" && r.State != "released" {
		return ErrLease
	}
	if r.ID != "" && !leaseContainerID.MatchString(r.ID) {
		return ErrLease
	}
	return r.validateIdentity()
}

func (r leaseResource) validateIdentity() error {
	switch r.Kind {
	case "database", "role":
		if r.ID != "" || r.ImageID != "" || r.State == "acquired" && r.OID == 0 {
			return ErrLease
		}
	case "container":
		if !r.validContainerIdentity() {
			return ErrLease
		}
	default:
		return ErrLease
	}
	return nil
}

var leaseTransitions = map[string]map[string]bool{
	"allocating":     {"allocating": true, "ready": true, "cleaning": true},
	"ready":          {"running": true, "cleaning": true},
	"running":        {"cleaning": true},
	"cleaning":       {"cleaning": true, "cleaned": true, "cleanup_failed": true},
	"cleanup_failed": {"cleaning": true},
	"cleaned":        {},
}

func (l leaseRecord) validateState() error {
	switch l.State {
	case "ready", "running":
		if l.CredentialRef == "" || !l.hasExecutionResources() || len(l.CleanupErrors) != 0 {
			return ErrLease
		}
		if !l.allResourcesInState("acquired") {
			return ErrLease
		}
	case "cleaned":
		if l.CredentialRef != "" || len(l.CleanupErrors) != 0 {
			return ErrLease
		}
		if !l.allResourcesInState("released") {
			return ErrLease
		}
	}
	return nil
}

func (l leaseRecord) hasExecutionResources() bool {
	kinds := map[string]bool{}
	for _, resource := range l.Resources {
		kinds[resource.Kind] = true
	}
	return kinds["role"] && kinds["database"]
}

func (l leaseRecord) allResourcesInState(state string) bool {
	for _, resource := range l.Resources {
		if resource.State != state {
			return false
		}
	}
	return true
}

func leaseResourcesAdvance(current, next leaseRecord) bool {
	if len(next.Resources) < len(current.Resources) {
		return false
	}
	if len(next.Resources) > len(current.Resources) && (current.State != "allocating" || next.State != "allocating") {
		return false
	}
	for i, old := range current.Resources {
		if !leaseResourceAdvances(old, next.Resources[i]) {
			return false
		}
	}
	return true
}

func leaseResourceAdvances(old, next leaseResource) bool {
	if old.Kind != next.Kind || old.Name != next.Name || old.Marker != next.Marker || old.ImageID != next.ImageID {
		return false
	}
	switch old.State {
	case "planned":
		return true
	case "acquired":
		return next.State != "planned" && old.OID == next.OID && old.ID == next.ID
	case "released":
		return old == next
	default:
		return false
	}
}

func (r leaseResource) validContainerIdentity() bool {
	return r.OID == 0 && leaseImageID.MatchString(r.ImageID) && (r.State != "acquired" || r.ID != "")
}

func (l leaseRecord) validateResourceModes(kinds map[string]bool) error {
	if l.Factory.Mode == "existing_admin" && kinds["container"] {
		return ErrLease
	}
	if l.Factory.Mode == "container" && (kinds["role"] || kinds["database"]) && !kinds["container"] {
		return ErrLease
	}
	return nil
}
