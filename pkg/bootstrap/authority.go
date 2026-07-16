// Package bootstrap defines the non-secret authority contract shared by CLI
// and operator-driven fresh database provisioning.
package bootstrap

import (
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
)

// Authentication identifies how an execution identity obtains a short-lived
// database session. Credential values are deliberately not representable.
type Authentication string

const (
	CurrentSession  Authentication = "current_session"
	SecretReference Authentication = "secret_reference"
	AWSIRSA         Authentication = "aws_irsa"
	OIDC            Authentication = "oidc"
)

// Capability is a server-side privilege required by a bootstrap phase.
type Capability string

const (
	CreateDatabase    Capability = "create_database"
	ManageRoles       Capability = "manage_roles"
	ManageExtensions  Capability = "manage_extensions"
	ManageSchema      Capability = "manage_schema_objects"
	ManageGrants      Capability = "manage_grants"
	TransferOwnership Capability = "transfer_ownership"
)

// Responsibility is one ordered phase of a complete database bootstrap.
type Responsibility string

const (
	DatabaseCreation Responsibility = "database_creation"
	RoleCreation     Responsibility = "role_creation"
	ExtensionSetup   Responsibility = "extension_setup"
	SchemaObjects    Responsibility = "schema_objects"
	GrantSetup       Responsibility = "grant_setup"
	OwnershipHandoff Responsibility = "ownership_handoff"
)

// CredentialRef names an external credential provider without containing a
// password, token, connection URL, or resolved provider response.
type CredentialRef struct {
	Provider string `json:"provider" yaml:"provider"`
	Name     string `json:"name" yaml:"name"`
	Key      string `json:"key,omitempty" yaml:"key,omitempty"`
}

// Identity is a stable, auditable execution principal. Subject is a database
// role or workload identity, not a credential value.
type Identity struct {
	Name           string         `json:"name" yaml:"name"`
	Subject        string         `json:"subject" yaml:"subject"`
	Authentication Authentication `json:"authentication" yaml:"authentication"`
	Credential     *CredentialRef `json:"credential,omitempty" yaml:"credential,omitempty"`
	Capabilities   []Capability   `json:"capabilities" yaml:"capabilities"`
}

// Assignment binds a required phase to exactly one identity.
type Assignment struct {
	Responsibility Responsibility `json:"responsibility" yaml:"responsibility"`
	Identity       string         `json:"identity" yaml:"identity"`
}

// Contract is safe to persist in documents, plans, fingerprints, and status.
// Resolved credentials belong to runtime-only connection providers.
type Contract struct {
	Identities  []Identity   `json:"identities" yaml:"identities"`
	Assignments []Assignment `json:"assignments" yaml:"assignments"`
}

// Requirement records why a phase is needed and the capability its identity
// must possess. It is deterministic input to preflight.
type Requirement struct {
	Responsibility Responsibility `json:"responsibility"`
	Capability     Capability     `json:"capability"`
	Reason         string         `json:"reason"`
}

// Binding is the resolved, non-secret execution assignment returned by
// preflight. It can safely be logged and included in an artifact fingerprint.
type Binding struct {
	Responsibility Responsibility `json:"responsibility"`
	Capability     Capability     `json:"capability"`
	Identity       string         `json:"identity"`
	Subject        string         `json:"subject"`
	Authentication Authentication `json:"authentication"`
	Reason         string         `json:"reason"`
}

