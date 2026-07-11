package postgres

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"testing"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
)

func renderResource(kind schema.Kind, name schema.Name, spec string, deps ...schema.Dependency) schema.Resource {
	r := schema.Resource{Kind: kind, Name: name, Spec: json.RawMessage(spec), Dependencies: deps}
	r.ID = schema.StableID(kind, name)
	return r
}
func projection(parent schema.Resource, name, typ string) schema.Resource {
	return renderResource(schema.KindColumn, schema.Name{Schema: parent.Name.Schema, Name: name, Parent: parent.ID}, `{"not_null":false,"ordinal":1,"type":"`+typ+`"}`, schema.Dependency{Target: parent.ID, Type: schema.DependencyContains})
}

func TestRenderDocumentQuotesAndOrders(t *testing.T) {
	s := renderResource(schema.KindSchema, schema.Name{Name: `Odd"Schema`}, `{}`)
	view := renderResource(schema.KindView, schema.Name{Schema: s.Name.Name, Name: `a"b`, Parent: s.ID}, `{"definition":"SELECT 1 AS value"}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	column := projection(view, "value", "integer")
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{column, view, s}}}
	out, err := RenderDocument(context.Background(), doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{`CREATE SCHEMA "Odd""Schema";`, `CREATE VIEW "Odd""Schema"."a""b" AS SELECT 1 AS value;`}
	var executable []plugin.Statement
	for _, statement := range out {
		if statement.Kind != plugin.StatementTopology {
			executable = append(executable, statement)
		}
	}
	if len(executable) != len(want) {
		t.Fatalf("out=%+v", out)
	}
	for i := range want {
		if executable[i].SQL != want[i] {
			t.Errorf("%d got %q want %q", i, executable[i].SQL, want[i])
		}
	}
}

func TestRenderDocumentGolden(t *testing.T) {
	s := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	view := renderResource(schema.KindView, schema.Name{Schema: "app", Name: "widgets", Parent: s.ID}, `{"definition":"SELECT 1 AS value"}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	column := projection(view, "value", "integer")
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{column, view, s}}}
	out, err := RenderDocument(context.Background(), doc, nil)
	if err != nil {
		t.Fatal(err)
	}
	var lines []string
	for i := range out {
		if out[i].Kind != plugin.StatementTopology {
			lines = append(lines, out[i].SQL)
		}
	}
	actual := strings.Join(lines, "\n") + "\n"
	expected, err := os.ReadFile("testdata/render_document.golden.sql")
	if err != nil {
		t.Fatal(err)
	}
	if actual != string(expected) {
		t.Fatalf("golden mismatch\nactual:\n%s\nexpected:\n%s", actual, expected)
	}
}

func TestConcurrentRenderingRejectedWithoutGuardedPhaseExecutor(t *testing.T) {
	table := renderResource(schema.KindTable, schema.Name{Schema: "public", Name: "users"}, `{}`)
	idx := renderResource(schema.KindIndex, schema.Name{Schema: "public", Name: "users_email_idx", Parent: table.ID}, `{"definition":"(email)"}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{table, idx}}}
	changes, _ := schema.Diff(empty, desired, schema.DiffOptions{})
	out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: empty, Desired: desired, Options: map[string]string{"concurrent_indexes": "true"}})
	if err == nil || len(out) != 0 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestMaterializedViewAlterRequiresExplicitRebuild(t *testing.T) {
	s := renderResource(schema.KindSchema, schema.Name{Name: "public"}, `{}`)
	before := renderResource(schema.KindMaterializedView, schema.Name{Schema: "public", Name: "users_mv", Parent: s.ID}, `{"definition":"SELECT 1 AS value"}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	after := before
	after.Spec = json.RawMessage(`{"definition":"SELECT 2 AS value"}`)
	column := projection(before, "value", "integer")
	current := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{s, before, column}}}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{s, after, column}}}
	changes, _ := schema.Diff(current, desired, schema.DiffOptions{})
	out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired})
	if err == nil || len(out) != 0 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
	out, err = New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired, Options: map[string]string{"allow_rebuild": "true"}})
	if err != nil || len(out) != 2 {
		t.Fatalf("out=%+v err=%v", out, err)
	}
}

