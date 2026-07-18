package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"testing"

	"autosql/pkg/plan"
	"autosql/pkg/plugin"
	"autosql/pkg/schema"
	"autosql/pkg/source"
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

func TestCellProvisioningConstraintIndexInventoryIsManaged(t *testing.T) {
	// This anonymized inventory is the acceptance baseline supplied by the
	// production cell. A change to capability coverage must account for every
	// object rather than allowing the next unsupported layer to surface later.
	inventory := map[schema.Kind]int{
		schema.KindIndex:            315,
		schema.KindPrimaryKey:       69,
		schema.KindForeignKey:       56,
		schema.KindCheckConstraint:  45,
		schema.KindUniqueConstraint: 27,
	}
	total := 0
	for kind, count := range inventory {
		total += count
		capability := New().Info().Capability(kind)
		if capability.Mode != plugin.Managed || len(capability.Operations) != 4 {
			t.Errorf("%s (%d objects) capability=%+v", kind, count, capability)
		}
	}
	if total != 512 {
		t.Fatalf("inventory total=%d want 512", total)
	}
}

func TestCheckConstraintQualifiesDeclaredEnumCastDependencies(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "global"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "global", Name: "cells", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	cellType := renderResource(schema.KindEnum, schema.Name{Schema: "global", Name: "cell_type", Parent: ns.ID}, `{"values":["dedicated","isolated","shared"]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	typeColumn := renderResource(schema.KindColumn, schema.Name{Schema: "global", Name: "type", Parent: table.ID}, `{"type":"global.cell_type","not_null":true,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: cellType.ID, Type: schema.DependencyUses})
	check := renderResource(schema.KindCheckConstraint, schema.Name{Schema: "global", Name: "cells_dedicated_capacity_check", Parent: table.ID}, `{"definition":"CHECK (type = ANY (ARRAY['dedicated'::cell_type, 'isolated'::cell_type, 'shared'::cell_type]))","columns":["type"],"deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: typeColumn.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: cellType.ID, Type: schema.DependencyUses})
	resources := []schema.Resource{ns, table, cellType, typeColumn, check}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: resources}}
	current := desired
	current.Graph.Resources = resources[:len(resources)-1]
	changes := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "create", Operation: schema.OperationCreate, ResourceID: check.ID, After: &check}}}

	out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired})
	if err != nil || len(out) != 1 {
		t.Fatalf("statements=%+v err=%v", out, err)
	}
	if !strings.Contains(out[0].SQL, `'dedicated'::global.cell_type`) || !strings.Contains(out[0].SQL, `'isolated'::global.cell_type`) || !strings.Contains(out[0].SQL, `'shared'::global.cell_type`) {
		t.Fatalf("constraint casts are not bound to the declared enum dependency: %s", out[0].SQL)
	}
	otherType := renderResource(schema.KindEnum, schema.Name{Schema: "global", Name: "other_type", Parent: ns.ID}, `{"values":["dedicated"]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	for _, fixture := range []struct {
		name string
		deps []schema.Dependency
	}{
		{"missing", []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}, {Target: typeColumn.ID, Type: schema.DependencyReferences}}},
		{"mismatched", []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}, {Target: typeColumn.ID, Type: schema.DependencyReferences}, {Target: otherType.ID, Type: schema.DependencyUses}}},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			unsafe := check
			unsafe.Dependencies = fixture.deps
			unsafeDesired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, cellType, otherType, typeColumn, unsafe}}}
			unsafeCurrent := unsafeDesired
			unsafeCurrent.Graph.Resources = unsafeDesired.Graph.Resources[:len(unsafeDesired.Graph.Resources)-1]
			unsafeChanges := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "create", Operation: schema.OperationCreate, ResourceID: unsafe.ID, After: &unsafe}}}
			if rendered, renderErr := New().Render(context.Background(), plugin.RenderRequest{Changes: unsafeChanges, Current: unsafeCurrent, Desired: unsafeDesired}); renderErr == nil || len(rendered) != 0 {
				t.Fatalf("unsafe constraint dependency rendered: statements=%+v err=%v", rendered, renderErr)
			}
		})
	}
}

