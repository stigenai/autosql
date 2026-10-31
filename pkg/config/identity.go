package config

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"strings"
)

// ProjectIdentity is available after validation, without reading input files or
// resolving secrets. It is not an effective-input digest or an approval proof.
type ProjectIdentity struct {
	ConfigDigest          string `json:"config_digest"`
	ExecutionConfigDigest string `json:"execution_config_digest"`
}

type configurationSnapshot struct {
	Version     int                 `json:"version"`
	Environment string              `json:"environment"`
	Values      Environment         `json:"values"`
	Factory     *DevelopmentFactory `json:"development_factory,omitempty"`
	Overrides   map[string]string   `json:"overrides,omitempty"`
}

// Identity binds normalized settings and override origins. File source ranges
// and absolute checkout locations are diagnostic metadata, so they are omitted.
// Configured timeouts remain settings; measured run durations are never inputs.
func (c *Config) Identity() (ProjectIdentity, error) {
	if err := c.Validate(); err != nil {
		return ProjectIdentity{}, err
	}
	if c.Version != CurrentVersion {
		return ProjectIdentity{}, c.problem("/version", "project input identities require version 2")
	}
	effective := c.Effective()
	snapshot := configurationSnapshot{Version: c.Version, Environment: effective.Name, Values: effective.Values, Factory: effective.DevelopmentFactory, Overrides: cloneMap(c.origins)}
	configuration, err := digestConfiguration("autosql.project-configuration/v1", snapshot)
	if err != nil {
		return ProjectIdentity{}, c.problem("", "cannot encode project configuration identity")
	}
	// Moving a report does not change which SQL was approved. Keep a distinct
	// report/configuration identity so reports still describe their settings.
	snapshot.Values.Reporting = ReportingDefaults{}
	for field := range snapshot.Overrides {
		if field == "/reporting" || strings.HasPrefix(field, "/reporting/") {
			delete(snapshot.Overrides, field)
		}
	}
	execution, err := digestConfiguration("autosql.project-execution-configuration/v1", snapshot)
	if err != nil {
		return ProjectIdentity{}, c.problem("", "cannot encode project execution identity")
	}
	return ProjectIdentity{ConfigDigest: configuration, ExecutionConfigDigest: execution}, nil
}

func digestConfiguration(domain string, snapshot configurationSnapshot) (string, error) {
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return "", err
	}
	digest := sha256.New()
	_, _ = digest.Write([]byte(domain + "\x00"))
	_, _ = digest.Write(raw)
	return "sha256:" + hex.EncodeToString(digest.Sum(nil)), nil
}
