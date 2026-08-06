package postgres

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"autosql/pkg/schema"
	"autosql/pkg/secret"
)

// BootstrapAuthorizationReference is the declarative, non-secret half of a
// bootstrap authorization. It names runtime locations for a signed manifest
// and its public verification key; private keys and resolved values are never
// part of the schema graph.
type BootstrapAuthorizationReference struct {
	Manifest  string `json:"manifest"`
	PublicKey string `json:"public_key"`
	Issuer    string `json:"issuer"`
	Signer    string `json:"signer"`
	Purpose   string `json:"purpose"`
}

func (r BootstrapAuthorizationReference) Validate() error {
	if strings.TrimSpace(r.Issuer) == "" || strings.TrimSpace(r.Signer) == "" || strings.TrimSpace(r.Purpose) == "" || r.Issuer != strings.TrimSpace(r.Issuer) || r.Signer != strings.TrimSpace(r.Signer) || r.Purpose != strings.TrimSpace(r.Purpose) {
		return errors.New("bootstrap authorization issuer, signer, and purpose are required")
	}
	for name, value := range map[string]string{"manifest": r.Manifest, "public_key": r.PublicKey} {
		if value != strings.TrimSpace(value) {
			return fmt.Errorf("bootstrap authorization %s must be an env:// or file:// runtime reference", name)
		}
		ref := secret.Reference(value)
		if err := ref.Validate(); err != nil {
			return fmt.Errorf("bootstrap authorization %s must be an env:// or file:// runtime reference", name)
		}
	}
	return nil
}

// BootstrapAuthorizationReferenceFromResource reads the optional
// bootstrap_authorization object from an HCL database resource. The object is
// intentionally outside DatabaseTarget and is ignored by database DDL.
func BootstrapAuthorizationReferenceFromResource(resource schema.Resource) (*BootstrapAuthorizationReference, error) {
	if resource.Kind != schema.KindDatabase {
		return nil, errors.New("bootstrap authorization requires a database resource")
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(resource.Spec, &raw); err != nil {
		return nil, fmt.Errorf("decode database bootstrap authorization: %w", err)
	}
	value, ok := raw["bootstrap_authorization"]
	if !ok {
		return nil, nil
	}
	var ref BootstrapAuthorizationReference
	decoder := json.NewDecoder(strings.NewReader(string(value)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&ref); err != nil {
		return nil, fmt.Errorf("decode bootstrap_authorization: %w", err)
	}
	if err := ref.Validate(); err != nil {
		return nil, err
	}
	return &ref, nil
}