func TestNormalizeDerivesOnlyExactLegacyCheckTypeDependencies(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "global"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "global", Name: "cells", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	cellType := renderResource(schema.KindEnum, schema.Name{Schema: "global", Name: "cell_type", Parent: ns.ID}, `{"values":["dedicated","isolated"]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	otherType := renderResource(schema.KindEnum, schema.Name{Schema: "global", Name: "other_type", Parent: ns.ID}, `{"values":["dedicated"]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	typeColumn := renderResource(schema.KindColumn, schema.Name{Schema: "global", Name: "type", Parent: table.ID}, `{"type":"global.cell_type","not_null":true,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: cellType.ID, Type: schema.DependencyUses})
	legacy := renderResource(schema.KindCheckConstraint, schema.Name{Schema: "global", Name: "cells_type_check", Parent: table.ID}, `{"definition":"CHECK (type = ANY (ARRAY['dedicated'::cell_type, 'isolated'::cell_type]))","columns":["type"],"deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: typeColumn.ID, Type: schema.DependencyReferences})
	resources := []schema.Resource{ns, table, cellType, otherType, typeColumn, legacy}
	normalized, err := New().Normalize(context.Background(), schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: resources}})
	if err != nil {
		t.Fatal(err)
	}
	resourceMap := resourceMapForRender(normalized)
	got := resourceMap[legacy.ID]
	if err := validateSemanticDependencies(got, resourceMap); err != nil {
		t.Fatalf("derived legacy dependency is not exact: %v", err)
	}
	if !strings.Contains(string(got.Spec), `global.cell_type`) {
		t.Fatalf("derived legacy constraint was not schema-bound: %s", got.Spec)
	}

	mismatched := legacy
	mismatched.Dependencies = append(mismatched.Dependencies, schema.Dependency{Target: otherType.ID, Type: schema.DependencyUses})
	resources[len(resources)-1] = mismatched
	normalized, err = New().Normalize(context.Background(), schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: resources}})
	if err != nil {
		t.Fatal(err)
	}
	resourceMap = resourceMapForRender(normalized)
	if err := validateSemanticDependencies(resourceMap[mismatched.ID], resourceMap); err == nil {
		t.Fatal("mismatched declared type dependency was silently repaired")
	}
}

func TestConstraintAndIndexDefinitionsAreParserBounded(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "users", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	resources := map[string]schema.Resource{ns.ID: ns, table.ID: table}

	constraint := renderResource(schema.KindCheckConstraint, schema.Name{Schema: "app", Name: "users_age_check", Parent: table.ID}, `{"definition":"CHECK (age >= 0) NOT VALID","deferrable":false,"initially_deferred":false,"validated":false}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	if _, err := parseConstraintDefinition(constraint, resources); err != nil {
		t.Fatal(err)
	}
	index := renderResource(schema.KindIndex, schema.Name{Schema: "app", Name: "users_email_idx", Parent: table.ID}, `{"definition":"CREATE UNIQUE INDEX users_email_idx ON app.users USING btree (lower(email) text_pattern_ops) INCLUDE (id) WITH (fillfactor='80') WHERE active","method":"btree","unique":true,"valid":true,"ready":true}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	if _, err := parseIndexDefinition(index, resources); err != nil {
		t.Fatal(err)
	}

	for name, resource := range map[string]schema.Resource{
		"second statement": func() schema.Resource {
			r := index
			r.Spec = json.RawMessage(`{"definition":"(email); DROP TABLE app.users"}`)
			return r
		}(),
		"wrong index": func() schema.Resource {
			r := index
			r.Spec = json.RawMessage(`{"definition":"CREATE INDEX another ON app.users (email)"}`)
			return r
		}(),
		"wrong table": func() schema.Resource {
			r := index
			r.Spec = json.RawMessage(`{"definition":"CREATE INDEX users_email_idx ON app.admins (email)"}`)
			return r
		}(),
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := parseIndexDefinition(resource, resources); err == nil {
				t.Fatal("unsafe index definition accepted")
			}
		})
	}

	badConstraint := constraint
	badConstraint.Spec = json.RawMessage(`{"definition":"CHECK (age >= 0); DROP TABLE app.users"}`)
	if _, err := parseConstraintDefinition(badConstraint, resources); err == nil {
		t.Fatal("unsafe constraint definition accepted")
	}
}