func TestCreateHelpersRemainDeterministicForInspectedKinds(t *testing.T) {
	s := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "users", Parent: s.ID}, `{}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	resources := map[string]schema.Resource{s.ID: s, table.ID: table}
	cases := []schema.Resource{
		s,
		renderResource(schema.KindExtension, schema.Name{Schema: "app", Name: "hstore", Parent: s.ID}, `{"version":"1.8"}`),
		renderResource(schema.KindEnum, schema.Name{Schema: "app", Name: "status", Parent: s.ID}, `{"values":["new"]}`),
		renderResource(schema.KindDomain, schema.Name{Schema: "app", Name: "positive", Parent: s.ID}, `{"base_type":"integer","not_null":true}`),
		renderResource(schema.KindComposite, schema.Name{Schema: "app", Name: "address", Parent: s.ID}, `{"attributes":[{"name":"zip","type":"integer"}]}`),
		renderResource(schema.KindSequence, schema.Name{Schema: "app", Name: "seq", Parent: s.ID}, `{"start":1,"increment":1}`),
		table,
		renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "id", Parent: table.ID}, `{"type":"bigint"}`),
		renderResource(schema.KindPrimaryKey, schema.Name{Schema: "app", Name: "users_pkey", Parent: table.ID}, `{"definition":"PRIMARY KEY (id)"}`),
		renderResource(schema.KindUniqueConstraint, schema.Name{Schema: "app", Name: "users_id_key", Parent: table.ID}, `{"definition":"UNIQUE (id)"}`),
		renderResource(schema.KindCheckConstraint, schema.Name{Schema: "app", Name: "users_id_check", Parent: table.ID}, `{"definition":"CHECK (id > 0)"}`),
		renderResource(schema.KindForeignKey, schema.Name{Schema: "app", Name: "users_parent_fkey", Parent: table.ID}, `{"definition":"FOREIGN KEY (id) REFERENCES app.users(id)"}`),
		renderResource(schema.KindIndex, schema.Name{Schema: "app", Name: "users_idx", Parent: table.ID}, `{"definition":"(id)"}`),
		renderResource(schema.KindView, schema.Name{Schema: "app", Name: "user_view", Parent: s.ID}, `{"definition":"SELECT 1 AS value"}`),
		renderResource(schema.KindMaterializedView, schema.Name{Schema: "app", Name: "user_mv", Parent: s.ID}, `{"definition":"SELECT 1 AS value"}`),
	}
	for _, r := range cases {
		resources[r.ID] = r
		if r.Kind == schema.KindView || r.Kind == schema.KindMaterializedView {
			column := projection(r, "value", "integer")
			resources[column.ID] = column
		}
	}
	for _, r := range cases {
		if _, err := renderCreate(r, resources, nil); err != nil {
			t.Errorf("create %s: %v", r.Kind, err)
		}
	}
}

func TestNativeViewFragmentsRejectInjection(t *testing.T) {
	for _, fragment := range []string{"SELECT 1; DROP TABLE users", "SELECT 1 -- hidden", "SELECT /* hidden */ 1", "SELECT $tag$payload$tag$"} {
		t.Run(fragment, func(t *testing.T) {
			r := renderResource(schema.KindView, schema.Name{Schema: "public", Name: "v"}, `{}`)
			raw, _ := json.Marshal(map[string]string{"definition": fragment})
			r.Spec = raw
			changes := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "c", Operation: schema.OperationCreate, ResourceID: r.ID, After: &r}}}
			out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes})
			if err == nil || len(out) != 0 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
		})
	}
}

func TestManagedMetadataAndIdentifierControlsFailClosed(t *testing.T) {
	cases := []schema.Resource{}
	s := renderResource(schema.KindSchema, schema.Name{Name: "app\nDROP"}, `{}`)
	cases = append(cases, s)
	catalog := renderResource(schema.KindSchema, schema.Name{Catalog: "other", Name: "app"}, `{}`)
	cases = append(cases, catalog)
	commented := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	commented.Annotations = map[string]string{"comment": "not rendered"}
	cases = append(cases, commented)
	extra := renderResource(schema.KindSchema, schema.Name{Name: "extra"}, `{}`)
	extra.Extra = map[string]json.RawMessage{"future": json.RawMessage(`true`)}
	cases = append(cases, extra)
	nameExtra := renderResource(schema.KindSchema, schema.Name{Name: "name_extra"}, `{}`)
	nameExtra.Name.Extra = map[string]json.RawMessage{"future": json.RawMessage(`true`)}
	cases = append(cases, nameExtra)
	for _, persistence := range []string{"t", "u"} {
		cases = append(cases, renderResource(schema.KindTable, schema.Name{Schema: "public", Name: "t_" + persistence}, `{"partitioned":false,"persistence":"`+persistence+`","row_security":false,"force_row_security":false}`))
	}
	wrongType := renderResource(schema.KindTable, schema.Name{Schema: "public", Name: "wrong_type"}, `{"partitioned":"false","persistence":1,"row_security":false,"force_row_security":false}`)
	cases = append(cases, wrongType)
	cases = append(cases, renderResource(schema.KindTable, schema.Name{Schema: "public", Name: "partitioned"}, `{"partitioned":true,"persistence":"p","row_security":false,"force_row_security":false}`))
	for _, r := range cases {
		change := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "c", Operation: schema.OperationCreate, ResourceID: r.ID, After: &r}}}
		out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: change})
		if err == nil || len(out) != 0 {
			t.Fatalf("resource=%+v out=%+v err=%v", r, out, err)
		}
	}
}

func TestManagedResourcesRequireCanonicalIdentityAndDependencies(t *testing.T) {
	app := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	other := renderResource(schema.KindSchema, schema.Name{Name: "other"}, `{}`)
	base := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "widgets", Parent: app.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: app.ID, Type: schema.DependencyContains})
	cases := map[string]schema.Resource{}
	wrongParent := base
	wrongParent.Name.Parent = other.ID
	cases["schema parent mismatch"] = wrongParent
	missingContains := base
	missingContains.Dependencies = nil
	cases["missing contains"] = missingContains
	extraDependency := base
	extraDependency.Dependencies = append(extraDependency.Dependencies, schema.Dependency{Target: other.ID, Type: schema.DependencyReferences})
	cases["ignored reference"] = extraDependency
	for name, table := range cases {
		t.Run(name, func(t *testing.T) {
			doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{app, other, table}}}
			changes := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "c", Operation: schema.OperationCreate, ResourceID: table.ID, After: &table}}}
			out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Desired: doc})
			if err == nil || len(out) != 0 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
		})
	}
	schemaWithDependency := app
	schemaWithDependency.Dependencies = []schema.Dependency{{Target: other.ID, Type: schema.DependencyReferences}}
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{other, schemaWithDependency}}}
	changes := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "c", Operation: schema.OperationCreate, ResourceID: app.ID, After: &schemaWithDependency}}}
	out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Desired: doc})
	if err == nil || len(out) != 0 {
		t.Fatalf("schema dependency out=%+v err=%v", out, err)
	}
}

func TestRenderedDependenciesMustExactlyMatchSemantics(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "widgets", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	unrelated := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "other", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	tableColumn := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "id", Parent: table.ID}, `{"type":"integer","not_null":false,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	base := renderResource(schema.KindView, schema.Name{Schema: "app", Name: "v", Parent: ns.ID}, `{"definition":"SELECT id FROM app.widgets"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains}, schema.Dependency{Target: table.ID, Type: schema.DependencyReferences})
	for name, mutate := range map[string]func(*schema.Resource){
		"missing actual reference": func(view *schema.Resource) { view.Dependencies = view.Dependencies[:1] },
		"extra unrelated reference": func(view *schema.Resource) {
			view.Dependencies = append(view.Dependencies, schema.Dependency{Target: unrelated.ID, Type: schema.DependencyReferences})
		},
	} {
		t.Run(name, func(t *testing.T) {
			view := base
			view.Dependencies = append([]schema.Dependency(nil), base.Dependencies...)
			mutate(&view)
			column := projection(view, "id", "integer")
			doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, tableColumn, unrelated, view, column}}}
			changes := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "c", Operation: schema.OperationCreate, ResourceID: view.ID, After: &view}}}
			out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Desired: doc})
			if err == nil || len(out) != 0 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
		})
	}
	status := renderResource(schema.KindEnum, schema.Name{Schema: "app", Name: "status", Parent: ns.ID}, `{"values":["new"]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	otherType := renderResource(schema.KindEnum, schema.Name{Schema: "app", Name: "other_status", Parent: ns.ID}, `{"values":["new"]}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	baseColumn := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "status", Parent: table.ID}, `{"type":"app.status","not_null":false,"ordinal":2}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains}, schema.Dependency{Target: status.ID, Type: schema.DependencyUses})
	for name, mutate := range map[string]func(*schema.Resource){
		"missing type use": func(column *schema.Resource) { column.Dependencies = column.Dependencies[:1] },
		"extra type use": func(column *schema.Resource) {
			column.Dependencies = append(column.Dependencies, schema.Dependency{Target: otherType.ID, Type: schema.DependencyUses})
		},
	} {
		t.Run(name, func(t *testing.T) {
			column := baseColumn
			column.Dependencies = append([]schema.Dependency(nil), baseColumn.Dependencies...)
			mutate(&column)
			doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, tableColumn, status, otherType, column}}}
			changes := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{{ID: "c", Operation: schema.OperationCreate, ResourceID: column.ID, After: &column}}}
			out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Desired: doc})
			if err == nil || len(out) != 0 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
		})
	}
}

