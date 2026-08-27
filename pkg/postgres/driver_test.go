package postgres

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
	"github.com/jackc/pgx/v5/pgconn"
)

func TestDriverMetadata(t *testing.T) {
	d := New()
	if err := plugin.ValidateDriver(d); err != nil {
		t.Fatalf("ValidateDriver: %v", err)
	}
	for kind, want := range map[schema.Kind]plugin.Mode{schema.KindSchema: plugin.Managed, schema.KindView: plugin.Managed, schema.KindMaterializedView: plugin.Managed, schema.KindTable: plugin.Managed, schema.KindColumn: plugin.Managed, schema.KindEnum: plugin.Managed, schema.KindDomain: plugin.Managed, schema.KindSequence: plugin.Managed, schema.KindIndex: plugin.Managed, schema.KindPrimaryKey: plugin.Managed, schema.KindUniqueConstraint: plugin.Managed, schema.KindCheckConstraint: plugin.Managed, schema.KindForeignKey: plugin.Managed, schema.KindPolicy: plugin.Managed, schema.KindRole: plugin.Managed, schema.KindGrant: plugin.Managed} {
		if got := d.Info().Capability(kind).Mode; got != want {
			t.Errorf("capability %s = %s, want %s", kind, got, want)
		}
	}
	all := []schema.Operation{schema.OperationCreate, schema.OperationAlter, schema.OperationDrop, schema.OperationRename}
	for kind, want := range map[schema.Kind][]schema.Operation{
		schema.KindSchema:           all,
		schema.KindEnum:             all,
		schema.KindDomain:           all,
		schema.KindSequence:         all,
		schema.KindTable:            all,
		schema.KindColumn:           all,
		schema.KindView:             all,
		schema.KindMaterializedView: all,
	} {
		if got := d.Info().Capability(kind).Operations; !reflect.DeepEqual(got, want) {
			t.Errorf("operations %s = %v, want %v", kind, got, want)
		}
	}
	if got := d.Info().Capability(schema.KindIndex).Operations; len(got) != 4 {
		t.Fatalf("managed index operations: %v", got)
	}
}

func TestPermissionError(t *testing.T) {
	cause := errors.New("catalog rejected access")
	err := &PermissionError{Resource: "roles", Privilege: "CREATEROLE", Cause: cause}
	if !errors.Is(err, ErrPermission) || !errors.Is(err, cause) {
		t.Fatalf("permission error does not preserve classification and cause: %v", err)
	}
	if got := err.Error(); !strings.Contains(got, "roles") || !strings.Contains(got, "grant CREATEROLE") {
		t.Fatalf("permission error is not actionable: %s", got)
	}
}

func TestTransientCatalogOIDClassificationIsNarrowAndPreserved(t *testing.T) {
	for _, message := range []string{"could not open relation with OID 42", "cache lookup failed for relation 42"} {
		cause := &pgconn.PgError{Code: "XX000", Message: message}
		classified := classify("indexes", "catalog metadata", "postgres://user:secret@db/app", cause)
		if !transientCatalogOID(classified) || !errors.Is(classified, cause) {
			t.Fatalf("catalog invalidation was not retryable/preserved: %v", classified)
		}
		if strings.Contains(classified.Error(), "secret") {
			t.Fatalf("classified catalog error leaked DSN: %v", classified)
		}
	}
	for _, cause := range []*pgconn.PgError{{Code: "XX000", Message: "unrelated internal error"}, {Code: "42P01", Message: "could not open relation with OID 42"}} {
		if transientCatalogOID(classify("indexes", "catalog metadata", "", cause)) {
			t.Fatalf("unrelated PostgreSQL error became retryable: %v", cause)
		}
	}
}

func TestRedactDSN(t *testing.T) {
	dsn := "postgres://alice:top-secret@db.example:5433/app?sslmode=require"
	got := redactDSN(dsn)
	if strings.Contains(got, "top-secret") || strings.Contains(got, "sslmode") {
		t.Fatalf("redacted DSN leaked secret or query: %s", got)
	}
	if !strings.Contains(got, "alice@db.example/app") {
		t.Fatalf("redacted DSN lost useful endpoint identity: %s", got)
	}
	bad := redactDSN("postgres://user:secret@%")
	if strings.Contains(bad, "secret") {
		t.Fatalf("malformed DSN leaked secret: %s", bad)
	}
}