func TestMaterializedViewIndexesResolveParentAndOrderAfterView(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "cell"}, `{}`)
	source := renderResource(schema.KindTable, schema.Name{Schema: "cell", Name: "block_health", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	sourceColumn := renderResource(schema.KindColumn, schema.Name{Schema: "cell", Name: "block_number", Parent: source.ID}, `{"type":"bigint","not_null":false,"ordinal":1}`, schema.Dependency{Target: source.ID, Type: schema.DependencyContains})
	view := renderResource(schema.KindMaterializedView, schema.Name{Schema: "cell", Name: "block_health_summary", Parent: ns.ID}, `{"definition":"SELECT block_number, count(*)::bigint AS sample_count FROM cell.block_health GROUP BY block_number"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains}, schema.Dependency{Target: source.ID, Type: schema.DependencyReferences})
	column := projection(view, "block_number", "integer")
	column.Spec = json.RawMessage(`{"type":"bigint","not_null":false,"ordinal":1}`)
	countColumn := projection(view, "sample_count", "bigint")
	countColumn.Spec = json.RawMessage(`{"type":"bigint","not_null":false,"ordinal":2}`)
	resources := []schema.Resource{ns, source, sourceColumn, view, column, countColumn}
	var firstIndex schema.Resource
	for number := 1; number <= 8; number++ {
		name := fmt.Sprintf("block_health_summary_idx_%d", number)
		index := renderResource(schema.KindIndex, schema.Name{Schema: "cell", Name: name, Parent: view.ID}, fmt.Sprintf(`{"definition":"CREATE INDEX %s ON cell.block_health_summary (block_number)","method":"btree","unique":false,"valid":true,"ready":true,"columns":["block_number"]}`, name), schema.Dependency{Target: view.ID, Type: schema.DependencyContains}, schema.Dependency{Target: column.ID, Type: schema.DependencyReferences})
		if firstIndex.ID == "" {
			firstIndex = index
		}
		resources = append(resources, index)
	}
	desired, err := New().Normalize(context.Background(), schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: resources}})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := New().Normalize(context.Background(), schema.Document{Version: schema.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	migration, err := plan.Build(context.Background(), New(), empty, desired, plan.Options{})
	if err != nil {
		t.Fatal(err)
	}
	viewPosition, indexCount := -1, 0
	for position, step := range migration.Steps {
		if strings.Contains(step.SQL, `CREATE MATERIALIZED VIEW "cell"."block_health_summary"`) {
			viewPosition = position
		}
		if strings.Contains(step.SQL, "CREATE INDEX block_health_summary_idx_") {
			indexCount++
			if viewPosition < 0 || position <= viewPosition {
				t.Fatalf("materialized-view index was not ordered after its parent: view=%d index=%d", viewPosition, position)
			}
		}
	}
	if viewPosition < 0 || indexCount != 8 {
		t.Fatalf("materialized-view plan view=%d indexes=%d steps=%+v", viewPosition, indexCount, migration.Steps)
	}

	regularView := renderResource(schema.KindView, schema.Name{Schema: "cell", Name: "ordinary_view", Parent: ns.ID}, `{"definition":"SELECT 1 AS block_number"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	badIndex := firstIndex
	badIndex.Name.Parent = regularView.ID
	if _, err := parseIndexDefinition(badIndex, map[string]schema.Resource{regularView.ID: regularView}); err == nil {
		t.Fatal("ordinary view was accepted as an index parent")
	}
	badConstraint := renderResource(schema.KindCheckConstraint, schema.Name{Schema: "cell", Name: "mv_check", Parent: view.ID}, `{"definition":"CHECK (block_number > 0)","deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: view.ID, Type: schema.DependencyContains})
	if _, err := parseConstraintDefinition(badConstraint, map[string]schema.Resource{view.ID: view}); err == nil {
		t.Fatal("materialized view was accepted as a table-constraint parent")
	}

	missingDependency := view
	missingDependency.Dependencies = missingDependency.Dependencies[:1]
	if err := validateSemanticDependencies(missingDependency, map[string]schema.Resource{ns.ID: ns, source.ID: source, view.ID: missingDependency}); err == nil {
		t.Fatal("complex materialized view with FROM but no captured relation dependency was accepted")
	}
	duplicateDependency := view
	duplicateDependency.Dependencies = append(duplicateDependency.Dependencies, schema.Dependency{Target: source.ID, Type: schema.DependencyReferences})
	if err := validateSemanticDependencies(duplicateDependency, map[string]schema.Resource{ns.ID: ns, source.ID: source, view.ID: duplicateDependency}); err == nil {
		t.Fatal("complex materialized view with duplicate captured dependencies was accepted")
	}
}

func TestConstraintIndexExpressionRoutinesAreExactAndImmutable(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "items", Parent: ns.ID}, `{}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	function := renderResource(schema.KindFunction, schema.Name{Schema: "app", Name: "is_valid(integer)", Parent: ns.ID}, `{"name":"is_valid","identity_arguments":"value integer","arguments":"value integer","result":"boolean","language":"sql","volatility":"i","security_definer":false,"definition":"CREATE FUNCTION app.is_valid(value integer) RETURNS boolean LANGUAGE sql IMMUTABLE AS $$ SELECT value > 0 $$"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	check := renderResource(schema.KindCheckConstraint, schema.Name{Schema: "app", Name: "items_valid_check", Parent: table.ID}, `{"definition":"CHECK (app.is_valid(value))","columns":[],"deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: function.ID, Type: schema.DependencyReferences})
	resources := map[string]schema.Resource{ns.ID: ns, table.ID: table, function.ID: function, check.ID: check}
	if err := validateSemanticDependencies(check, resources); err != nil {
		t.Fatal(err)
	}
	volatile := function
	volatile.Spec = json.RawMessage(strings.Replace(string(function.Spec), `"volatility":"i"`, `"volatility":"v"`, 1))
	resources[function.ID] = volatile
	if err := validateSemanticDependencies(check, resources); err == nil {
		t.Fatal("volatile check routine accepted")
	}
	undeclared := check
	undeclared.Spec = json.RawMessage(strings.Replace(string(check.Spec), "app.is_valid(value)", "app.missing(value)", 1))
	if err := validateSemanticDependencies(undeclared, resources); err == nil {
		t.Fatal("undeclared expression routine accepted")
	}
	subquery := check
	subquery.Spec = json.RawMessage(strings.Replace(string(check.Spec), "app.is_valid(value)", "EXISTS (SELECT 1)", 1))
	if err := validateSemanticDependencies(subquery, resources); err == nil {
		t.Fatal("subquery check accepted")
	}
}

func TestUnchangedOpaqueCheckDoesNotBlockUnrelatedTableChange(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	first := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "first", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	second := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "second", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	opaque := renderResource(schema.KindCheckConstraint, schema.Name{Schema: "app", Name: "first_future_check", Parent: first.ID}, `{"definition":"CHECK (future_extension_check(value))","deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: first.ID, Type: schema.DependencyContains})
	current := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, first, second, opaque}}}
	after := second
	after.Annotations = map[string]string{"comment": "unrelated change"}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, first, after, opaque}}}
	changes, _ := schema.Diff(current, desired, schema.DiffOptions{})
	statements, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired})
	if err != nil || len(statements) != 1 || !strings.Contains(statements[0].SQL, "COMMENT ON TABLE") {
		t.Fatalf("unrelated change statements=%+v err=%v", statements, err)
	}
}

func TestConstraintIndexCreateCommentsAndForeignKeyDependencies(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	users := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "users", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	orders := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "orders", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	usersPK := renderResource(schema.KindPrimaryKey, schema.Name{Schema: "app", Name: "users_pkey", Parent: users.ID}, `{"definition":"PRIMARY KEY (id)","deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: users.ID, Type: schema.DependencyContains})
	fk := renderResource(schema.KindForeignKey, schema.Name{Schema: "app", Name: "orders_user_fkey", Parent: orders.ID}, `{"definition":"FOREIGN KEY (user_id) REFERENCES app.users(id) ON UPDATE CASCADE ON DELETE RESTRICT","deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: orders.ID, Type: schema.DependencyContains}, schema.Dependency{Target: users.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: usersPK.ID, Type: schema.DependencyReferences})
	fk.Annotations = map[string]string{"comment": "tenant ownership"}
	index := renderResource(schema.KindIndex, schema.Name{Schema: "app", Name: "orders_user_idx", Parent: orders.ID}, `{"definition":"CREATE INDEX orders_user_idx ON app.orders USING btree (user_id)","method":"btree","unique":false,"valid":true,"ready":true}`, schema.Dependency{Target: orders.ID, Type: schema.DependencyContains})
	index.Annotations = map[string]string{"comment": "join support"}
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, users, orders, usersPK, fk, index}}}
	doc, err := New().Normalize(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	statements, err := RenderDocument(context.Background(), doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	var sql []string
	for _, statement := range statements {
		if statement.Kind == plugin.StatementExecutable {
			sql = append(sql, statement.SQL)
		}
	}
	joined := strings.Join(sql, "\n")
	for _, want := range []string{
		`ALTER TABLE "app"."orders" ADD CONSTRAINT "orders_user_fkey"`,
		`CREATE INDEX orders_user_idx ON app.orders`,
		`COMMENT ON CONSTRAINT "orders_user_fkey" ON "app"."orders" IS 'tenant ownership';`,
		`COMMENT ON INDEX "app"."orders_user_idx" IS 'join support';`,
	} {
		if !strings.Contains(joined, want) {
			t.Errorf("missing %q in:\n%s", want, joined)
		}
	}

	missingReference := fk
	missingReference.Dependencies = []schema.Dependency{{Target: orders.ID, Type: schema.DependencyContains}}
	bad := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, users, orders, usersPK, missingReference}}}
	bad, _ = New().Normalize(context.Background(), bad)
	if rendered, renderErr := RenderDocument(context.Background(), bad, nil); renderErr == nil || len(rendered) != 0 {
		t.Fatalf("missing FK dependency rendered=%+v err=%v", rendered, renderErr)
	}
}

func TestConstraintValidationUsesOnlineValidateCommand(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "orders", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	before := renderResource(schema.KindCheckConstraint, schema.Name{Schema: "app", Name: "orders_amount_check", Parent: table.ID}, `{"definition":"CHECK (amount >= 0) NOT VALID","deferrable":false,"initially_deferred":false,"validated":false}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	after := before
	after.Spec = json.RawMessage(`{"definition":"CHECK (amount >= 0)","deferrable":false,"initially_deferred":false,"validated":true}`)
	current := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, before}}}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, after}}}
	changes, err := schema.Diff(current, desired, schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	statements, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired})
	if err != nil {
		t.Fatal(err)
	}
	if len(statements) != 1 || statements[0].SQL != `ALTER TABLE "app"."orders" VALIDATE CONSTRAINT "orders_amount_check";` {
		t.Fatalf("statements=%+v", statements)
	}
}

func TestNullsNotDistinctRequiresVersionCapability(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "users", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	unique := renderResource(schema.KindUniqueConstraint, schema.Name{Schema: "app", Name: "users_email_key", Parent: table.ID}, `{"definition":"UNIQUE NULLS NOT DISTINCT (email)","deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, unique}}}
	for _, version := range []string{"", "14"} {
		if statements, err := RenderDocument(context.Background(), doc, map[string]string{"postgres_version": version}); err == nil || len(statements) != 0 {
			t.Fatalf("version %q statements=%+v err=%v", version, statements, err)
		}
	}
	if statements, err := RenderDocument(context.Background(), doc, map[string]string{"postgres_version": "15"}); err != nil || len(statements) == 0 {
		t.Fatalf("PostgreSQL 15 statements=%+v err=%v", statements, err)
	}
}

func TestIndexPreflightDetectsInvalidRemnantsAndUnavailableAccessMethods(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "items", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	invalid := renderResource(schema.KindIndex, schema.Name{Schema: "app", Name: "items_invalid_idx", Parent: table.ID}, `{"definition":"CREATE INDEX items_invalid_idx ON app.items (id)","method":"btree","unique":false,"valid":false,"ready":false,"columns":[]}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, invalid}}}
	report, err := PreflightProvisioning(context.Background(), doc, nil)
	if err != nil || report.Supported || len(report.Diagnostics) == 0 {
		t.Fatalf("invalid remnant preflight=%+v err=%v", report, err)
	}
	custom := invalid
	custom.Name.Name = "items_vector_idx"
	custom.ID = schema.StableID(custom.Kind, custom.Name)
	custom.Spec = json.RawMessage(`{"definition":"CREATE INDEX items_vector_idx ON app.items USING hnsw (id)","method":"hnsw","unique":false,"valid":true,"ready":true,"columns":[]}`)
	doc.Graph.Resources[2] = custom
	if report, err = PreflightProvisioning(context.Background(), doc, nil); err != nil || report.Supported {
		t.Fatalf("unavailable access method preflight=%+v err=%v", report, err)
	}
	if report, err = PreflightProvisioning(context.Background(), doc, map[string]string{"available_index_access_methods": "hnsw"}); err != nil || !report.Supported {
		t.Fatalf("declared access method preflight=%+v err=%v", report, err)
	}
}