// Validate resolves all required phases before any SQL is rendered or run.
func (c Contract) Validate(required []Requirement) ([]Binding, error) {
	var problems []error
	identities := make(map[string]Identity, len(c.Identities))
	for _, identity := range c.Identities {
		if err := validateIdentity(identity); err != nil {
			problems = append(problems, err)
			continue
		}
		if _, exists := identities[identity.Name]; exists {
			problems = append(problems, fmt.Errorf("duplicate bootstrap identity %q", identity.Name))
			continue
		}
		identities[identity.Name] = identity
	}
	assignments := make(map[Responsibility]string, len(c.Assignments))
	for _, assignment := range c.Assignments {
		if assignment.Responsibility == "" || strings.TrimSpace(assignment.Identity) == "" {
			problems = append(problems, errors.New("bootstrap assignments require responsibility and identity"))
			continue
		}
		if _, exists := assignments[assignment.Responsibility]; exists {
			problems = append(problems, fmt.Errorf("duplicate bootstrap assignment for %s", assignment.Responsibility))
			continue
		}
		assignments[assignment.Responsibility] = assignment.Identity
		if _, exists := identities[assignment.Identity]; !exists {
			problems = append(problems, fmt.Errorf("bootstrap assignment for %s names unknown identity %q", assignment.Responsibility, assignment.Identity))
		}
	}

	requirements := append([]Requirement(nil), required...)
	sort.Slice(requirements, func(i, j int) bool { return requirements[i].Responsibility < requirements[j].Responsibility })
	bindings := make([]Binding, 0, len(requirements))
	for _, requirement := range requirements {
		name := assignments[requirement.Responsibility]
		if name == "" {
			problems = append(problems, fmt.Errorf("bootstrap responsibility %s has no assigned identity", requirement.Responsibility))
			continue
		}
		identity, ok := identities[name]
		if !ok {
			problems = append(problems, fmt.Errorf("bootstrap responsibility %s names unknown identity %q", requirement.Responsibility, name))
			continue
		}
		if !hasCapability(identity.Capabilities, requirement.Capability) {
			problems = append(problems, fmt.Errorf("bootstrap identity %q lacks %s for %s", name, requirement.Capability, requirement.Responsibility))
			continue
		}
		bindings = append(bindings, Binding{Responsibility: requirement.Responsibility, Capability: requirement.Capability, Identity: identity.Name, Subject: identity.Subject, Authentication: identity.Authentication, Reason: requirement.Reason})
	}
	return bindings, errors.Join(problems...)
}

func validateIdentity(identity Identity) error {
	if strings.TrimSpace(identity.Name) == "" || strings.TrimSpace(identity.Subject) == "" {
		return errors.New("bootstrap identities require name and subject")
	}
	switch identity.Authentication {
	case CurrentSession, AWSIRSA, OIDC:
		if identity.Credential != nil {
			return fmt.Errorf("bootstrap identity %q uses %s and must not declare a credential reference", identity.Name, identity.Authentication)
		}
	case SecretReference:
		if identity.Credential == nil || strings.TrimSpace(identity.Credential.Provider) == "" || strings.TrimSpace(identity.Credential.Name) == "" {
			return fmt.Errorf("bootstrap identity %q requires an external credential reference", identity.Name)
		}
	default:
		return fmt.Errorf("bootstrap identity %q has unsupported authentication %q", identity.Name, identity.Authentication)
	}
	seen := map[Capability]bool{}
	for _, capability := range identity.Capabilities {
		if capability == "" || seen[capability] {
			return fmt.Errorf("bootstrap identity %q has empty or duplicate capability", identity.Name)
		}
		seen[capability] = true
	}
	return nil
}

func hasCapability(capabilities []Capability, required Capability) bool {
	for _, capability := range capabilities {
		if capability == required {
			return true
		}
	}
	return false
}

// MarshalCanonical returns a deterministic representation after sorting set-
// like fields. It contains no runtime credential material by construction.
func (c Contract) MarshalCanonical() ([]byte, error) {
	copy := c
	copy.Identities = append([]Identity(nil), c.Identities...)
	for i := range copy.Identities {
		copy.Identities[i].Capabilities = append([]Capability(nil), copy.Identities[i].Capabilities...)
		sort.Slice(copy.Identities[i].Capabilities, func(a, b int) bool { return copy.Identities[i].Capabilities[a] < copy.Identities[i].Capabilities[b] })
	}
	sort.Slice(copy.Identities, func(i, j int) bool { return copy.Identities[i].Name < copy.Identities[j].Name })
	copy.Assignments = append([]Assignment(nil), c.Assignments...)
	sort.Slice(copy.Assignments, func(i, j int) bool { return copy.Assignments[i].Responsibility < copy.Assignments[j].Responsibility })
	return json.Marshal(copy)
}
