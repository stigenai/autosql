package bootstrap

import (
	"strings"
	"testing"
)

func TestContractAggregatesAuthorityAndNeverRepresentsCredentials(t *testing.T) {
	contract := Contract{
		Identities: []Identity{
			{Name: "cluster", Subject: "arn:aws:iam::123456789012:role/autosql", Authentication: AWSIRSA, Capabilities: []Capability{CreateDatabase}},
			{Name: "schema", Subject: "autosql_owner", Authentication: SecretReference, Credential: &CredentialRef{Provider: "kubernetes", Name: "autosql-db", Key: "url"}, Capabilities: []Capability{ManageSchema}},
		},
		Assignments: []Assignment{
			{Responsibility: DatabaseCreation, Identity: "cluster"},
			{Responsibility: SchemaObjects, Identity: "schema"},
		},
	}
	required := []Requirement{
		{Responsibility: DatabaseCreation, Capability: CreateDatabase, Reason: "create target"},
		{Responsibility: SchemaObjects, Capability: ManageSchema, Reason: "create objects"},
		{Responsibility: GrantSetup, Capability: ManageGrants, Reason: "grant access"},
		{Responsibility: OwnershipHandoff, Capability: TransferOwnership, Reason: "handoff"},
	}
	bindings, err := contract.Validate(required)
	if err == nil || !strings.Contains(err.Error(), "grant_setup has no assigned identity") || !strings.Contains(err.Error(), "ownership_handoff has no assigned identity") {
		t.Fatalf("expected aggregate missing authority, got bindings=%+v err=%v", bindings, err)
	}
	if len(bindings) != 2 || bindings[0].Responsibility != DatabaseCreation || bindings[1].Responsibility != SchemaObjects {
		t.Fatalf("bindings=%+v", bindings)
	}
	encoded, err := contract.MarshalCanonical()
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range []string{"password", "token", "postgres://"} {
		if strings.Contains(strings.ToLower(string(encoded)), forbidden) {
			t.Fatalf("contract represented secret material: %s", encoded)
		}
	}
}

func TestContractSupportsOIDCWithoutCredentialMaterial(t *testing.T) {
	contract := Contract{
		Identities:  []Identity{{Name: "operator", Subject: "system:serviceaccount:autosql:operator", Authentication: OIDC, Capabilities: []Capability{ManageSchema, TransferOwnership}}},
		Assignments: []Assignment{{Responsibility: SchemaObjects, Identity: "operator"}, {Responsibility: OwnershipHandoff, Identity: "operator"}},
	}
	bindings, err := contract.Validate([]Requirement{{Responsibility: SchemaObjects, Capability: ManageSchema}, {Responsibility: OwnershipHandoff, Capability: TransferOwnership}})
	if err != nil || len(bindings) != 2 {
		t.Fatalf("bindings=%+v err=%v", bindings, err)
	}
}