func TestBuiltinInetOperatorClassIsMethodAndTypeBound(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "global"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "global", Name: "ipam_allocations", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	column := renderResource(schema.KindColumn, schema.Name{Schema: "global", Name: "cidr", Parent: table.ID}, `{"type":"cidr","not_null":false,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	index := renderResource(schema.KindIndex, schema.Name{Schema: "global", Name: "idx_ipam_allocations_cidr", Parent: table.ID}, `{"definition":"CREATE INDEX idx_ipam_allocations_cidr ON global.ipam_allocations USING gist (cidr inet_ops)","method":"gist","unique":false,"valid":true,"ready":true,"columns":["cidr"]}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: column.ID, Type: schema.DependencyReferences})
	for _, method := range []string{"btree", "hash", "gist", "spgist"} {
		candidate := index
		candidate.Name.Name = "idx_ipam_allocations_cidr_" + method
		candidate.ID = schema.StableID(candidate.Kind, candidate.Name)
		candidate.Spec = json.RawMessage(fmt.Sprintf(`{"definition":"CREATE INDEX %s ON global.ipam_allocations USING %s (cidr inet_ops)","method":"%s","unique":false,"valid":true,"ready":true,"columns":["cidr"]}`, candidate.Name.Name, method, method))
		resources := []schema.Resource{ns, table, column, candidate}
		if statements, err := RenderDocument(context.Background(), schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: resources}}, nil); err != nil || len(statements) == 0 {
			t.Fatalf("%s inet_ops statements=%+v err=%v", method, statements, err)
		}
	}
	qualified := index
	qualified.Spec = json.RawMessage(strings.Replace(string(index.Spec), "cidr inet_ops", "cidr pg_catalog.inet_ops", 1))
	if statements, err := RenderDocument(context.Background(), schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, column, qualified}}}, nil); err != nil || len(statements) == 0 {
		t.Fatalf("pg_catalog.inet_ops statements=%+v err=%v", statements, err)
	}
	for name, mutate := range map[string]func(*schema.Resource, *schema.Resource){
		"GIN method": func(candidate, _ *schema.Resource) {
			candidate.Spec = json.RawMessage(strings.Replace(strings.Replace(string(candidate.Spec), "USING gist", "USING gin", 1), `"method":"gist"`, `"method":"gin"`, 1))
		},
		"text column": func(_ *schema.Resource, candidateColumn *schema.Resource) {
			candidateColumn.Spec = json.RawMessage(`{"type":"text","not_null":false,"ordinal":1}`)
		},
		"custom qualification": func(candidate, _ *schema.Resource) {
			candidate.Spec = json.RawMessage(strings.Replace(string(candidate.Spec), "cidr inet_ops", "cidr public.inet_ops", 1))
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate, candidateColumn := index, column
			mutate(&candidate, &candidateColumn)
			if statements, err := RenderDocument(context.Background(), schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, candidateColumn, candidate}}}, nil); err == nil || len(statements) != 0 {
				t.Fatalf("statements=%+v err=%v", statements, err)
			}
		})
	}
}