func TestFilterDocumentDependencyClosure(t *testing.T) {
	ns := resource(schema.KindSchema, schema.Name{Name: "app"})
	table := resource(schema.KindTable, schema.Name{Schema: "app", Name: "users", Parent: ns.ID})
	table.Dependencies = dep(ns.ID, schema.DependencyContains)
	col := resource(schema.KindColumn, schema.Name{Schema: "app", Name: "id", Parent: table.ID})
	col.Dependencies = dep(table.ID, schema.DependencyContains)
	other := resource(schema.KindTable, schema.Name{Schema: "app", Name: "logs", Parent: ns.ID})
	other.Dependencies = dep(ns.ID, schema.DependencyContains)
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{other, col, table, ns}}}

	got := filterDocument(doc, []string{"app"}, []string{"table:app.users"}, nil)
	if err := got.Validate(); err != nil {
		t.Fatalf("filtered document invalid: %v", err)
	}
	kinds := map[schema.Kind]int{}
	for _, r := range got.Graph.Resources {
		kinds[r.Kind]++
		if r.Name.Name == "logs" {
			t.Fatal("unselected table retained")
		}
	}
	if kinds[schema.KindSchema] != 1 || kinds[schema.KindTable] != 1 || kinds[schema.KindColumn] != 1 {
		t.Fatalf("unexpected projection: %#v", kinds)
	}
}

func TestMergeManagedAdvancedResourcesKeepsOnlyDesiredRoleScope(t *testing.T) {
	ns := resource(schema.KindSchema, schema.Name{Name: "app"})
	table := resource(schema.KindTable, schema.Name{Schema: "app", Name: "users", Parent: ns.ID})
	owner := resource(schema.KindRole, schema.Name{Name: "app_owner"})
	postgresRole := resource(schema.KindRole, schema.Name{Name: "postgres"})
	managedTable := table
	managedTable.Dependencies = []schema.Dependency{{Target: owner.ID, Type: schema.DependencyOwns}}
	managedSchema := ns
	managedSchema.Dependencies = []schema.Dependency{{Target: owner.ID, Type: schema.DependencyOwns}}
	managed := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{managedSchema, managedTable, owner}}}
	advancedTable := managedTable
	advancedTable.Spec = []byte(`{"owner":"app_owner"}`)
	advancedSchema := ns
	advancedSchema.Spec = []byte(`{"owner":"postgres"}`)
	advancedSchema.Dependencies = []schema.Dependency{{Target: postgresRole.ID, Type: schema.DependencyOwns}}
	base := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table}}}
	advanced := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{advancedSchema, advancedTable, owner, postgresRole}}}
	got := mergeManagedAdvancedResources(base, advanced, managedAdvancedResourceIDs(managed.Graph.Resources))
	resources := map[string]schema.Resource{}
	for _, candidate := range got.Graph.Resources {
		resources[candidate.ID] = candidate
	}
	if string(resources[table.ID].Spec) != string(advancedTable.Spec) || resources[owner.ID].Kind != schema.KindRole {
		t.Fatalf("managed resources not merged: %#v", resources)
	}
	if _, imported := resources[postgresRole.ID]; imported {
		t.Fatal("unrelated cluster role imported")
	}
	if string(resources[ns.ID].Spec) != string(ns.Spec) {
		t.Fatal("resource with unmanaged owner dependency replaced")
	}
}

func TestNormalizeBindsDeclaredRolesFromManagedSpecifications(t *testing.T) {
	role := resource(schema.KindRole, schema.Name{Name: "app"})
	function := resource(schema.KindFunction, schema.Name{Schema: "public", Name: "f()"})
	function.Spec = []byte(`{"owner":"app"}`)
	policy := resource(schema.KindPolicy, schema.Name{Schema: "public", Name: "tenant_access"})
	policy.Spec = []byte(`{"roles":["app","public"]}`)
	doc, err := New().Normalize(context.Background(), schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{function, policy, role}}})
	if err != nil {
		t.Fatal(err)
	}
	resources := map[string]schema.Resource{}
	for _, candidate := range doc.Graph.Resources {
		resources[candidate.ID] = candidate
	}
	assertDependency := func(resource schema.Resource, dependencyType schema.DependencyType) {
		t.Helper()
		for _, dependency := range resource.Dependencies {
			if dependency.Target == role.ID && dependency.Type == dependencyType {
				return
			}
		}
		t.Fatalf("resource %s dependencies=%v", resource.ID, resource.Dependencies)
	}
	assertDependency(resources[function.ID], schema.DependencyOwns)
	assertDependency(resources[policy.ID], schema.DependencyReferences)
}

func resource(kind schema.Kind, name schema.Name) schema.Resource {
	return schema.Resource{ID: schema.StableID(kind, name), Kind: kind, Name: name, Spec: []byte(`{}`)}
}
