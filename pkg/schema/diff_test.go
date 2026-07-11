package schema_test

import (
	"encoding/json"
	"errors"
	"math/rand"
	"reflect"
	"testing"

	"autosql/pkg/schema"
)

func res(kind schema.Kind, name, parent string, spec string, deps ...schema.Dependency) schema.Resource {
	namespace := "public"
	if kind == schema.KindSchema {
		namespace = ""
	}
	n := schema.Name{Schema: namespace, Name: name, Parent: parent}
	r := schema.Resource{Kind: kind, Name: n, Spec: json.RawMessage(spec), Dependencies: deps}
	r.ID = schema.StableID(kind, n)
	return r
}
func doc(resources ...schema.Resource) schema.Document {
	return schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: resources}}
}

func TestSemanticFingerprintExcludesOnlySource(t *testing.T) {
	r := res(schema.KindTable, "users", "", `{"future":{"meaning":1}}`)
	a := doc(r)
	r.Source = &schema.SourceLocation{URI: "a.sql", Line: 1}
	b := doc(r)
	af, _ := schema.SemanticFingerprint(a)
	bf, _ := schema.SemanticFingerprint(b)
	if af != bf {
		t.Fatal("source changed fingerprint")
	}
	r.Spec = json.RawMessage(`{"future":{"meaning":2}}`)
	cf, _ := schema.SemanticFingerprint(doc(r))
	if af == cf {
		t.Fatal("unknown semantics were erased")
	}
	changes, err := schema.Diff(a, b, schema.DiffOptions{})
	if err != nil || len(changes.Changes) != 0 {
		t.Fatalf("changes=%+v err=%v", changes, err)
	}
}

func TestSemanticFingerprintCanonicalizesMapsAndSets(t *testing.T) {
	parent := res(schema.KindTable, "users", "", `{"b":2,"a":1}`)
	child := res(schema.KindColumn, "id", parent.ID, `{"future":{"z":2,"a":1}}`,
		schema.Dependency{Target: parent.ID, Type: schema.DependencyReferences},
		schema.Dependency{Target: parent.ID, Type: schema.DependencyContains})
	a := doc(parent, child)
	parent.Spec = json.RawMessage(`{"a":1,"b":2}`)
	child.Spec = json.RawMessage(`{"future":{"a":1,"z":2}}`)
	child.Dependencies[0], child.Dependencies[1] = child.Dependencies[1], child.Dependencies[0]
	b := doc(child, parent)
	af, err := schema.SemanticFingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	bf, err := schema.SemanticFingerprint(b)
	if err != nil {
		t.Fatal(err)
	}
	if af != bf {
		t.Fatalf("equivalent graphs differ: %s != %s", af, bf)
	}
}