func TestConstraintIndexPlanOrderingAndPhaseIdentityAreStable(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	accounts := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "accounts", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	accountID := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "id", Parent: accounts.ID}, `{"type":"bigint","not_null":true,"ordinal":1}`, schema.Dependency{Target: accounts.ID, Type: schema.DependencyContains})
	accountsPK := renderResource(schema.KindPrimaryKey, schema.Name{Schema: "app", Name: "accounts_pkey", Parent: accounts.ID}, `{"definition":"PRIMARY KEY (id)","columns":["id"],"deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: accounts.ID, Type: schema.DependencyContains}, schema.Dependency{Target: accountID.ID, Type: schema.DependencyReferences})
	orders := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "orders", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	orderID := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "id", Parent: orders.ID}, `{"type":"bigint","not_null":true,"ordinal":1}`, schema.Dependency{Target: orders.ID, Type: schema.DependencyContains})
	orderAccount := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "account_id", Parent: orders.ID}, `{"type":"bigint","not_null":true,"ordinal":2}`, schema.Dependency{Target: orders.ID, Type: schema.DependencyContains})
	ordersPK := renderResource(schema.KindPrimaryKey, schema.Name{Schema: "app", Name: "orders_pkey", Parent: orders.ID}, `{"definition":"PRIMARY KEY (id)","columns":["id"],"deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: orders.ID, Type: schema.DependencyContains}, schema.Dependency{Target: orderID.ID, Type: schema.DependencyReferences})
	fk := renderResource(schema.KindForeignKey, schema.Name{Schema: "app", Name: "orders_account_fkey", Parent: orders.ID}, `{"definition":"FOREIGN KEY (account_id) REFERENCES app.accounts(id) NOT VALID","columns":["account_id"],"referenced_columns":["id"],"deferrable":false,"initially_deferred":false,"validated":false}`, schema.Dependency{Target: orders.ID, Type: schema.DependencyContains}, schema.Dependency{Target: orderAccount.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: accounts.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: accountID.ID, Type: schema.DependencyReferences}, schema.Dependency{Target: accountsPK.ID, Type: schema.DependencyReferences})
	index := renderResource(schema.KindIndex, schema.Name{Schema: "app", Name: "orders_account_idx", Parent: orders.ID}, `{"definition":"CREATE INDEX orders_account_idx ON app.orders (account_id)","method":"btree","unique":false,"valid":true,"ready":true,"columns":["account_id"]}`, schema.Dependency{Target: orders.ID, Type: schema.DependencyContains}, schema.Dependency{Target: orderAccount.ID, Type: schema.DependencyReferences})
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, accounts, accountID, accountsPK, orders, orderID, orderAccount, ordersPK, fk, index}}}
	desired, err := New().Normalize(context.Background(), desired)
	if err != nil {
		t.Fatal(err)
	}
	empty, err := New().Normalize(context.Background(), schema.Document{Version: schema.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	options := plan.Options{Render: map[string]string{"concurrent_indexes": "true"}}
	first, err := plan.Build(context.Background(), New(), empty, desired, options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := plan.Build(context.Background(), New(), empty, desired, options)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := first.MarshalCanonical()
	b, _ := second.MarshalCanonical()
	if string(a) != string(b) {
		t.Fatal("repeated constraint/index plans are not byte-stable")
	}
	positions := map[string]int{}
	for index, step := range first.Steps {
		for _, needle := range []string{"ADD COLUMN \"account_id\"", "ADD CONSTRAINT \"accounts_pkey\"", "ADD CONSTRAINT \"orders_account_fkey\"", "CREATE INDEX CONCURRENTLY"} {
			if strings.Contains(step.SQL, needle) {
				positions[needle] = index
			}
		}
	}
	if positions["ADD COLUMN \"account_id\""] >= positions["ADD CONSTRAINT \"orders_account_fkey\""] || positions["ADD CONSTRAINT \"accounts_pkey\""] >= positions["ADD CONSTRAINT \"orders_account_fkey\""] {
		t.Fatalf("unsafe constraint ordering: %v", positions)
	}
	hasProhibited := false
	for _, step := range first.Steps {
		hasProhibited = hasProhibited || step.Transaction == plan.TransactionProhibited
	}
	if !hasProhibited {
		t.Fatalf("concurrent phase missing: %+v", first.Phases)
	}
}

func TestIndexPredicateTypeDependencyIsDerivedAndOrdered(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "global"}, `{}`)
	cellType := renderResource(schema.KindEnum, schema.Name{Schema: "global", Name: "cell_type", Parent: ns.ID}, `{"values":["shared","dedicated"]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	table := renderResource(schema.KindTable, schema.Name{Schema: "global", Name: "cells", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	column := renderResource(schema.KindColumn, schema.Name{Schema: "global", Name: "type", Parent: table.ID}, `{"type":"global.cell_type","not_null":true,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: cellType.ID, Type: schema.DependencyUses})
	index := renderResource(schema.KindIndex, schema.Name{Schema: "global", Name: "idx_cells_available_shared", Parent: table.ID}, `{"definition":"CREATE INDEX idx_cells_available_shared ON global.cells (type) WHERE (type = 'shared'::cell_type)","method":"btree","unique":false,"valid":true,"ready":true,"columns":["type"]}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: column.ID, Type: schema.DependencyReferences})
	desired, err := New().Normalize(context.Background(), schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, cellType, table, column, index}}})
	if err != nil {
		t.Fatal(err)
	}
	var normalizedIndex schema.Resource
	for _, resource := range desired.Graph.Resources {
		if resource.ID == index.ID {
			normalizedIndex = resource
		}
	}
	foundTypeDependency := false
	for _, dependency := range normalizedIndex.Dependencies {
		foundTypeDependency = foundTypeDependency || dependency.Target == cellType.ID && dependency.Type == schema.DependencyUses
	}
	if !foundTypeDependency {
		t.Fatalf("index predicate dependencies=%+v", normalizedIndex.Dependencies)
	}
	hcl, err := source.FormatHCL(desired)
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err := source.LoadContext(context.Background(), source.Input{URI: "index-predicate.hcl", Format: source.FormatHCLSource, Data: hcl})
	if err != nil {
		t.Fatal(err)
	}
	roundTrip, err = New().Normalize(context.Background(), roundTrip)
	if err != nil {
		t.Fatal(err)
	}
	foundTypeDependency = false
	for _, resource := range roundTrip.Graph.Resources {
		if resource.ID == index.ID {
			for _, dependency := range resource.Dependencies {
				foundTypeDependency = foundTypeDependency || dependency.Target == cellType.ID && dependency.Type == schema.DependencyUses
			}
		}
	}
	if !foundTypeDependency {
		t.Fatal("index predicate type dependency was lost through HCL")
	}
	otherType := renderResource(schema.KindEnum, schema.Name{Schema: "global", Name: "other_type", Parent: ns.ID}, `{"values":["shared"]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	badIndex := index
	badIndex.Dependencies = append(badIndex.Dependencies, schema.Dependency{Target: otherType.ID, Type: schema.DependencyUses})
	bad, err := New().Normalize(context.Background(), schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, cellType, otherType, table, column, badIndex}}})
	if err != nil {
		t.Fatal(err)
	}
	if statements, renderErr := RenderDocument(context.Background(), bad, nil); renderErr == nil || len(statements) != 0 {
		t.Fatalf("mismatched index type dependency statements=%+v err=%v", statements, renderErr)
	}
	empty, err := New().Normalize(context.Background(), schema.Document{Version: schema.SchemaVersion})
	if err != nil {
		t.Fatal(err)
	}
	for _, concurrent := range []bool{false, true} {
		options := map[string]string{}
		if concurrent {
			options["concurrent_indexes"] = "true"
		}
		built, buildErr := plan.Build(context.Background(), New(), empty, desired, plan.Options{Render: options})
		if buildErr != nil {
			t.Fatalf("concurrent=%v: %v", concurrent, buildErr)
		}
		typePosition, indexPosition := -1, -1
		for position, step := range built.Steps {
			if strings.Contains(step.SQL, `CREATE TYPE "global"."cell_type"`) {
				typePosition = position
			}
			if strings.Contains(step.SQL, "idx_cells_available_shared") && strings.Contains(step.SQL, "CREATE INDEX") {
				indexPosition = position
			}
		}
		if typePosition < 0 || indexPosition < 0 || typePosition >= indexPosition {
			t.Fatalf("concurrent=%v type=%d index=%d steps=%+v", concurrent, typePosition, indexPosition, built.Steps)
		}
	}
}