func TestColumnTypeAlterAllowsOnlyKnownSafeCasts(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "widgets", Parent: ns.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	before := renderResource(schema.KindColumn, schema.Name{Schema: "app", Name: "value", Parent: table.ID}, `{"type":"text","not_null":false,"ordinal":1}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	for name, typ := range map[string]string{"unsafe text integer": "integer", "safe text varchar is assignment-direction unsafe": "character varying"} {
		t.Run(name, func(t *testing.T) {
			after := before
			after.Spec = json.RawMessage(`{"type":"` + typ + `","not_null":false,"ordinal":1}`)
			current := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, before}}}
			desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, table, after}}}
			changes, _ := schema.Diff(current, desired, schema.DiffOptions{})
			out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired})
			if err == nil || len(out) != 0 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
		})
	}
}

func TestViewAlterOutputShapePolicy(t *testing.T) {
	s := renderResource(schema.KindSchema, schema.Name{Name: "public"}, `{}`)
	view := renderResource(schema.KindView, schema.Name{Schema: "public", Name: "v", Parent: s.ID}, `{"definition":"SELECT 1 AS value"}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	column := projection(view, "value", "integer")
	current := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{s, view, column}}}
	same := view
	same.Spec = json.RawMessage(`{"definition":"SELECT 2 AS value"}`)
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{s, same, column}}}
	changes, _ := schema.Diff(current, desired, schema.DiffOptions{})
	out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired})
	if err != nil || len(out) != 1 {
		t.Fatalf("same shape out=%+v err=%v", out, err)
	}
	for name, mutate := range map[string]func(*schema.Resource, *schema.Resource){"name": func(v, c *schema.Resource) {
		v.Spec = json.RawMessage(`{"definition":"SELECT 2 AS other"}`)
		c.Name.Name = "other"
		c.ID = schema.StableID(c.Kind, c.Name)
	}, "type": func(v, c *schema.Resource) {
		v.Spec = json.RawMessage(`{"definition":"SELECT 'x'::text AS value"}`)
		c.Spec = json.RawMessage(`{"not_null":false,"ordinal":1,"type":"text"}`)
	}} {
		t.Run(name, func(t *testing.T) {
			v, c := view, column
			mutate(&v, &c)
			target := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{s, v, c}}}
			cs, _ := schema.Diff(current, target, schema.DiffOptions{})
			result, e := New().Render(context.Background(), plugin.RenderRequest{Changes: cs, Current: current, Desired: target})
			if e == nil || len(result) != 0 {
				t.Fatalf("out=%+v err=%v", result, e)
			}
		})
	}
}