func TestCreateAndReverseDropDependencyOrder(t *testing.T) {
	s := res(schema.KindSchema, "public", "", `{}`)
	table := res(schema.KindTable, "users", s.ID, `{}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	col := res(schema.KindColumn, "id", table.ID, `{"type":"bigint"}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	full := doc(col, table, s)
	empty := doc()
	created, err := schema.Diff(empty, full, schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertNames(t, created, "public", "users", "id")
	dropped, err := schema.Diff(full, empty, schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	assertNames(t, dropped, "id", "users", "public")
	for i, c := range created.Changes {
		if i > 0 && len(c.DependsOn) == 0 {
			t.Fatalf("create dependency missing: %+v", c)
		}
	}
	for i, c := range dropped.Changes {
		if i > 0 && len(c.DependsOn) == 0 {
			t.Fatalf("drop dependency missing: %+v", c)
		}
	}
}
func assertNames(t *testing.T, cs schema.ChangeSet, want ...string) {
	t.Helper()
	got := []string{}
	for _, c := range cs.Changes {
		r := c.After
		if r == nil {
			r = c.Before
		}
		got = append(got, r.Name.Name)
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("got %v want %v", got, want)
	}
}

func TestSelectionDependencyClosure(t *testing.T) {
	s := res(schema.KindSchema, "public", "", `{}`)
	table := res(schema.KindTable, "users", s.ID, `{}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	col := res(schema.KindColumn, "email", table.ID, `{}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	selected, err := schema.Select(doc(s, table, col), []string{"column:public.email"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Graph.Resources) != 3 {
		t.Fatalf("closure=%+v", selected.Graph.Resources)
	}
	excluded, err := schema.Select(doc(s, table, col), nil, []string{"table:public.users"})
	if err != nil {
		t.Fatal(err)
	}
	if len(excluded.Graph.Resources) != 1 || excluded.Graph.Resources[0].Kind != schema.KindSchema {
		t.Fatalf("excluded=%+v", excluded.Graph.Resources)
	}
}

func TestSelectionContainerDescendantsAndBoundaryClosure(t *testing.T) {
	s := res(schema.KindSchema, "public", "", `{}`)
	enum := res(schema.KindEnum, "status", s.ID, `{"values":["active"]}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	table := res(schema.KindTable, "users", s.ID, `{}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	col := res(schema.KindColumn, "status", table.ID, `{"type":"status"}`,
		schema.Dependency{Target: table.ID, Type: schema.DependencyContains},
		schema.Dependency{Target: enum.ID, Type: schema.DependencyUses})
	all := doc(s, enum, table, col)
	selected, err := schema.Select(all, []string{"table:public.users"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(selected.Graph.Resources) != 4 {
		t.Fatalf("container descendants/dependency closure=%+v", selected.Graph.Resources)
	}
	excluded, err := schema.Select(all, []string{"table:public.users"}, []string{"enum:public.status"})
	if err != nil {
		t.Fatal(err)
	}
	for _, r := range excluded.Graph.Resources {
		if r.Kind == schema.KindColumn || r.Kind == schema.KindEnum {
			t.Fatalf("cross-boundary exclusion retained dependent: %+v", excluded.Graph.Resources)
		}
	}
}

func TestSelectionRejectsInvalidPattern(t *testing.T) {
	_, err := schema.Select(doc(), []string{"["}, nil)
	if !errors.Is(err, schema.ErrInvalidSelection) {
		t.Fatalf("error=%v", err)
	}
}

func TestExplicitRenameAndAmbiguity(t *testing.T) {
	old := res(schema.KindTable, "old", "", `{"x":1}`)
	next := res(schema.KindTable, "new", "", `{"x":1}`)
	cs, err := schema.Diff(doc(old), doc(next), schema.DiffOptions{RenameHints: []schema.RenameHint{{From: old.ID, To: next.ID}}})
	if err != nil || len(cs.Changes) != 1 || cs.Changes[0].Operation != schema.OperationRename {
		t.Fatalf("changes=%+v err=%v", cs, err)
	}
	sameTable := res(schema.KindTable, "same", "", `{}`)
	sameView := res(schema.KindView, "same", "", `{}`)
	_, err = schema.Diff(doc(sameTable, sameView), doc(next), schema.DiffOptions{RenameHints: []schema.RenameHint{{From: "public.same", To: next.ID}}})
	if !errors.Is(err, schema.ErrAmbiguousRename) {
		t.Fatalf("error=%v", err)
	}
	_, err = schema.Diff(doc(old), doc(next), schema.DiffOptions{RenameHints: []schema.RenameHint{{From: old.ID, To: next.ID}, {From: old.ID, To: next.ID}}})
	if !errors.Is(err, schema.ErrAmbiguousRename) {
		t.Fatalf("duplicate hint error=%v", err)
	}
}

func TestRenameWithAlterAndCrossParentRejection(t *testing.T) {
	old := res(schema.KindTable, "old", "", `{"x":1}`)
	next := res(schema.KindTable, "new", "", `{"x":2}`)
	cs, err := schema.Diff(doc(old), doc(next), schema.DiffOptions{RenameHints: []schema.RenameHint{{From: old.ID, To: next.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Changes) != 2 || cs.Changes[0].Operation != schema.OperationRename || cs.Changes[1].Operation != schema.OperationAlter ||
		!reflect.DeepEqual(cs.Changes[1].DependsOn, []string{cs.Changes[0].ID}) {
		t.Fatalf("rename+alter order=%+v", cs.Changes)
	}
	left := res(schema.KindTable, "left", "", `{}`)
	right := res(schema.KindTable, "right", "", `{}`)
	before := res(schema.KindColumn, "value", left.ID, `{}`, schema.Dependency{Target: left.ID, Type: schema.DependencyContains})
	after := res(schema.KindColumn, "renamed", right.ID, `{}`, schema.Dependency{Target: right.ID, Type: schema.DependencyContains})
	_, err = schema.Diff(doc(left, before), doc(right, after), schema.DiffOptions{RenameHints: []schema.RenameHint{{From: before.ID, To: after.ID}}})
	if !errors.Is(err, schema.ErrAmbiguousRename) {
		t.Fatalf("cross-parent error=%v", err)
	}
	_, err = schema.Diff(doc(old), doc(next), schema.DiffOptions{RenameHints: []schema.RenameHint{{From: "table:public.missing", To: next.ID}}})
	if !errors.Is(err, schema.ErrAmbiguousRename) {
		t.Fatalf("stale hint error=%v", err)
	}
}

func TestGeneratedNameAnnotationsCannotEraseIdentity(t *testing.T) {
	a := res(schema.KindPrimaryKey, "users_pkey", "", `{"definition":"PRIMARY KEY (id)"}`)
	b := res(schema.KindPrimaryKey, "custom_pkey", "", `{"definition":"PRIMARY KEY (id)"}`)
	a.Annotations = map[string]string{"autosql.io/generated-name": "true", "autosql.io/name-origin": "generated"}
	b.Annotations = map[string]string{"autosql.io/generated-name": "true", "autosql.io/name-origin": "generated"}
	cs, err := schema.Diff(doc(a), doc(b), schema.DiffOptions{})
	if err != nil || len(cs.Changes) != 2 {
		t.Fatalf("changes=%+v err=%v", cs, err)
	}
}

func TestParentRenamePropagatesFullChildTopology(t *testing.T) {
	s := res(schema.KindSchema, "public", "", `{}`)
	oldTable := res(schema.KindTable, "users_old", s.ID, `{}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	newTable := res(schema.KindTable, "users", s.ID, `{}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	makeChild := func(kind schema.Kind, name string, parent schema.Resource) schema.Resource {
		return res(kind, name, parent.ID, `{}`, schema.Dependency{Target: parent.ID, Type: schema.DependencyContains})
	}
	oldChildren := []schema.Resource{
		makeChild(schema.KindColumn, "id", oldTable),
		makeChild(schema.KindPrimaryKey, "users_old_pkey", oldTable),
		makeChild(schema.KindIndex, "users_email_idx", oldTable),
	}
	newChildren := []schema.Resource{
		makeChild(schema.KindColumn, "id", newTable),
		makeChild(schema.KindPrimaryKey, "users_old_pkey", newTable),
		makeChild(schema.KindIndex, "users_email_idx", newTable),
	}
	current := doc(append([]schema.Resource{s, oldTable}, oldChildren...)...)
	desired := doc(append([]schema.Resource{s, newTable}, newChildren...)...)
	cs, err := schema.Diff(current, desired, schema.DiffOptions{RenameHints: []schema.RenameHint{{From: oldTable.ID, To: newTable.ID}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs.Changes) != 4 {
		t.Fatalf("child topology produced destructive changes: %+v", cs.Changes)
	}
	for _, change := range cs.Changes {
		if change.Operation != schema.OperationRename {
			t.Fatalf("operation=%s changes=%+v", change.Operation, cs.Changes)
		}
	}
}

func TestGeneratedLookingSuffixRequiresProvenance(t *testing.T) {
	a := res(schema.KindIndex, "users_idx", "", `{"definition":"(id)"}`)
	b := res(schema.KindIndex, "renamed_idx", "", `{"definition":"(id)"}`)
	cs, err := schema.Diff(doc(a), doc(b), schema.DiffOptions{})
	if err != nil || len(cs.Changes) != 2 {
		t.Fatalf("suffix-only names ignored: changes=%+v err=%v", cs, err)
	}
}

func TestChangeIDBindsSemanticSnapshots(t *testing.T) {
	current := res(schema.KindTable, "users", "", `{"x":1}`)
	a := res(schema.KindTable, "users", "", `{"x":2}`)
	b := res(schema.KindTable, "users", "", `{"x":3}`)
	one, err := schema.Diff(doc(current), doc(a), schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	two, err := schema.Diff(doc(current), doc(b), schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	again, err := schema.Diff(doc(current), doc(a), schema.DiffOptions{})
	if err != nil {
		t.Fatal(err)
	}
	if one.Changes[0].ID == two.Changes[0].ID || one.Changes[0].ID != again.Changes[0].ID {
		t.Fatalf("semantic change IDs: %q %q %q", one.Changes[0].ID, two.Changes[0].ID, again.Changes[0].ID)
	}
}

func TestDiffDeterministicAcrossResourcePermutations(t *testing.T) {
	s := res(schema.KindSchema, "public", "", `{}`)
	table := res(schema.KindTable, "users", s.ID, `{"future":true}`, schema.Dependency{Target: s.ID, Type: schema.DependencyContains})
	col := res(schema.KindColumn, "id", table.ID, `{"type":"integer"}`, schema.Dependency{Target: table.ID, Type: schema.DependencyContains})
	desired := []schema.Resource{s, table, col}
	var baseline []byte
	for seed := int64(0); seed < 100; seed++ {
		shuffled := append([]schema.Resource(nil), desired...)
		rand.New(rand.NewSource(seed)).Shuffle(len(shuffled), func(i, j int) { shuffled[i], shuffled[j] = shuffled[j], shuffled[i] })
		cs, err := schema.Diff(doc(), doc(shuffled...), schema.DiffOptions{})
		if err != nil {
			t.Fatal(err)
		}
		raw, err := cs.MarshalCanonical()
		if err != nil {
			t.Fatal(err)
		}
		if seed == 0 {
			baseline = raw
		} else if string(raw) != string(baseline) {
			t.Fatalf("seed %d nondeterministic\n%s\n%s", seed, baseline, raw)
		}
	}
}