func TestConstraintAndIndexRenameRebuildDropLifecycle(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "users", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	resources := map[string]schema.Resource{ns.ID: ns, table.ID: table}
	for _, before := range []schema.Resource{
		renderResource(schema.KindPrimaryKey, schema.Name{Schema: "app", Name: "users_pkey", Parent: table.ID}, `{"definition":"PRIMARY KEY (id)","deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}),
		renderResource(schema.KindUniqueConstraint, schema.Name{Schema: "app", Name: "users_email_key", Parent: table.ID}, `{"definition":"UNIQUE (email)","deferrable":false,"initially_deferred":false,"validated":true}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}),
		renderResource(schema.KindIndex, schema.Name{Schema: "app", Name: "users_email_idx", Parent: table.ID}, `{"definition":"CREATE INDEX users_email_idx ON app.users (email)","method":"btree","unique":false,"valid":true,"ready":true}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}),
	} {
		resources[before.ID] = before
		after := before
		after.Name.Name += "_v2"
		after.ID = schema.StableID(after.Kind, after.Name)
		rename, err := renderRename(before, after, resources)
		if err != nil || len(rename) != 1 || !strings.Contains(rename[0], "RENAME") {
			t.Fatalf("%s rename=%v err=%v", before.Kind, rename, err)
		}
		drop, err := renderDrop(before, resources, nil)
		if err != nil || len(drop) != 1 || !strings.Contains(drop[0], "DROP") {
			t.Fatalf("%s drop=%v err=%v", before.Kind, drop, err)
		}
		if before.Kind == schema.KindIndex {
			after = before
			after.Spec = json.RawMessage(`{"definition":"CREATE INDEX users_email_idx ON app.users (email DESC)","method":"btree","unique":false,"valid":true,"ready":true}`)
		} else {
			after = before
			after.Spec = json.RawMessage(strings.Replace(string(before.Spec), ")\"", ", tenant_id)\"", 1))
		}
		rebuild, err := renderAlter(before, after, resources, map[string]string{"allow_rebuild": "true"})
		if err != nil || len(rebuild) != 2 {
			t.Fatalf("%s rebuild=%v err=%v", before.Kind, rebuild, err)
		}
	}
}

func FuzzIndexDefinitionFailsWithoutPartialSQL(f *testing.F) {
	for _, seed := range []string{"(id)", "(lower(id)) WHERE id IS NOT NULL", "(id); DROP TABLE app.items", "", "USING gin (id)"} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, definition string) {
		ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
		table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "items", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
		column := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "id", Parent: table.ID}, `{"type":"text","not_null":false,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
		raw, _ := json.Marshal(map[string]any{"definition": definition, "columns": []string{}, "unique": false, "valid": true, "ready": true})
		index := schema.Resource{Kind: schema.KindIndex, Name: schema.Name{Schema: "app", Name: "items_idx", Parent: table.ID}, Spec: raw, Dependencies: []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}}
		index.ID = schema.StableID(index.Kind, index.Name)
		doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, column, index}}}
		statements, err := RenderDocument(context.Background(), doc, nil)
		if err != nil && len(statements) != 0 {
			t.Fatalf("partial SQL escaped fail-closed render: %+v", statements)
		}
		if err == nil {
			for _, statement := range statements {
				if statement.Kind == plugin.StatementExecutable {
					parsed, parseErr := pg_query.Parse(statement.SQL)
					if parseErr != nil || len(parsed.Stmts) != 1 {
						t.Fatalf("rendered non-single statement %q: %v", statement.SQL, parseErr)
					}
				}
			}
		}
	})
}