func TestIndependentOrMalformedProjectionChangesFailClosed(t *testing.T) {
	ns := renderResource(schema.KindSchema, schema.Name{Name: "public"}, `{}`)
	view := renderResource(schema.KindView, schema.Name{Schema: "public", Name: "v", Parent: ns.ID}, `{"definition":"SELECT 1 AS value"}`, schema.Dependency{Target: ns.ID, Type: schema.DependencyContains})
	column := projection(view, "value", "integer")
	current := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, view}}}
	desired := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{ns, view, column}}}
	changes, _ := schema.Diff(current, desired, schema.DiffOptions{})
	out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired})
	if err == nil || len(out) != 0 {
		t.Fatalf("independent out=%+v err=%v", out, err)
	}
	bad := column
	bad.Spec = json.RawMessage(`{"future":true,"not_null":false,"ordinal":1,"type":"integer"}`)
	desired.Graph.Resources = []schema.Resource{ns, view, bad}
	changes, _ = schema.Diff(current, desired, schema.DiffOptions{})
	out, err = New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired})
	if err == nil || len(out) != 0 {
		t.Fatalf("malformed out=%+v err=%v", out, err)
	}
	bad = column
	bad.Dependencies = []schema.Dependency{{Target: view.ID, Type: schema.DependencyReferences}}
	desired.Graph.Resources = []schema.Resource{ns, view, bad}
	changes, _ = schema.Diff(current, desired, schema.DiffOptions{})
	out, err = New().Render(context.Background(), plugin.RenderRequest{Changes: changes, Current: current, Desired: desired})
	if err == nil || len(out) != 0 {
		t.Fatalf("bad dependency out=%+v err=%v", out, err)
	}
}

