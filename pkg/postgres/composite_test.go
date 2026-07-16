package postgres

import (
	"context"
	"encoding/json"
	"os"
	"testing"

	"autosql/pkg/schema"
	"autosql/pkg/source"

	"github.com/jackc/pgx/v5"
)

func TestCompositeAttributeModelRejectsUnknownAndInexactDependencies(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	enum := renderResource(schema.KindEnum, schema.Name{Schema: "app", Name: "mood", Parent: ns.ID}, `{"values":["good"]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	good := renderResource(schema.KindComposite, schema.Name{Schema: "app", Name: "profile", Parent: ns.ID}, `{"attributes":[{"name":"display name","type":"text","ordinal":1,"collation":"pg_catalog.\"C\""},{"name":"mood","type":"app.mood[]","ordinal":2}]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains}, schema.Dependency{Target: enum.ID, Type: schema.DependencyUses})
	resources := resourceMapFromSlice([]schema.Resource{ns, enum, good})
	if err := validateCompositeSpec(good, resources); err != nil {
		t.Fatal(err)
	}
	cases := []string{
		`{"attributes":[{"name":"x","type":"app.mood","ordinal":1,"future":true}]}`,
		`{"attributes":[{"name":"x","type":"app.mood","ordinal":2}]}`,
		`{"attributes":[{"name":"x","type":"text","ordinal":1,"not_null":true}]}`,
		`{"attributes":[{"name":"x","type":"text; drop schema app","ordinal":1}]}`,
	}
	for _, raw := range cases {
		bad := good
		bad.Spec = []byte(raw)
		if err := validateCompositeSpec(bad, resources); err == nil {
			t.Fatalf("invalid composite passed: %s", raw)
		}
	}
	missing := good
	missing.Dependencies = []schema.Dependency{{Target: ns.ID, Type: schema.DependencyContains}}
	resources[missing.ID] = missing
	if err := validateCompositeSpec(missing, resources); err == nil {
		t.Fatal("missing nested type dependency passed")
	}
}

