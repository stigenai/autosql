package postgres

import (
	"context"
	"encoding/json"
	"os"
	"slices"
	"strings"
	"testing"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
	"autosql/pkg/source"

	"github.com/jackc/pgx/v5"
)

func TestExtensionPreflightAggregatesPolicyAndAvailability(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	pgcrypto := renderResource(schema.KindExtension, schema.Name{Schema: "app", Name: "pgcrypto", Parent: ns.ID}, `{"version":"1.3","relocatable":true,"trusted":true,"superuser":true,"requires":[],"owner":"postgres"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	hstore := renderResource(schema.KindExtension, schema.Name{Schema: "app", Name: "hstore", Parent: ns.ID}, `{"version":"9.9","relocatable":true,"trusted":true,"superuser":true,"requires":[],"owner":"postgres"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	doc := schema.Document{Version: schema.SchemaVersion, Annotations: map[string]string{"dialect": "postgresql"}, Graph: schema.Graph{Resources: []schema.Resource{ns, pgcrypto, hstore}}}
	catalog := ExtensionCatalog{Versions: []ExtensionAvailability{
		{Name: "pgcrypto", Version: "1.3", Relocatable: true, Superuser: true, Trusted: false, Requires: []string{"plpgsql"}},
		{Name: "hstore", Version: "1.8", Schema: "public", Superuser: true, Trusted: true},
	}}
	policy := ExtensionPolicy{
		Allowed:        map[string]bool{"pgcrypto": true},
		Versions:       map[string][]string{"pgcrypto": {"1.2"}, "hstore": {"1.8"}},
		Schemas:        map[string][]string{"pgcrypto": {"public"}, "hstore": {"public"}},
		RequireTrusted: true,
	}
	report, err := PreflightExtensions(doc, catalog, policy)
	if err != nil {
		t.Fatal(err)
	}
	if report.Supported {
		t.Fatal("unsupported extension inventory passed preflight")
	}
	got := make([]string, 0, len(report.Diagnostics))
	for _, diagnostic := range report.Diagnostics {
		got = append(got, diagnostic.Name+":"+diagnostic.Class)
	}
	want := []string{"hstore:allowlist", "hstore:package", "pgcrypto:authority", "pgcrypto:dependency", "pgcrypto:schema", "pgcrypto:version"}
	if !slices.Equal(got, want) {
		t.Fatalf("diagnostics=%v want=%v", got, want)
	}
}

func TestExtensionRequirementsRoundTripHCL(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	first := renderResource(schema.KindExtension, schema.Name{Schema: "app", Name: "pgcrypto", Parent: ns.ID}, `{"version":"1.3","relocatable":true,"trusted":true,"superuser":true,"requires":[],"owner":"postgres"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	first.Annotations = map[string]string{"comment": "cryptographic functions"}
	second := renderResource(schema.KindExtension, schema.Name{Schema: "app", Name: "uuid-ossp", Parent: ns.ID}, `{"version":"1.1","relocatable":true,"trusted":true,"superuser":true,"requires":["plpgsql"],"owner":"postgres"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	doc := schema.Document{Version: schema.SchemaVersion, Annotations: map[string]string{"dialect": "postgresql"}, Graph: schema.Graph{Resources: []schema.Resource{ns, first, second}}}
	hcl, err := source.FormatHCL(doc)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := source.LoadContext(context.Background(), source.Input{URI: "extensions.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	diff, err := schema.Diff(doc, reloaded, schema.DiffOptions{})
	if err != nil || len(diff.Changes) != 0 {
		t.Fatalf("extension HCL round trip drifted: changes=%d err=%v\n%s", len(diff.Changes), err, hcl)
	}
}

func TestValidateExtensionTransitionUsesServerAdvertisedPath(t *testing.T) {
	catalog := ExtensionCatalog{Paths: []ExtensionUpdatePath{{Name: "hstore", Source: "1.7", Target: "1.8", Path: "1.7--1.8"}}}
	if err := ValidateExtensionTransition("hstore", "1.7", "1.8", catalog); err != nil {
		t.Fatal(err)
	}
	if err := ValidateExtensionTransition("hstore", "1.8", "1.7", catalog); err == nil {
		t.Fatal("unavailable downgrade path passed")
	}
}

func TestInspectExtensionCatalogURL(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	catalog, err := InspectExtensionCatalogURL(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, version := range catalog.Versions {
		if version.Name == "pgcrypto" && version.Version != "" {
			found = true
			break
		}
	}
	if !found {
		t.Fatal("server catalog did not expose pgcrypto")
	}
}

func TestExtensionCreateUpdateSchemaMoveAndGuardedDrop(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	_, err = conn.Exec(ctx, `drop extension if exists hstore cascade; drop schema if exists autosql_extension_a cascade; drop schema if exists autosql_extension_b cascade`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop extension if exists hstore cascade; drop schema if exists autosql_extension_a cascade; drop schema if exists autosql_extension_b cascade`)

	nsA := renderResource(schema.KindSchema, schema.Name{Name: "autosql_extension_a"}, `{}`)
	nsB := renderResource(schema.KindSchema, schema.Name{Name: "autosql_extension_b"}, `{}`)
	extension := renderResource(schema.KindExtension, schema.Name{Schema: nsA.Name.Name, Name: "hstore", Parent: nsA.ID}, `{"version":"1.7","relocatable":true,"trusted":true,"superuser":true,"requires":[]}`, schema.Dependency{Target: nsA.ID, Type: schema.DependencyContains})
	extension.Annotations = map[string]string{"comment": "managed hstore"}
	current := schema.Document{Version: schema.SchemaVersion, Annotations: map[string]string{"dialect": "postgresql"}}
	desired := schema.Document{Version: schema.SchemaVersion, Annotations: map[string]string{"dialect": "postgresql"}, Graph: schema.Graph{Resources: []schema.Resource{nsA, nsB, extension}}}
	desired.Normalize()
	options := map[string]string{"extension_allowlist": "hstore", "extension_version.hstore": "1.7", "extension_schemas.hstore": "autosql_extension_a,autosql_extension_b"}
	applyExtensionTransition(t, ctx, conn, current, desired, schema.DiffOptions{}, options)
	assertInstalledExtension(t, ctx, conn, "hstore", "1.7", "autosql_extension_a")

	current = desired
	updated := desired
	updated.Graph.Resources = append([]schema.Resource(nil), desired.Graph.Resources...)
	for index := range updated.Graph.Resources {
		if updated.Graph.Resources[index].Kind == schema.KindExtension {
			updated.Graph.Resources[index].Spec = []byte(`{"version":"1.8","relocatable":true,"trusted":true,"superuser":true,"requires":[]}`)
		}
	}
	options["extension_version.hstore"] = "1.8"
	applyExtensionTransition(t, ctx, conn, current, updated, schema.DiffOptions{}, options)
	assertInstalledExtension(t, ctx, conn, "hstore", "1.8", "autosql_extension_a")

	current = updated
	moved := updated
	moved.Graph.Resources = append([]schema.Resource(nil), updated.Graph.Resources...)
	var oldID, newID string
	for index := range moved.Graph.Resources {
		if moved.Graph.Resources[index].Kind != schema.KindExtension {
			continue
		}
		oldID = moved.Graph.Resources[index].ID
		moved.Graph.Resources[index].Name.Schema = nsB.Name.Name
		moved.Graph.Resources[index].Name.Parent = nsB.ID
		moved.Graph.Resources[index].Dependencies = []schema.Dependency{{Target: nsB.ID, Type: schema.DependencyContains}}
		moved.Graph.Resources[index].ID = schema.StableID(schema.KindExtension, moved.Graph.Resources[index].Name)
		newID = moved.Graph.Resources[index].ID
	}
	moved.Normalize()
	applyExtensionTransition(t, ctx, conn, current, moved, schema.DiffOptions{RenameHints: []schema.RenameHint{{From: oldID, To: newID}}}, options)
	assertInstalledExtension(t, ctx, conn, "hstore", "1.8", "autosql_extension_b")

	withoutExtension := moved
	withoutExtension.Graph.Resources = nil
	for _, resource := range moved.Graph.Resources {
		if resource.Kind != schema.KindExtension {
			withoutExtension.Graph.Resources = append(withoutExtension.Graph.Resources, resource)
		}
	}
	if _, err := renderExtensionTransition(ctx, moved, withoutExtension, schema.DiffOptions{}, options); err == nil || !strings.Contains(err.Error(), "allow_extension_drop") {
		t.Fatalf("unguarded extension drop err=%v", err)
	}
	options["allow_extension_drop"] = "true"
	applyExtensionTransition(t, ctx, conn, moved, withoutExtension, schema.DiffOptions{}, options)
	var installed bool
	if err := conn.QueryRow(ctx, `select exists(select 1 from pg_extension where extname='hstore')`).Scan(&installed); err != nil || installed {
		t.Fatalf("installed=%v err=%v", installed, err)
	}
}

func TestExtensionOwnedMembersDoNotProduceDuplicateLifecycle(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	conn, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	_, err = conn.Exec(ctx, `drop extension if exists pgcrypto cascade; drop schema if exists autosql_extension_members cascade; create schema autosql_extension_members; create extension pgcrypto with schema autosql_extension_members; create table autosql_extension_members.tokens(id uuid default autosql_extension_members.gen_random_uuid())`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop extension if exists pgcrypto cascade; drop schema if exists autosql_extension_members cascade`)

	doc, err := InspectURL(ctx, url, Options{Schemas: []string{"autosql_extension_members"}})
	if err != nil {
		t.Fatal(err)
	}
	resources := resourceMapForRender(doc)
	var extension schema.Resource
	ownedRoutines := 0
	for _, resource := range doc.Graph.Resources {
		if resource.Kind == schema.KindExtension && resource.Name.Name == "pgcrypto" {
			extension = resource
		}
		if (resource.Kind == schema.KindFunction || resource.Kind == schema.KindProcedure) && stringValue(spec(resource), "extension") == "pgcrypto" {
			ownedRoutines++
			if extensionOwnerID(resource, resources) == "" {
				t.Fatalf("retained member lacks extension ownership: %s", resource.Name.String())
			}
		}
	}
	if extension.ID == "" {
		t.Fatal("pgcrypto extension missing from inspection")
	}
	// pgcrypto creates many functions; only an application-referenced member
	// may remain in the graph as an inert prerequisite.
	if ownedRoutines > 1 {
		t.Fatalf("unreferenced extension members leaked into graph: %d", ownedRoutines)
	}

	hcl, err := source.FormatHCL(doc)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := source.LoadContext(ctx, source.Input{URI: "extension-members.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	diff, err := schema.Diff(doc, reloaded, schema.DiffOptions{})
	if err != nil || len(diff.Changes) != 0 {
		t.Fatalf("extension member HCL drifted: changes=%d err=%v", len(diff.Changes), err)
	}

	if ownedRoutines == 1 {
		mutated := doc
		mutated.Graph.Resources = append([]schema.Resource(nil), doc.Graph.Resources...)
		for index := range mutated.Graph.Resources {
			resource := &mutated.Graph.Resources[index]
			if stringValue(spec(*resource), "extension") != "pgcrypto" {
				continue
			}
			values := spec(*resource)
			values["definition"] = stringValue(values, "definition") + " "
			resource.Spec, _ = json.Marshal(values)
		}
		changes, err := schema.Diff(doc, mutated, schema.DiffOptions{})
		if err != nil {
			t.Fatal(err)
		}
		statements, renderErr := New().Render(ctx, plugin.RenderRequest{Changes: changes, Current: doc, Desired: mutated})
		if renderErr == nil || len(statements) != 0 || !strings.Contains(renderErr.Error(), "extension-owned resource") {
			t.Fatalf("independent member change statements=%v err=%v", statements, renderErr)
		}
	}
}

func TestExtensionOwnedMemberChangesRequireOwningExtensionTransition(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	extension := renderResource(schema.KindExtension, schema.Name{Schema: "app", Name: "hstore", Parent: ns.ID}, `{"version":"1.7","relocatable":true,"trusted":true,"superuser":true,"requires":[]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	function := renderResource(schema.KindFunction, schema.Name{Schema: "app", Name: "member(text)", Parent: ns.ID}, `{"name":"member","identity_arguments":"text","arguments":"text","result":"text","language":"sql","volatility":"i","strict":false,"security_definer":false,"leakproof":false,"parallel":"s","cost":1,"rows":0,"configuration":[],"definition":"select $1","body_digest":"one","extension":"hstore"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains}, schema.Dependency{Target: extension.ID, Type: schema.DependencyOwns})
	current := schema.Document{Version: schema.SchemaVersion, Annotations: map[string]string{"dialect": "postgresql"}, Graph: schema.Graph{Resources: []schema.Resource{ns, extension, function}}}
	desired := current
	desired.Graph.Resources = append([]schema.Resource(nil), current.Graph.Resources...)
	for index := range desired.Graph.Resources {
		if desired.Graph.Resources[index].Kind == schema.KindFunction {
			desired.Graph.Resources[index].Spec = []byte(`{"name":"member","identity_arguments":"text","arguments":"text","result":"text","language":"sql","volatility":"i","strict":false,"security_definer":false,"leakproof":false,"parallel":"s","cost":1,"rows":0,"configuration":[],"definition":"select upper($1)","body_digest":"two","extension":"hstore"}`)
		}
	}
	changes, err := schema.Diff(current, desired, schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if statements, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired}); err == nil || len(statements) != 0 {
		t.Fatalf("independent member statements=%v err=%v", statements, err)
	}

	for index := range desired.Graph.Resources {
		if desired.Graph.Resources[index].Kind == schema.KindExtension {
			desired.Graph.Resources[index].Spec = []byte(`{"version":"1.8","relocatable":true,"trusted":true,"superuser":true,"requires":[]}`)
		}
	}
	changes, err = schema.Diff(current, desired, schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	statements, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired, Options: map[string]string{"extension_allowlist": "hstore", "extension_version.hstore": "1.8", "extension_schemas.hstore": "app"}})
	if err != nil {
		t.Fatal(err)
	}
	executable, topology := 0, 0
	for _, statement := range statements {
		if statement.Kind == plugin.StatementTopology {
			topology++
		} else if strings.Contains(statement.SQL, `ALTER EXTENSION "hstore" UPDATE TO '1.8'`) {
			executable++
		}
	}
	if executable != 1 || topology != 1 {
		t.Fatalf("statements=%+v", statements)
	}
}

func renderExtensionTransition(ctx context.Context, current, desired schema.Document, diffOptions schema.DiffOptions, options map[string]string) ([]plugin.Statement, error) {
	changes, err := schema.Diff(current, desired, diffOptions)
	if err != nil {
		return nil, err
	}
	return New().Render(ctx, plugin.RenderRequest{Changes: changes, Current: current, Desired: desired, Options: options})
}

func applyExtensionTransition(t *testing.T, ctx context.Context, conn *pgx.Conn, current, desired schema.Document, diffOptions schema.DiffOptions, options map[string]string) {
	t.Helper()
	statements, err := renderExtensionTransition(ctx, current, desired, diffOptions, options)
	if err != nil {
		t.Fatal(err)
	}
	for _, statement := range statements {
		if statement.Kind == plugin.StatementTopology {
			continue
		}
		if _, err := conn.Exec(ctx, statement.SQL); err != nil {
			t.Fatalf("%s: %v", statement.SQL, err)
		}
	}
}

func assertInstalledExtension(t *testing.T, ctx context.Context, conn *pgx.Conn, name, version, namespace string) {
	t.Helper()
	var gotVersion, gotSchema string
	err := conn.QueryRow(ctx, `select e.extversion,n.nspname from pg_extension e join pg_namespace n on n.oid=e.extnamespace where e.extname=$1`, name).Scan(&gotVersion, &gotSchema)
	if err != nil || gotVersion != version || gotSchema != namespace {
		t.Fatalf("extension %s version=%s schema=%s err=%v", name, gotVersion, gotSchema, err)
	}
}