func TestManagedLifecycleMatrix(t *testing.T) {
	s := renderResource(schema.KindSchema, schema.Name{Name: "app"}, `{}`)
	s2 := renderResource(schema.KindSchema, schema.Name{Name: "app2"}, `{}`)
	table := renderResource(schema.KindTable, schema.Name{Schema: "app", Name: "users", Parent: s.ID}, `{"partitioned":false,"persistence":"p","row_security":false,"force_row_security":false}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	table2 := table
	table2.Spec = json.RawMessage(`{"partitioned":false,"persistence":"p","row_security":true,"force_row_security":false}`)
	tableRenamed := table
	tableRenamed.Name.Name = "users2"
	tableRenamed.ID = schema.StableID(tableRenamed.Kind, tableRenamed.Name)
	view := renderResource(schema.KindView, schema.Name{Schema: "app", Name: "v", Parent: s.ID}, `{"definition":"SELECT 1 AS value"}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	view2 := view
	view2.Spec = json.RawMessage(`{"definition":"SELECT 2 AS value"}`)
	viewRenamed := view
	viewRenamed.Name.Name = "v2"
	viewRenamed.ID = schema.StableID(viewRenamed.Kind, viewRenamed.Name)
	mv := renderResource(schema.KindMaterializedView, schema.Name{Schema: "app", Name: "mv", Parent: s.ID}, `{"definition":"SELECT 1 AS value"}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	mv2 := mv
	mv2.Spec = json.RawMessage(`{"definition":"SELECT 2 AS value"}`)
	mvRenamed := mv
	mvRenamed.Name.Name = "mv2"
	mvRenamed.ID = schema.StableID(mvRenamed.Kind, mvRenamed.Name)
	vcol := projection(view, "value", "integer")
	mvcol := projection(mv, "value", "integer")
	vcolRenamed := projection(viewRenamed, "value", "integer")
	mvcolRenamed := projection(mvRenamed, "value", "integer")
	resources := map[string]schema.Resource{s.ID: s, s2.ID: s2, table.ID: table, tableRenamed.ID: tableRenamed, view.ID: view, viewRenamed.ID: viewRenamed, mv.ID: mv, mvRenamed.ID: mvRenamed, vcol.ID: vcol, mvcol.ID: mvcol, vcolRenamed.ID: vcolRenamed, mvcolRenamed.ID: mvcolRenamed}
	tests := []struct {
		name    string
		change  schema.Change
		options map[string]string
	}{{"schema create", schema.Change{ID: "c1", Operation: schema.OperationCreate, ResourceID: s.ID, After: &s}, nil}, {"schema drop", schema.Change{ID: "c2", Operation: schema.OperationDrop, ResourceID: s.ID, Before: &s}, nil}, {"schema rename", schema.Change{ID: "c3", Operation: schema.OperationRename, ResourceID: s2.ID, Before: &s, After: &s2}, nil}, {"table create", schema.Change{ID: "c4", Operation: schema.OperationCreate, ResourceID: table.ID, After: &table}, nil}, {"table drop", schema.Change{ID: "c5", Operation: schema.OperationDrop, ResourceID: table.ID, Before: &table}, nil}, {"table alter", schema.Change{ID: "c6", Operation: schema.OperationAlter, ResourceID: table.ID, Before: &table, After: &table2}, nil}, {"table rename", schema.Change{ID: "c11", Operation: schema.OperationRename, ResourceID: tableRenamed.ID, Before: &table, After: &tableRenamed}, nil}, {"view create", schema.Change{ID: "c7", Operation: schema.OperationCreate, ResourceID: view.ID, After: &view}, nil}, {"view drop", schema.Change{ID: "c8", Operation: schema.OperationDrop, ResourceID: view.ID, Before: &view}, nil}, {"view alter", schema.Change{ID: "c9", Operation: schema.OperationAlter, ResourceID: view.ID, Before: &view, After: &view2}, nil}, {"view rename", schema.Change{ID: "c12", Operation: schema.OperationRename, ResourceID: viewRenamed.ID, Before: &view, After: &viewRenamed}, nil}, {"mv create", schema.Change{ID: "c13", Operation: schema.OperationCreate, ResourceID: mv.ID, After: &mv}, nil}, {"mv drop", schema.Change{ID: "c14", Operation: schema.OperationDrop, ResourceID: mv.ID, Before: &mv}, nil}, {"mv rename", schema.Change{ID: "c15", Operation: schema.OperationRename, ResourceID: mvRenamed.ID, Before: &mv, After: &mvRenamed}, nil}, {"mv rebuild", schema.Change{ID: "c10", Operation: schema.OperationAlter, ResourceID: mv.ID, Before: &mv, After: &mv2}, map[string]string{"allow_rebuild": "true"}}}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cs := schema.ChangeSet{Version: schema.ChangeVersion, Changes: []schema.Change{tc.change}}
			out, err := New().Render(context.Background(), plugin.RenderRequest{Changes: cs, Current: schema.Document{Graph: schema.Graph{Resources: mapValues(resources)}}, Desired: schema.Document{Graph: schema.Graph{Resources: mapValues(resources)}}, Options: tc.options})
			if err != nil || len(out) == 0 {
				t.Fatalf("out=%+v err=%v", out, err)
			}
		})
	}
}
func mapValues(values map[string]schema.Resource) []schema.Resource {
	out := make([]schema.Resource, 0, len(values))
	for _, r := range values {
		out = append(out, r)
	}
	return out
}