func TestCompositeInspectHCLRoundTripPreservesOrderTypesAndCollation(t *testing.T) {
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
	_, err = conn.Exec(ctx, `drop extension if exists hstore cascade;
drop schema if exists autosql_composite_model cascade;
create schema autosql_composite_model;
create extension hstore with schema autosql_composite_model;
create type autosql_composite_model.mood as enum ('good','bad');
create domain autosql_composite_model.positive as integer check (value > 0);
create type autosql_composite_model.location as (street text, zip integer);
create type autosql_composite_model.profile as (
  "display name" text collate "C",
  moods autosql_composite_model.mood[],
  score autosql_composite_model.positive,
  home autosql_composite_model.location,
  metadata autosql_composite_model.hstore
);
alter type autosql_composite_model.profile drop attribute score`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop extension if exists hstore cascade; drop schema if exists autosql_composite_model cascade`)
	doc, err := InspectURL(ctx, url, Options{Schemas: []string{"autosql_composite_model"}})
	if err != nil {
		t.Fatal(err)
	}
	var profile schema.Resource
	for _, resource := range doc.Graph.Resources {
		if resource.Kind == schema.KindComposite && resource.Name.Name == "profile" {
			profile = resource
		}
	}
	if profile.ID == "" {
		t.Fatal("profile composite missing")
	}
	attributes, err := parseCompositeAttributes(profile)
	if err != nil {
		t.Fatal(err)
	}
	if len(attributes) != 4 || attributes[0].Name != "display name" || attributes[0].Collation == "" || attributes[1].Type != "autosql_composite_model.mood[]" || attributes[3].Type != "autosql_composite_model.hstore" || attributes[3].Ordinal != 4 {
		t.Fatalf("attributes=%+v", attributes)
	}
	if err := validateCompositeSpec(profile, resourceMapForRender(doc)); err != nil {
		t.Fatal(err)
	}
	hcl, err := source.FormatHCL(doc)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := source.LoadContext(ctx, source.Input{URI: "composites.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	diff, err := schema.Diff(doc, reloaded, schema.DiffOptions{})
	if err != nil || len(diff.Changes) != 0 {
		t.Fatalf("composite HCL drifted: changes=%d err=%v\n%s", len(diff.Changes), err, hcl)
	}
}

func TestCompositeCreateAttributeLifecycleRenameAndGuardedDrop(t *testing.T) {
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
	_, err = conn.Exec(ctx, `drop schema if exists autosql_composite_lifecycle cascade`)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Exec(context.Background(), `drop schema if exists autosql_composite_lifecycle cascade`)

	ns := renderResource(schema.KindSchema, schema.Name{Name: "autosql_composite_lifecycle"}, `{}`)
	composite := renderResource(schema.KindComposite, schema.Name{Schema: ns.Name.Name, Name: "profile", Parent: ns.ID}, `{"attributes":[{"name":"name","type":"text","ordinal":1},{"name":"count","type":"integer","ordinal":2}]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	current := schema.Document{Version: schema.SchemaVersion, Annotations: map[string]string{"dialect": "postgresql"}}
	desired := schema.Document{Version: schema.SchemaVersion, Annotations: map[string]string{"dialect": "postgresql"}, Graph: schema.Graph{Resources: []schema.Resource{ns, composite}}}
	desired.Normalize()
	applyExtensionTransition(t, ctx, conn, current, desired, schema.DiffOptions{}, nil)
	assertCompositeConverges(t, ctx, url, desired)

	current = desired
	desired = compositeWithAttributes(current, []map[string]any{
		{"name": "label", "type": "text", "ordinal": 1},
		{"name": "count", "type": "integer", "ordinal": 2},
		{"name": "tags", "type": "text[]", "ordinal": 3},
	})
	applyExtensionTransition(t, ctx, conn, current, desired, schema.DiffOptions{}, nil)
	assertCompositeConverges(t, ctx, url, desired)

	current = desired
	desired = compositeWithAttributes(current, []map[string]any{
		{"name": "label", "type": "text", "ordinal": 1},
		{"name": "tags", "type": "text[]", "ordinal": 2},
	})
	applyExtensionTransition(t, ctx, conn, current, desired, schema.DiffOptions{}, nil)
	assertCompositeConverges(t, ctx, url, desired)

	current = desired
	desired = compositeWithAttributes(current, []map[string]any{
		{"name": "label", "type": "text", "ordinal": 1},
		{"name": "tags", "type": "character varying(20)[]", "ordinal": 2},
	})
	if statements, err := renderExtensionTransition(ctx, current, desired, schema.DiffOptions{}, nil); err == nil || len(statements) != 0 {
		t.Fatalf("unguarded attribute type change statements=%v err=%v", statements, err)
	}
	applyExtensionTransition(t, ctx, conn, current, desired, schema.DiffOptions{}, map[string]string{"allow_composite_attribute_type_change": "true"})
	assertCompositeConverges(t, ctx, url, desired)

	current = desired
	renamed := desired
	renamed.Graph.Resources = append([]schema.Resource(nil), desired.Graph.Resources...)
	var oldID, newID string
	for index := range renamed.Graph.Resources {
		if renamed.Graph.Resources[index].Kind == schema.KindComposite {
			oldID = renamed.Graph.Resources[index].ID
			renamed.Graph.Resources[index].Name.Name = "account_profile"
			renamed.Graph.Resources[index].ID = schema.StableID(schema.KindComposite, renamed.Graph.Resources[index].Name)
			newID = renamed.Graph.Resources[index].ID
		}
	}
	renamed.Normalize()
	applyExtensionTransition(t, ctx, conn, current, renamed, schema.DiffOptions{RenameHints: []schema.RenameHint{{From: oldID, To: newID}}}, nil)
	assertCompositeConverges(t, ctx, url, renamed)

	without := renamed
	without.Graph.Resources = []schema.Resource{ns}
	if statements, err := renderExtensionTransition(ctx, renamed, without, schema.DiffOptions{}, nil); err == nil || len(statements) != 0 {
		t.Fatalf("unguarded composite drop statements=%v err=%v", statements, err)
	}
	applyExtensionTransition(t, ctx, conn, renamed, without, schema.DiffOptions{}, map[string]string{"allow_composite_drop": "true"})
	assertCompositeConverges(t, ctx, url, without)
}

func compositeWithAttributes(document schema.Document, attributes []map[string]any) schema.Document {
	out := document
	out.Graph.Resources = append([]schema.Resource(nil), document.Graph.Resources...)
	for index := range out.Graph.Resources {
		if out.Graph.Resources[index].Kind != schema.KindComposite {
			continue
		}
		values := spec(out.Graph.Resources[index])
		values["attributes"] = attributes
		out.Graph.Resources[index].Spec, _ = json.Marshal(values)
	}
	out.Normalize()
	return out
}

func assertCompositeConverges(t *testing.T, ctx context.Context, url string, desired schema.Document) {
	t.Helper()
	actual, err := InspectURL(ctx, url, Options{Schemas: []string{"autosql_composite_lifecycle"}})
	if err != nil {
		t.Fatal(err)
	}
	diff, err := schema.Diff(actual, desired, schema.DiffOptions{})
	if err != nil || len(diff.Changes) != 0 {
		t.Fatalf("composite did not converge: changes=%+v err=%v", diff.Changes, err)
	}
}
