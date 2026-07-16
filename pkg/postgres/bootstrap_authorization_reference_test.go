package postgres

import (
	"context"
	"strings"
	"testing"

	"autosql/pkg/schema"
	"autosql/pkg/source"
)

func TestBootstrapAuthorizationReferenceFromHCLDatabaseResource(t *testing.T) {
	resource := schema.Resource{Kind: schema.KindDatabase, Spec: []byte(`{"bootstrap_authorization":{"manifest":"file:///run/autosql/authorization.json","public_key":"env://AUTOSQL_AUTH_PUBLIC","issuer":"security","signer":"dba","purpose":"bootstrap-authorization"}}`)}
	ref, err := BootstrapAuthorizationReferenceFromResource(resource)
	if err != nil || ref == nil || ref.Signer != "dba" {
		t.Fatalf("reference=%+v err=%v", ref, err)
	}
	encoded := string(resource.Spec)
	for _, forbidden := range []string{"private_key", "password", "token"} {
		if strings.Contains(encoded, forbidden) {
			t.Fatalf("credential field leaked into reference: %s", forbidden)
		}
	}
}

func TestHCLBootstrapAuthorizationRoundTripsAsRuntimeReferences(t *testing.T) {
	raw := []byte(`database "cell" {
  mode = "managed"
  endpoint = { host = "db.internal", port = 5432, tls_mode = "verify-full" }
  maintenance_database = "postgres"
  owner = "postgres"
  connection_limit = -1
  allow_connections = true
  bootstrap_authorization = {
    manifest = "file:///run/autosql/bootstrap-authorization.json"
    public_key = "env://AUTOSQL_BOOTSTRAP_AUTH_PUBLIC"
    issuer = "security"
    signer = "dba"
    purpose = "bootstrap-authorization"
  }
}`)
	doc, err := source.LoadContext(context.Background(), source.Input{URI: "bootstrap.hcl", Format: source.FormatHCLSource, Data: raw})
	if err != nil {
		t.Fatal(err)
	}
	var database schema.Resource
	for _, resource := range doc.Graph.Resources {
		if resource.Kind == schema.KindDatabase {
			database = resource
		}
	}
	ref, err := BootstrapAuthorizationReferenceFromResource(database)
	if err != nil || ref == nil || ref.Manifest != "file:///run/autosql/bootstrap-authorization.json" {
		t.Fatalf("reference=%+v err=%v", ref, err)
	}
	formatted, err := source.FormatHCL(doc)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := source.LoadContext(context.Background(), source.Input{URI: "formatted.hcl", Format: source.FormatHCLSource, Data: formatted})
	if err != nil {
		t.Fatal(err)
	}
	if len(reloaded.Graph.Resources) != len(doc.Graph.Resources) {
		t.Fatalf("round-trip resource count changed: %d != %d", len(reloaded.Graph.Resources), len(doc.Graph.Resources))
	}
}

func TestBootstrapAuthorizationReferenceRejectsCredentialAndNonRuntimeFields(t *testing.T) {
	for _, raw := range []string{
		`{"bootstrap_authorization":{"manifest":"authorization.json","public_key":"env://PUB","issuer":"security","signer":"dba","purpose":"bootstrap-authorization"}}`,
		`{"bootstrap_authorization":{"manifest":"file:///authorization.json","public_key":"env://PUB","issuer":"security","signer":"dba","purpose":"bootstrap-authorization","private_key":"secret"}}`,
	} {
		if _, err := BootstrapAuthorizationReferenceFromResource(schema.Resource{Kind: schema.KindDatabase, Spec: []byte(raw)}); err == nil {
			t.Fatalf("unsafe reference accepted: %s", raw)
		}
	}
}
