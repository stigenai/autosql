package postgres

import (
	"context"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
)

func (*Driver) Render(ctx context.Context, request plugin.RenderRequest) ([]plugin.Statement, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := request.Changes.Validate(); err != nil {
		return nil, fmt.Errorf("render PostgreSQL changes: %w", err)
	}
	if err := validateManagedDocuments(request); err != nil {
		return nil, fmt.Errorf("render PostgreSQL changes: %w", err)
	}
	if err := validateColumnOrdinalTransitions(request); err != nil {
		return nil, fmt.Errorf("render PostgreSQL changes: %w", err)
	}
	if err := validateColumnDependentTransitions(request); err != nil {
		return nil, fmt.Errorf("render PostgreSQL changes: %w", err)
	}
	if err := validateParentRenameDependents(request); err != nil {
		return nil, fmt.Errorf("render PostgreSQL changes: %w", err)
	}
	rebuilds, err := validateProjectionTopology(request)
	if err != nil {
		return nil, fmt.Errorf("render PostgreSQL changes: %w", err)
	}
	resources := map[string]schema.Resource{}
	for _, doc := range []schema.Document{request.Current, request.Desired} {
		for _, r := range doc.Graph.Resources {
			resources[r.ID] = r
		}
	}
	var output []plugin.Statement
	for _, change := range request.Changes.Changes {
		options := request.Options
		if rebuilds[change.ID] {
			options = cloneOptions(options)
			options["__view_rebuild"] = "true"
		}
		statements, err := renderChange(change, resources, options)
		if err != nil {
			return nil, fmt.Errorf("render PostgreSQL changes: %w", err)
		}
		output = append(output, statements...)
	}
	return output, nil
}

// RenderDocument renders a complete desired graph from an empty database
// projection. It only renders managed kinds and never executes SQL.
func RenderDocument(ctx context.Context, doc schema.Document, options map[string]string) ([]plugin.Statement, error) {
	empty := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: []schema.Resource{}}}
	changes, err := schema.Diff(empty, doc, schema.DiffOptions{})
	if err != nil {
		return nil, err
	}
	return New().Render(ctx, plugin.RenderRequest{Changes: changes, Current: empty, Desired: doc, Options: options})
}

func renderChange(change schema.Change, resources map[string]schema.Resource, options map[string]string) ([]plugin.Statement, error) {
	r := change.After
	if r == nil {
		r = change.Before
	}
	if r == nil {
		return nil, fmt.Errorf("%w: change %s has no resource", plugin.ErrUnsupported, change.ID)
	}
	if err := validateManagedMetadata(*r); err != nil {
		return nil, err
	}
	if change.Before != nil {
		if err := validateManagedMetadata(*change.Before); err != nil {
			return nil, err
		}
	}
	parentOnlyRename := change.Operation == schema.OperationRename && change.Before != nil && change.After != nil && change.Before.Name.Name == change.After.Name.Name && change.Before.Name.Parent != change.After.Name.Parent
	projectionChild := r.Kind == schema.KindColumn && isManagedProjectionParent(r.Name.Parent, resources)
	if !parentOnlyRename && !projectionChild {
		if err := plugin.RequireManagedOperation(New().Info(), r.Kind, change.Operation); err != nil {
			return nil, err
		}
	}
	if parentOnlyRename || projectionChild {
		return []plugin.Statement{{ChangeID: change.ID, Transactional: true, Kind: plugin.StatementTopology}}, nil
	}
	if change.Operation == schema.OperationAlter && r.Kind == schema.KindColumn && columnOrdinalOnly(*change.Before, *change.After) {
		return []plugin.Statement{{ChangeID: change.ID, Transactional: true, Kind: plugin.StatementTopology}}, nil
	}
	var sqls []string
	var err error
	switch change.Operation {
	case schema.OperationCreate:
		sqls, err = renderCreate(*change.After, resources, options)
	case schema.OperationDrop:
		sqls, err = renderDrop(*change.Before, resources, options)
	case schema.OperationRename:
		sqls, err = renderRename(*change.Before, *change.After, resources)
	case schema.OperationAlter:
		sqls, err = renderAlter(*change.Before, *change.After, resources, options)
	default:
		err = fmt.Errorf("%w: operation %s", plugin.ErrUnsupported, change.Operation)
	}
	if err != nil {
		return nil, err
	}
	out := make([]plugin.Statement, len(sqls))
	for i, sql := range sqls {
		out[i] = plugin.Statement{SQL: terminate(sql), ChangeID: change.ID, Transactional: !strings.Contains(strings.ToUpper(sql), "CONCURRENTLY"), Kind: plugin.StatementExecutable}
	}
	return out, nil
}

func renderCreate(r schema.Resource, resources map[string]schema.Resource, options map[string]string) ([]string, error) {
	s := spec(r)
	name := qualified(r.Name)
	parent, err := parentName(r, resources)
	switch r.Kind {
	case schema.KindSchema:
		if !allowedKeys(s) {
			return nil, unsupported(r, "unknown schema semantics")
		}
		return []string{"CREATE SCHEMA " + quote(r.Name.Name)}, nil
	case schema.KindExtension:
		q := "CREATE EXTENSION " + quote(r.Name.Name)
		if r.Name.Schema != "" {
			q += " WITH SCHEMA " + quote(r.Name.Schema)
		}
		if v := stringValue(s, "version"); v != "" {
			q += " VERSION " + literal(v)
		}
		return []string{q}, nil
	case schema.KindEnum:
		vals := stringSlice(s, "values")
		if len(vals) == 0 {
			return nil, unsupported(r, "enum values")
		}
		qs := make([]string, len(vals))
		for i, v := range vals {
			qs[i] = literal(v)
		}
		return []string{"CREATE TYPE " + name + " AS ENUM (" + strings.Join(qs, ", ") + ")"}, nil
	case schema.KindDomain:
		base := stringValue(s, "base_type")
		if base == "" {
			return nil, unsupported(r, "domain base_type")
		}
		q := "CREATE DOMAIN " + name + " AS " + base
		if d := stringValue(s, "default"); d != "" {
			q += " DEFAULT " + d
		}
		if boolValue(s, "not_null") {
			q += " NOT NULL"
		}
		for _, constraint := range stringSlice(s, "constraints") {
			q += " " + constraint
		}
		return []string{q}, nil
	case schema.KindComposite:
		attrs, _ := s["attributes"].([]any)
		if len(attrs) == 0 {
			return nil, unsupported(r, "composite attributes")
		}
		parts := []string{}
		for _, raw := range attrs {
			o, _ := raw.(map[string]any)
			n, t := stringValue(o, "name"), stringValue(o, "type")
			if n == "" || t == "" {
				return nil, unsupported(r, "composite attribute")
			}
			parts = append(parts, quote(n)+" "+t)
		}
		return []string{"CREATE TYPE " + name + " AS (" + strings.Join(parts, ", ") + ")"}, nil
	case schema.KindSequence:
		q := "CREATE SEQUENCE " + name
		for _, x := range []struct{ k, w string }{{"start", " START WITH "}, {"increment", " INCREMENT BY "}, {"min", " MINVALUE "}, {"max", " MAXVALUE "}, {"cache", " CACHE "}} {
			if v, ok := numberValue(s, x.k); ok {
				q += x.w + v
			}
		}
		if boolValue(s, "cycle") {
			q += " CYCLE"
		}
		return []string{q}, nil
	case schema.KindTable:
		if !allowedKeys(s, "partitioned", "persistence", "row_security", "force_row_security") {
			return nil, unsupported(r, "unknown table semantics")
		}
		if e := validateTableSpec(s); e != nil {
			return nil, unsupported(r, e.Error())
		}
		if boolValue(s, "partitioned") {
			return nil, unsupported(r, "partitioned table requires an explicit partition strategy")
		}
		prefix := "CREATE "
		switch stringValue(s, "persistence") {
		case "", "p":
		default:
			return nil, unsupported(r, "temporary/unlogged table persistence is outside the managed matrix")
		}
		out := []string{prefix + "TABLE " + name + " ()"}
		if boolValue(s, "row_security") {
			out = append(out, "ALTER TABLE "+name+" ENABLE ROW LEVEL SECURITY")
		}
		if boolValue(s, "force_row_security") {
			out = append(out, "ALTER TABLE "+name+" FORCE ROW LEVEL SECURITY")
		}
		return out, nil
	case schema.KindColumn:
		if err != nil {
			return nil, err
		}
		def, e := columnDefinition(r, resources)
		if e != nil {
			return nil, e
		}
		return []string{"ALTER TABLE " + parent + " ADD COLUMN " + quote(r.Name.Name) + " " + def}, nil
	case schema.KindPrimaryKey, schema.KindUniqueConstraint, schema.KindCheckConstraint, schema.KindForeignKey:
		if err != nil {
			return nil, err
		}
		d := stringValue(s, "definition")
		if d == "" {
			return nil, unsupported(r, "constraint definition")
		}
		return []string{"ALTER TABLE " + parent + " ADD CONSTRAINT " + quote(r.Name.Name) + " " + d}, nil
	case schema.KindIndex:
		if err != nil {
			return nil, err
		}
		return renderCreateIndex(r, parent, options)
	case schema.KindView, schema.KindMaterializedView:
		if !allowedKeys(s, "definition") {
			return nil, unsupported(r, "unknown view semantics")
		}
		d := stringValue(s, "definition")
		if d == "" {
			return nil, unsupported(r, "view definition")
		}
		if e := validateSQLFragment(d); e != nil {
			return nil, unsupported(r, "unsafe view definition: "+e.Error())
		}
		if e := validateProjectionShape(r, resources); e != nil {
			return nil, unsupported(r, "output shape is not provable: "+e.Error())
		}
		kind := "VIEW"
		if r.Kind == schema.KindMaterializedView {
			kind = "MATERIALIZED VIEW"
		}
		return []string{"CREATE " + kind + " " + name + " AS " + d}, nil
	default:
		return nil, unsupported(r, "create")
	}
}

func renderDrop(r schema.Resource, resources map[string]schema.Resource, options map[string]string) ([]string, error) {
	name := qualified(r.Name)
	parent, err := parentName(r, resources)
	switch r.Kind {
	case schema.KindSchema:
		return []string{"DROP SCHEMA " + name}, nil
	case schema.KindExtension:
		return []string{"DROP EXTENSION " + quote(r.Name.Name)}, nil
	case schema.KindEnum, schema.KindDomain, schema.KindComposite:
		return []string{"DROP TYPE " + name}, nil
	case schema.KindSequence:
		return []string{"DROP SEQUENCE " + name}, nil
	case schema.KindTable:
		return []string{"DROP TABLE " + name}, nil
	case schema.KindColumn:
		if err != nil {
			return nil, err
		}
		return []string{"ALTER TABLE " + parent + " DROP COLUMN " + quote(r.Name.Name)}, nil
	case schema.KindPrimaryKey, schema.KindUniqueConstraint, schema.KindCheckConstraint, schema.KindForeignKey:
		if err != nil {
			return nil, err
		}
		return []string{"ALTER TABLE " + parent + " DROP CONSTRAINT " + quote(r.Name.Name)}, nil
	case schema.KindIndex:
		q := "DROP INDEX "
		if enabled(options, "concurrent_indexes", false) {
			q += "CONCURRENTLY "
		}
		return []string{q + name}, nil
	case schema.KindView:
		return []string{"DROP VIEW " + name}, nil
	case schema.KindMaterializedView:
		return []string{"DROP MATERIALIZED VIEW " + name}, nil
	default:
		return nil, unsupported(r, "drop")
	}
}

func renderRename(before, after schema.Resource, resources map[string]schema.Resource) ([]string, error) {
	old, newName := qualified(before.Name), quote(after.Name.Name)
	parent, err := parentName(after, resources)
	switch after.Kind {
	case schema.KindSchema:
		return []string{"ALTER SCHEMA " + old + " RENAME TO " + newName}, nil
	case schema.KindTable:
		return []string{"ALTER TABLE " + old + " RENAME TO " + newName}, nil
	case schema.KindColumn:
		if err != nil {
			return nil, err
		}
		return []string{"ALTER TABLE " + parent + " RENAME COLUMN " + quote(before.Name.Name) + " TO " + newName}, nil
	case schema.KindPrimaryKey, schema.KindUniqueConstraint, schema.KindCheckConstraint, schema.KindForeignKey:
		if err != nil {
			return nil, err
		}
		return []string{"ALTER TABLE " + parent + " RENAME CONSTRAINT " + quote(before.Name.Name) + " TO " + newName}, nil
	case schema.KindIndex:
		return []string{"ALTER INDEX " + old + " RENAME TO " + newName}, nil
	case schema.KindSequence:
		return []string{"ALTER SEQUENCE " + old + " RENAME TO " + newName}, nil
	case schema.KindView:
		return []string{"ALTER VIEW " + old + " RENAME TO " + newName}, nil
	case schema.KindMaterializedView:
		return []string{"ALTER MATERIALIZED VIEW " + old + " RENAME TO " + newName}, nil
	case schema.KindEnum, schema.KindDomain, schema.KindComposite:
		return []string{"ALTER TYPE " + old + " RENAME TO " + newName}, nil
	default:
		return nil, unsupported(after, "rename")
	}
}

func renderAlter(before, after schema.Resource, resources map[string]schema.Resource, options map[string]string) ([]string, error) {
	if before.Kind != after.Kind {
		return nil, unsupported(after, "kind change")
	}
	bs, as := spec(before), spec(after)
	name := qualified(after.Name)
	parent, err := parentName(after, resources)
	switch after.Kind {
	case schema.KindColumn:
		if err != nil {
			return nil, err
		}
		var out []string
		btype, atype := stringValue(bs, "type"), stringValue(as, "type")
		if btype != atype {
			if atype == "" {
				return nil, unsupported(after, "column type")
			}
			if !safeAssignmentCast(btype, atype) {
				return nil, unsupported(after, "column type change is not a known implicit or assignment-safe cast")
			}
			out = append(out, "ALTER TABLE "+parent+" ALTER COLUMN "+quote(after.Name.Name)+" TYPE "+atype)
		}
		bd, ad := stringValue(bs, "default"), stringValue(as, "default")
		if bd != ad {
			q := "ALTER TABLE " + parent + " ALTER COLUMN " + quote(after.Name.Name)
			if ad == "" {
				q += " DROP DEFAULT"
			} else {
				q += " SET DEFAULT " + ad
			}
			out = append(out, q)
		}
		bn, an := boolValue(bs, "not_null"), boolValue(as, "not_null")
		if bn != an {
			action := " DROP NOT NULL"
			if an {
				action = " SET NOT NULL"
			}
			out = append(out, "ALTER TABLE "+parent+" ALTER COLUMN "+quote(after.Name.Name)+action)
		}
		if stringValue(bs, "identity") != stringValue(as, "identity") {
			return nil, unsupported(after, "identity alteration")
		}
		if len(out) == 0 {
			return nil, unsupported(after, "column alteration")
		}
		return out, nil
	case schema.KindEnum:
		old, new := stringSlice(bs, "values"), stringSlice(as, "values")
		if len(new) < len(old) {
			return nil, unsupported(after, "enum value removal")
		}
		for i := range old {
			if old[i] != new[i] {
				return nil, unsupported(after, "enum reorder")
			}
		}
		out := []string{}
		for _, v := range new[len(old):] {
			out = append(out, "ALTER TYPE "+name+" ADD VALUE "+literal(v))
		}
		if len(out) == 0 {
			return nil, unsupported(after, "enum alteration")
		}
		return out, nil
	case schema.KindView:
		if !allowedKeys(bs, "definition") || !allowedKeys(as, "definition") {
			return nil, unsupported(after, "unknown view semantics")
		}
		d := stringValue(as, "definition")
		if d == "" {
			return nil, unsupported(after, "view definition")
		}
		if e := validateSQLFragment(d); e != nil {
			return nil, unsupported(after, "unsafe view definition: "+e.Error())
		}
		if e := validateProjectionShape(after, resources); e != nil {
			return nil, unsupported(after, "output shape is not provable: "+e.Error())
		}
		if enabled(options, "__view_rebuild", false) {
			drop, e := renderDrop(before, resources, options)
			if e != nil {
				return nil, e
			}
			create, e := renderCreate(after, resources, options)
			return append(drop, create...), e
		}
		return []string{"CREATE OR REPLACE VIEW " + name + " AS " + d}, nil
	case schema.KindIndex:
		if !enabled(options, "allow_rebuild", false) {
			return nil, unsupported(after, "index rebuild requires allow_rebuild=true")
		}
		drop, _ := renderDrop(before, resources, options)
		create, e := renderCreate(after, resources, options)
		return append(drop, create...), e
	case schema.KindPrimaryKey, schema.KindUniqueConstraint, schema.KindCheckConstraint, schema.KindForeignKey, schema.KindMaterializedView:
		if !enabled(options, "allow_rebuild", false) {
			return nil, unsupported(after, "rebuild requires allow_rebuild=true")
		}
		drop, e := renderDrop(before, resources, options)
		if e != nil {
			return nil, e
		}
		create, e := renderCreate(after, resources, options)
		return append(drop, create...), e
	case schema.KindExtension:
		version := stringValue(as, "version")
		if version == "" || version == stringValue(bs, "version") {
			return nil, unsupported(after, "extension alteration")
		}
		return []string{"ALTER EXTENSION " + quote(after.Name.Name) + " UPDATE TO " + literal(version)}, nil
	case schema.KindSequence:
		q := "ALTER SEQUENCE " + name
		changed := false
		for _, x := range []struct{ k, w string }{{"start", " START WITH "}, {"increment", " INCREMENT BY "}, {"min", " MINVALUE "}, {"max", " MAXVALUE "}, {"cache", " CACHE "}} {
			bv, bok := numberValue(bs, x.k)
			av, aok := numberValue(as, x.k)
			if bok != aok || bv != av {
				if !aok {
					return nil, unsupported(after, "sequence option removal")
				}
				q += x.w + av
				changed = true
			}
		}
		if boolValue(bs, "cycle") != boolValue(as, "cycle") {
			if boolValue(as, "cycle") {
				q += " CYCLE"
			} else {
				q += " NO CYCLE"
			}
			changed = true
		}
		if !changed {
			return nil, unsupported(after, "sequence alteration")
		}
		return []string{q}, nil
	case schema.KindTable:
		if !allowedKeys(bs, "partitioned", "persistence", "row_security", "force_row_security") || !allowedKeys(as, "partitioned", "persistence", "row_security", "force_row_security") {
			return nil, unsupported(after, "unknown table semantics")
		}
		if e := validateTableSpec(bs); e != nil {
			return nil, unsupported(before, e.Error())
		}
		if e := validateTableSpec(as); e != nil {
			return nil, unsupported(after, e.Error())
		}
		if stringValue(bs, "persistence") != stringValue(as, "persistence") || boolValue(bs, "partitioned") != boolValue(as, "partitioned") {
			return nil, unsupported(after, "table storage alteration")
		}
		var out []string
		if boolValue(bs, "row_security") != boolValue(as, "row_security") {
			action := " DISABLE ROW LEVEL SECURITY"
			if boolValue(as, "row_security") {
				action = " ENABLE ROW LEVEL SECURITY"
			}
			out = append(out, "ALTER TABLE "+name+action)
		}
		if boolValue(bs, "force_row_security") != boolValue(as, "force_row_security") {
			action := " NO FORCE ROW LEVEL SECURITY"
			if boolValue(as, "force_row_security") {
				action = " FORCE ROW LEVEL SECURITY"
			}
			out = append(out, "ALTER TABLE "+name+action)
		}
		if len(out) == 0 {
			return nil, unsupported(after, "table alteration")
		}
		return out, nil
	default:
		return nil, unsupported(after, "alter")
	}
}

func columnOrdinalOnly(before, after schema.Resource) bool {
	bs, as := spec(before), spec(after)
	if numberAsInt(bs, "ordinal") == numberAsInt(as, "ordinal") {
		return false
	}
	delete(bs, "ordinal")
	delete(as, "ordinal")
	return slices.Equal(canonicalMapEntries(bs), canonicalMapEntries(as))
}

func canonicalMapEntries(m map[string]any) []string {
	keys := make([]string, 0, len(m))
	for key, value := range m {
		keys = append(keys, fmt.Sprintf("%s=%v", key, value))
	}
	sort.Strings(keys)
	return keys
}

func safeAssignmentCast(before, after string) bool {
	normalize := func(value string) string { return strings.ToLower(strings.TrimSpace(value)) }
	pair := normalize(before) + "->" + normalize(after)
	switch pair {
	case "smallint->integer", "smallint->bigint", "smallint->numeric",
		"integer->bigint", "integer->numeric", "bigint->numeric",
		"real->double precision", "character varying->text", "character->text":
		return true
	default:
		return false
	}
}

func renderCreateIndex(r schema.Resource, parent string, options map[string]string) ([]string, error) {
	s := spec(r)
	d := stringValue(s, "definition")
	if d == "" {
		return nil, unsupported(r, "index definition")
	}
	u := strings.ToUpper(strings.TrimSpace(d))
	if strings.HasPrefix(u, "CREATE ") {
		if enabled(options, "concurrent_indexes", false) && !strings.Contains(u, "CONCURRENTLY") {
			d = strings.Replace(d, "CREATE INDEX", "CREATE INDEX CONCURRENTLY", 1)
		}
		return []string{d}, nil
	}
	q := "CREATE "
	if boolValue(s, "unique") {
		q += "UNIQUE "
	}
	q += "INDEX "
	if enabled(options, "concurrent_indexes", false) {
		q += "CONCURRENTLY "
	}
	q += qualified(r.Name) + " ON " + parent + " " + d
	return []string{q}, nil
}
func columnDefinition(r schema.Resource, resources map[string]schema.Resource) (string, error) {
	s := spec(r)
	t := stringValue(s, "type")
	if t == "" {
		return "", unsupported(r, "column type")
	}
	q := t
	var uses []schema.Resource
	for _, dep := range r.Dependencies {
		if dep.Type == schema.DependencyUses {
			if target, ok := resources[dep.Target]; ok {
				uses = append(uses, target)
			}
		}
	}
	if len(uses) > 1 {
		return "", unsupported(r, "column type has ambiguous uses dependencies")
	}
	if len(uses) == 1 {
		q = quote(uses[0].Name.Schema) + "." + quote(uses[0].Name.Name)
		if strings.HasSuffix(t, "[]") {
			q += "[]"
		}
	}
	d := stringValue(s, "default")
	generated := stringValue(s, "generated")
	if generated != "" {
		if generated != "s" || d == "" {
			return "", unsupported(r, "generated column")
		}
		q += " GENERATED ALWAYS AS (" + d + ") STORED"
	} else if d != "" {
		q += " DEFAULT " + d
	}
	if boolValue(s, "not_null") {
		q += " NOT NULL"
	}
	switch stringValue(s, "identity") {
	case "a", "always":
		q += " GENERATED ALWAYS AS IDENTITY"
	case "d", "by_default":
		q += " GENERATED BY DEFAULT AS IDENTITY"
	case "":
	default:
		return "", unsupported(r, "identity")
	}
	return q, nil
}
func parentName(r schema.Resource, resources map[string]schema.Resource) (string, error) {
	p, ok := resources[r.Name.Parent]
	if !ok {
		return "", unsupported(r, "missing parent resource")
	}
	return qualified(p.Name), nil
}
func spec(r schema.Resource) map[string]any {
	m := map[string]any{}
	_ = json.Unmarshal(r.Spec, &m)
	return m
}
func stringValue(m map[string]any, k string) string { v, _ := m[k].(string); return v }
func boolValue(m map[string]any, k string) bool     { v, _ := m[k].(bool); return v }
func numberValue(m map[string]any, k string) (string, bool) {
	v, ok := m[k]
	if !ok {
		return "", false
	}
	switch n := v.(type) {
	case float64:
		return strconv.FormatFloat(n, 'f', -1, 64), true
	case json.Number:
		return n.String(), true
	}
	return "", false
}
func stringSlice(m map[string]any, k string) []string {
	raw, _ := m[k].([]any)
	out := make([]string, 0, len(raw))
	for _, v := range raw {
		if s, ok := v.(string); ok {
			out = append(out, s)
		}
	}
	return out
}
func quote(v string) string { return `"` + strings.ReplaceAll(v, `"`, `""`) + `"` }
func qualified(n schema.Name) string {
	parts := []string{}
	for _, v := range []string{n.Catalog, n.Schema, n.Name} {
		if v != "" {
			parts = append(parts, quote(v))
		}
	}
	return strings.Join(parts, ".")
}
func literal(v string) string { return `'` + strings.ReplaceAll(v, `'`, `''`) + `'` }
func terminate(sql string) string {
	sql = strings.TrimSpace(sql)
	if !strings.HasSuffix(sql, ";") {
		sql += ";"
	}
	return sql
}
func unsupported(r schema.Resource, what string) error {
	return fmt.Errorf("%w: %s %s %s", plugin.ErrUnsupported, r.Kind, r.Name.String(), what)
}
func isManagedProjectionParent(id string, resources map[string]schema.Resource) bool {
	r, ok := resources[id]
	return ok && (r.Kind == schema.KindView || r.Kind == schema.KindMaterializedView)
}
func validateProjectionShape(view schema.Resource, resources map[string]schema.Resource) error {
	var children []schema.Resource
	for _, r := range resources {
		if r.Kind == schema.KindColumn && r.Name.Parent == view.ID {
			children = append(children, r)
		}
	}
	if len(children) == 0 {
		return fmt.Errorf("no canonical output columns")
	}
	definition := stringValue(spec(view), "definition")
	expected := map[string]string{}
	expectedOrdinal := map[string]int{}
	if match := simpleViewMatch(definition); match != nil {
		if strings.TrimSpace(match[1]) == "*" {
			return fmt.Errorf("wildcard was not canonically expanded")
		}
		var table schema.Resource
		for _, r := range resources {
			if r.Kind == schema.KindTable && r.Name.Schema+"."+r.Name.Name == match[2]+"."+match[3] {
				table = r
			}
		}
		if table.ID == "" {
			return fmt.Errorf("source table is absent")
		}
		for index, item := range strings.Split(match[1], ",") {
			name := strings.TrimSpace(item)
			if dot := strings.LastIndex(name, "."); dot >= 0 {
				name = name[dot+1:]
			}
			for _, r := range resources {
				if r.Kind == schema.KindColumn && r.Name.Parent == table.ID && r.Name.Name == name {
					expected[name] = stringValue(spec(r), "type")
					expectedOrdinal[name] = index + 1
				}
			}
		}
	} else if match := simpleLiteralMatch(definition); match != nil {
		expr, name := strings.TrimSpace(match[1]), match[2]
		if _, e := strconv.Atoi(expr); e == nil {
			expected[name] = "integer"
			expectedOrdinal[name] = 1
		} else if strings.HasSuffix(expr, "::text") && len(strings.TrimSuffix(expr, "::text")) >= 2 && strings.TrimSuffix(expr, "::text")[0] == '\'' {
			expected[name] = "text"
			expectedOrdinal[name] = 1
		}
	}
	if len(expected) != len(children) {
		return fmt.Errorf("projection count mismatch")
	}
	for _, child := range children {
		if expected[child.Name.Name] == "" || expected[child.Name.Name] != stringValue(spec(child), "type") || boolValue(spec(child), "not_null") || numberAsInt(spec(child), "ordinal") != expectedOrdinal[child.Name.Name] {
			return fmt.Errorf("projection %s mismatch", child.Name.Name)
		}
	}
	return nil
}
func validateManagedMetadata(r schema.Resource) error {
	if r.Name.Catalog != "" {
		return unsupported(r, "PostgreSQL catalog qualification is not renderable")
	}
	if len(r.Annotations) > 0 || len(r.Extra) > 0 || len(r.Name.Extra) > 0 {
		return unsupported(r, "annotations, comments, and extension metadata are not renderable")
	}
	for _, dep := range r.Dependencies {
		if len(dep.Extra) > 0 {
			return unsupported(r, "dependency extension metadata is not renderable")
		}
	}
	for _, part := range []string{r.Name.Catalog, r.Name.Schema, r.Name.Name} {
		for _, ch := range part {
			if ch < ' ' || ch == 127 {
				return unsupported(r, "identifier contains control characters")
			}
		}
	}
	return nil
}
func validateTableSpec(values map[string]any) error {
	for _, key := range []string{"partitioned", "row_security", "force_row_security"} {
		if value, ok := values[key]; ok {
			if _, valid := value.(bool); !valid {
				return fmt.Errorf("table %s must be boolean", key)
			}
		}
	}
	if value, ok := values["persistence"]; ok {
		if _, valid := value.(string); !valid {
			return fmt.Errorf("table persistence must be a string")
		}
	}
	return nil
}
func cloneOptions(in map[string]string) map[string]string {
	out := map[string]string{}
	for k, v := range in {
		out[k] = v
	}
	return out
}
func validateProjectionTopology(request plugin.RenderRequest) (map[string]bool, error) {
	current, desired := resourceMapForRender(request.Current), resourceMapForRender(request.Desired)
	parents := map[string]schema.Change{}
	for _, change := range request.Changes.Changes {
		if change.Before != nil {
			parents[change.Before.ID] = change
		}
		if change.After != nil {
			parents[change.After.ID] = change
		}
	}
	rebuilds := map[string]bool{}
	for _, change := range request.Changes.Changes {
		r := change.After
		if r == nil {
			r = change.Before
		}
		if r == nil {
			continue
		}
		if r.Kind == schema.KindView || r.Kind == schema.KindMaterializedView {
			if change.Operation == schema.OperationAlter {
				beforeSig, e := projectionSignature(current, change.Before.ID)
				if e != nil {
					return nil, e
				}
				afterSig, e := projectionSignature(desired, change.After.ID)
				if e != nil {
					return nil, e
				}
				needsRebuild := r.Kind == schema.KindMaterializedView || beforeSig != afterSig
				if needsRebuild {
					if !enabled(request.Options, "allow_rebuild", false) {
						return nil, unsupported(*r, "view output shape change requires allow_rebuild=true")
					}
					rebuilds[change.ID] = true
					if e := validateRebuildDependents(current, desired, change.Before.ID, request.Changes); e != nil {
						return nil, e
					}
				}
			}
			continue
		}
		if r.Kind != schema.KindColumn {
			continue
		}
		beforeParent, afterParent := "", ""
		if change.Before != nil {
			beforeParent = change.Before.Name.Parent
		}
		if change.After != nil {
			afterParent = change.After.Name.Parent
		}
		if !isProjectionID(beforeParent, current) && !isProjectionID(afterParent, desired) {
			continue
		}
		if change.Before != nil && change.Operation != schema.OperationRename {
			if e := validateProjectionResource(*change.Before, beforeParent); e != nil {
				return nil, e
			}
		}
		if change.After != nil && change.Operation != schema.OperationRename {
			if e := validateProjectionResource(*change.After, afterParent); e != nil {
				return nil, e
			}
		}
		parent, ok := parents[beforeParent]
		if !ok {
			parent, ok = parents[afterParent]
		}
		if !ok {
			return nil, unsupported(*r, "independent projection child transition")
		}
		allowed := false
		switch change.Operation {
		case schema.OperationCreate:
			allowed = parent.Operation == schema.OperationCreate || parent.Operation == schema.OperationAlter && enabled(request.Options, "allow_rebuild", false)
		case schema.OperationDrop:
			allowed = parent.Operation == schema.OperationDrop || parent.Operation == schema.OperationAlter && enabled(request.Options, "allow_rebuild", false)
		case schema.OperationRename:
			allowed = parent.Operation == schema.OperationRename && change.Before.Name.Name == change.After.Name.Name && sameProjectionSpec(*change.Before, *change.After)
		case schema.OperationAlter:
			allowed = parent.Operation == schema.OperationAlter && enabled(request.Options, "allow_rebuild", false)
		}
		if !allowed {
			return nil, unsupported(*r, "projection child transition is not a proven parent consequence")
		}
	}
	return rebuilds, nil
}
func validateRebuildDependents(current, desired map[string]schema.Resource, parent string, changes schema.ChangeSet) error {
	drops := map[string]schema.Change{}
	creates := map[string]schema.Change{}
	for _, change := range changes.Changes {
		if change.Operation == schema.OperationDrop && change.Before != nil {
			drops[change.Before.ID] = change
		}
		if change.Operation == schema.OperationCreate && change.After != nil {
			creates[change.After.ID] = change
		}
	}
	for _, r := range current {
		if r.Kind == schema.KindColumn && r.Name.Parent == parent {
			continue
		}
		dependent := r.Name.Parent == parent
		for _, dep := range r.Dependencies {
			dependent = dependent || dep.Target == parent && (dep.Type == schema.DependencyReferences || dep.Type == schema.DependencyOwns)
		}
		if !dependent {
			continue
		}
		if _, ok := drops[r.ID]; !ok {
			return unsupported(r, "unchanged dependent blocks parent rebuild")
		}
		if _, ok := creates[r.ID]; !ok {
			return unsupported(r, "dependent must have a complete managed drop/recreate transition")
		}
		if e := plugin.RequireManagedOperation(New().Info(), r.Kind, schema.OperationDrop); e != nil {
			return unsupported(r, "dependent drop is not managed")
		}
		if e := plugin.RequireManagedOperation(New().Info(), desired[r.ID].Kind, schema.OperationCreate); e != nil {
			return unsupported(r, "dependent recreate is not managed")
		}
	}
	return nil
}
func validateColumnOrdinalTransitions(request plugin.RenderRequest) error {
	current, desired := resourceMapForRender(request.Current), resourceMapForRender(request.Desired)
	renames := map[string]string{}
	renamedAfter := map[string]bool{}
	parentRenames := map[string]string{}
	renamedParents := map[string]bool{}
	for _, change := range request.Changes.Changes {
		if change.Operation == schema.OperationRename && change.Before != nil && change.After != nil && change.Before.Kind == schema.KindColumn {
			renames[change.Before.ID] = change.After.ID
			renamedAfter[change.After.ID] = true
		}
		if change.Operation == schema.OperationRename && change.Before != nil && change.After != nil && change.Before.Kind == schema.KindTable {
			parentRenames[change.Before.ID] = change.After.ID
			renamedParents[change.After.ID] = true
		}
	}
	parents := map[string]bool{}
	for _, resources := range []map[string]schema.Resource{current, desired} {
		for _, r := range resources {
			if r.Kind == schema.KindColumn && resources[r.Name.Parent].Kind == schema.KindTable {
				parents[r.Name.Parent] = true
			}
		}
	}
	ordered := func(resources map[string]schema.Resource, parent string) []schema.Resource {
		var columns []schema.Resource
		for _, r := range resources {
			if r.Kind == schema.KindColumn && r.Name.Parent == parent {
				columns = append(columns, r)
			}
		}
		sort.Slice(columns, func(i, j int) bool {
			return numberAsInt(spec(columns[i]), "ordinal") < numberAsInt(spec(columns[j]), "ordinal")
		})
		return columns
	}
	for parent := range parents {
		if renamedParents[parent] {
			continue
		}
		afterParent := parent
		if renamed := parentRenames[parent]; renamed != "" {
			afterParent = renamed
		}
		before, after := ordered(current, parent), ordered(desired, afterParent)
		var achievable []string
		for _, column := range before {
			target, retained := desired[column.ID]
			physicalID := column.ID
			if renamed := renames[column.ID]; renamed != "" {
				target, retained, physicalID = desired[renamed], true, renamed
			}
			if retained {
				achievable = append(achievable, physicalID)
				beforeSpec, afterSpec := spec(column), spec(target)
				delete(beforeSpec, "ordinal")
				delete(afterSpec, "ordinal")
				if physicalID != column.ID && !slices.Equal(canonicalMapEntries(beforeSpec), canonicalMapEntries(afterSpec)) {
					return unsupported(target, "column rename cannot include attribute changes")
				}
				if numberAsInt(spec(column), "ordinal") != numberAsInt(spec(target), "ordinal") && !columnOrdinalOnly(column, target) {
					return unsupported(target, "ordinal shift cannot be mixed with attribute changes")
				}
			}
		}
		for _, column := range after {
			if _, existed := current[column.ID]; !existed && !renamedAfter[column.ID] {
				achievable = append(achievable, column.ID)
			}
		}
		actual := make([]string, len(after))
		for i := range after {
			actual[i] = after[i].ID
		}
		if !slices.Equal(achievable, actual) {
			r := schema.Resource{Kind: schema.KindTable, ID: parent}
			if candidate, ok := desired[parent]; ok {
				r = candidate
			}
			return unsupported(r, "column order requires middle insertion or reorder")
		}
	}
	return nil
}

func validateManagedDocuments(request plugin.RenderRequest) error {
	for _, doc := range []schema.Document{request.Current, request.Desired} {
		resources := resourceMapForRender(doc)
		if e := validateCoreColumnOrdinals(resources); e != nil {
			return e
		}
		for _, r := range doc.Graph.Resources {
			mode := New().Info().Capability(r.Kind).Mode
			if mode == plugin.Managed {
				if e := validateManagedMetadata(r); e != nil {
					return e
				}
				if e := validateCanonicalIdentity(r, resources); e != nil {
					return e
				}
				if e := validateSemanticDependencies(r, resources); e != nil {
					return e
				}
				s := spec(r)
				switch r.Kind {
				case schema.KindSchema:
					if !allowedKeys(s) {
						return unsupported(r, "unknown schema semantics")
					}
				case schema.KindTable:
					if !allowedKeys(s, "partitioned", "persistence", "row_security", "force_row_security") {
						return unsupported(r, "unknown table semantics")
					}
					if e := validateTableSpec(s); e != nil {
						return unsupported(r, e.Error())
					}
					if boolValue(s, "partitioned") || stringValue(s, "persistence") != "p" && stringValue(s, "persistence") != "" {
						return unsupported(r, "table storage is outside managed matrix")
					}
				case schema.KindView, schema.KindMaterializedView:
					if !allowedKeys(s, "definition") {
						return unsupported(r, "unknown view semantics")
					}
					if e := validateSQLFragment(stringValue(s, "definition")); e != nil {
						return unsupported(r, e.Error())
					}
					if e := validateProjectionShape(r, resources); e != nil {
						return unsupported(r, e.Error())
					}
				case schema.KindColumn:
					if !allowedKeys(s, "type", "default", "not_null", "ordinal") {
						return unsupported(r, "unknown column semantics")
					}
					if _, ok := s["type"].(string); !ok {
						return unsupported(r, "column type must be a string")
					}
					if _, ok := s["not_null"].(bool); !ok {
						return unsupported(r, "column not_null must be boolean")
					}
					if ordinal, ok := s["ordinal"].(float64); !ok || ordinal < 1 || ordinal != float64(int(ordinal)) {
						return unsupported(r, "column ordinal must be a positive integer")
					}
					if e := validateCoreColumnType(r, resources); e != nil {
						return e
					}
					if d := stringValue(s, "default"); d != "" {
						if e := validateCoreDefault(r, d); e != nil {
							return e
						}
					}
				}
			} else if r.Kind == schema.KindColumn && isManagedProjectionParent(r.Name.Parent, resources) {
				if e := validateManagedMetadata(r); e != nil {
					return e
				}
				if e := validateProjectionResource(r, r.Name.Parent); e != nil {
					return e
				}
			}
		}
	}
	return nil
}
func validateCoreColumnType(r schema.Resource, resources map[string]schema.Resource) error {
	for _, dep := range r.Dependencies {
		if dep.Type == schema.DependencyUses {
			return nil
		}
	}
	typ := stringValue(spec(r), "type")
	base := strings.TrimSuffix(typ, "[]")
	allowed := map[string]bool{"smallint": true, "integer": true, "bigint": true, "real": true, "double precision": true, "numeric": true, "boolean": true, "text": true, "character varying": true, "date": true, "timestamp": true, "timestamptz": true, "uuid": true, "json": true, "jsonb": true, "bytea": true}
	if !allowed[base] || typ != base && typ != base+"[]" {
		return unsupported(r, "column type is outside canonical core grammar")
	}
	return nil
}
func validateCoreDefault(r schema.Resource, value string) error {
	typ := strings.TrimSuffix(stringValue(spec(r), "type"), "[]")
	if strings.HasSuffix(stringValue(spec(r), "type"), "[]") {
		return unsupported(r, "array defaults are not modeled")
	}
	integer := regexp.MustCompile(`^-?[0-9]+$`)
	quoted := regexp.MustCompile(`^'(?:''|[^'])*'$`)
	ok := false
	switch typ {
	case "smallint", "integer", "bigint":
		if integer.MatchString(value) && (value == "0" || !strings.HasPrefix(value, "0")) && !strings.HasPrefix(value, "-0") {
			bits := 64
			if typ == "smallint" {
				bits = 16
			}
			if typ == "integer" {
				bits = 32
			}
			_, err := strconv.ParseInt(value, 10, bits)
			ok = err == nil
		}
	case "real", "double precision", "numeric":
		ok = false
	case "boolean":
		ok = value == "true" || value == "false"
	case "text", "character varying":
		ok = quoted.MatchString(value)
	case "timestamp", "timestamptz":
		ok = value == "CURRENT_TIMESTAMP"
	}
	if !ok {
		return unsupported(r, "column default is outside canonical core grammar")
	}
	return nil
}
func validateParentRenameDependents(request plugin.RenderRequest) error {
	current := resourceMapForRender(request.Current)
	dropped := map[string]bool{}
	for _, change := range request.Changes.Changes {
		if change.Operation == schema.OperationDrop && change.Before != nil {
			dropped[change.Before.ID] = true
		}
	}
	for _, change := range request.Changes.Changes {
		if change.Operation != schema.OperationRename || change.Before == nil || (change.Before.Kind != schema.KindTable && change.Before.Kind != schema.KindSchema && change.Before.Kind != schema.KindView && change.Before.Kind != schema.KindMaterializedView) {
			continue
		}
		root := change.Before.ID
		isDescendant := func(r schema.Resource) bool {
			p := r.Name.Parent
			for p != "" {
				if p == root {
					return true
				}
				parent, ok := current[p]
				if !ok {
					break
				}
				p = parent.Name.Parent
			}
			return false
		}
		for _, r := range current {
			if dropped[r.ID] {
				continue
			}
			opaqueDescendant := isDescendant(r) && r.Kind != schema.KindTable && r.Kind != schema.KindColumn
			dependent := false
			for _, dep := range r.Dependencies {
				if dep.Type == schema.DependencyReferences && (dep.Target == root || isDescendant(current[dep.Target])) {
					dependent = true
				}
			}
			if opaqueDescendant || dependent {
				return unsupported(r, "retained opaque object may be rewritten by parent rename")
			}
		}
	}
	return nil
}
func validateColumnDependentTransitions(request plugin.RenderRequest) error {
	current, desired := resourceMapForRender(request.Current), resourceMapForRender(request.Desired)
	for _, change := range request.Changes.Changes {
		if change.Before == nil || change.Before.Kind != schema.KindColumn || (change.Operation != schema.OperationDrop && change.Operation != schema.OperationRename) {
			continue
		}
		table := change.Before.Name.Parent
		for _, dependent := range current {
			if dependent.Kind == schema.KindColumn {
				continue
			}
			mayDepend := dependent.Name.Parent == table
			for _, dep := range dependent.Dependencies {
				if dep.Target == table && dep.Type == schema.DependencyReferences {
					mayDepend = true
				}
			}
			if mayDepend {
				if _, retained := desired[dependent.ID]; retained {
					return unsupported(dependent, "retained read-only object may depend on changed column")
				}
			}
		}
	}
	return nil
}
func validateCoreColumnOrdinals(resources map[string]schema.Resource) error {
	groups := map[string][]schema.Resource{}
	for _, r := range resources {
		if r.Kind == schema.KindColumn && resources[r.Name.Parent].Kind == schema.KindTable {
			groups[r.Name.Parent] = append(groups[r.Name.Parent], r)
		}
	}
	for _, columns := range groups {
		sort.Slice(columns, func(i, j int) bool {
			return numberAsInt(spec(columns[i]), "ordinal") < numberAsInt(spec(columns[j]), "ordinal")
		})
		for i, column := range columns {
			if numberAsInt(spec(column), "ordinal") != i+1 {
				return unsupported(column, "table column ordinals must be contiguous and unique")
			}
		}
	}
	return nil
}
func validateSemanticDependencies(r schema.Resource, resources map[string]schema.Resource) error {
	expectedType := schema.DependencyUses
	var expected []string
	switch r.Kind {
	case schema.KindView, schema.KindMaterializedView:
		expectedType = schema.DependencyReferences
		definition := stringValue(spec(r), "definition")
		if match := simpleViewMatch(definition); match != nil {
			for id, candidate := range resources {
				if (candidate.Kind == schema.KindTable || candidate.Kind == schema.KindView || candidate.Kind == schema.KindMaterializedView) && candidate.Name.Schema == match[2] && candidate.Name.Name == match[3] {
					expected = append(expected, id)
				}
			}
			if len(expected) != 1 {
				return unsupported(r, "view source dependency is not provable")
			}
		} else if simpleLiteralMatch(definition) == nil {
			return unsupported(r, "view dependencies are not provable from definition")
		}
	case schema.KindColumn:
		if parent := resources[r.Name.Parent]; parent.Kind != schema.KindTable {
			return nil
		}
		typ := stringValue(spec(r), "type")
		for id, candidate := range resources {
			switch candidate.Kind {
			case schema.KindEnum, schema.KindDomain, schema.KindComposite:
				if typeReferenceMatches(typ, r.Name.Schema, candidate.Name) {
					expected = append(expected, id)
				}
			}
		}
	default:
		return nil
	}
	var actual []string
	for _, dep := range r.Dependencies {
		if dep.Type == expectedType {
			actual = append(actual, dep.Target)
		}
	}
	sort.Strings(expected)
	sort.Strings(actual)
	if !slices.Equal(expected, actual) {
		return unsupported(r, "declared dependencies do not exactly match rendered semantics")
	}
	return nil
}
func typeReferenceMatches(typ, columnSchema string, name schema.Name) bool {
	base := strings.TrimSpace(typ)
	for strings.HasSuffix(base, "[]") {
		base = strings.TrimSpace(strings.TrimSuffix(base, "[]"))
	}
	quotedName := quote(name.Name)
	quotedSchema := quote(name.Schema)
	qualified := []string{quotedSchema + "." + quotedName, name.Schema + "." + quotedName}
	if name.Name == strings.ToLower(name.Name) {
		qualified = append(qualified, quotedSchema+"."+name.Name, name.Schema+"."+name.Name)
	}
	for _, spelling := range qualified {
		if base == spelling {
			return true
		}
	}
	if name.Schema == columnSchema || name.Schema == "public" {
		if base == quotedName || name.Name == strings.ToLower(name.Name) && base == name.Name {
			return true
		}
	}
	return false
}
func validateCanonicalIdentity(r schema.Resource, resources map[string]schema.Resource) error {
	if r.Name.Catalog != "" {
		return unsupported(r, "catalog must be empty")
	}
	switch r.Kind {
	case schema.KindSchema:
		if r.Name.Schema != "" || r.Name.Parent != "" || len(r.Dependencies) != 0 {
			return unsupported(r, "schema name/parent/dependencies are noncanonical")
		}
	case schema.KindTable, schema.KindView, schema.KindMaterializedView:
		parent, ok := resources[r.Name.Parent]
		if !ok || parent.Kind != schema.KindSchema || parent.Name.Name != r.Name.Schema || r.Name.Schema == "" {
			return unsupported(r, "schema parent is noncanonical")
		}
		contains := 0
		for _, dep := range r.Dependencies {
			if dep.Target == parent.ID && dep.Type == schema.DependencyContains {
				contains++
				continue
			}
			if (r.Kind == schema.KindView || r.Kind == schema.KindMaterializedView) && dep.Type == schema.DependencyReferences {
				if _, ok := resources[dep.Target]; ok {
					continue
				}
			}
			return unsupported(r, "dependencies are noncanonical")
		}
		if contains != 1 {
			return unsupported(r, "exactly one schema containment dependency is required")
		}
	case schema.KindColumn:
		parent, ok := resources[r.Name.Parent]
		if !ok || (parent.Kind != schema.KindTable && parent.Kind != schema.KindView && parent.Kind != schema.KindMaterializedView) || r.Name.Schema != parent.Name.Schema {
			return unsupported(r, "column parent is noncanonical")
		}
		contains := 0
		for _, dep := range r.Dependencies {
			if dep.Target == parent.ID && dep.Type == schema.DependencyContains {
				contains++
				continue
			}
			if dep.Type == schema.DependencyUses {
				if _, ok := resources[dep.Target]; ok {
					continue
				}
			}
			return unsupported(r, "column dependencies are noncanonical")
		}
		if contains != 1 {
			return unsupported(r, "column requires exactly one parent containment dependency")
		}
	}
	return nil
}
func validateNativeAtom(value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("empty")
	}
	quoted := byte(0)
	for i := 0; i < len(value); i++ {
		b := value[i]
		if quoted != 0 {
			if b == quoted {
				if i+1 < len(value) && value[i+1] == quoted {
					i++
					continue
				}
				quoted = 0
			}
			continue
		}
		if b == '\'' || b == '"' {
			quoted = b
			continue
		}
		if b == ';' || b == '$' || b == '\n' || b == '\r' || i+1 < len(value) && (value[i:i+2] == "--" || value[i:i+2] == "/*") {
			return fmt.Errorf("unsafe token")
		}
	}
	if quoted != 0 {
		return fmt.Errorf("unterminated quote")
	}
	return nil
}
func resourceMapForRender(doc schema.Document) map[string]schema.Resource {
	out := map[string]schema.Resource{}
	for _, r := range doc.Graph.Resources {
		out[r.ID] = r
	}
	return out
}
func isProjectionID(id string, resources map[string]schema.Resource) bool {
	r, ok := resources[id]
	return ok && (r.Kind == schema.KindView || r.Kind == schema.KindMaterializedView)
}
func validateProjectionResource(r schema.Resource, parent string) error {
	s := spec(r)
	if !allowedKeys(s, "type", "not_null", "ordinal") {
		return unsupported(r, "unknown projection spec")
	}
	if _, ok := s["type"].(string); !ok {
		return unsupported(r, "projection type must be a string")
	}
	if _, ok := s["not_null"].(bool); !ok {
		return unsupported(r, "projection not_null must be boolean")
	}
	ordinal, ok := s["ordinal"].(float64)
	if !ok || ordinal < 1 || ordinal != float64(int(ordinal)) {
		return unsupported(r, "projection ordinal must be a positive integer")
	}
	if len(r.Dependencies) != 1 || r.Dependencies[0].Target != parent || r.Dependencies[0].Type != schema.DependencyContains || len(r.Dependencies[0].Extra) > 0 {
		return unsupported(r, "projection dependency must be exactly its parent")
	}
	return nil
}
func sameProjectionSpec(a, b schema.Resource) bool {
	as, bs := spec(a), spec(b)
	return stringValue(as, "type") == stringValue(bs, "type") && boolValue(as, "not_null") == boolValue(bs, "not_null") && numberAsInt(as, "ordinal") == numberAsInt(bs, "ordinal")
}
func projectionSignature(resources map[string]schema.Resource, parent string) (string, error) {
	var children []schema.Resource
	for _, r := range resources {
		if r.Kind == schema.KindColumn && r.Name.Parent == parent {
			if e := validateProjectionResource(r, parent); e != nil {
				return "", e
			}
			children = append(children, r)
		}
	}
	if len(children) == 0 {
		return "", fmt.Errorf("projection %s has no canonical output", parent)
	}
	sort.Slice(children, func(i, j int) bool {
		return numberAsInt(spec(children[i]), "ordinal") < numberAsInt(spec(children[j]), "ordinal")
	})
	parts := make([]string, len(children))
	for i, r := range children {
		if numberAsInt(spec(r), "ordinal") != i+1 {
			return "", unsupported(r, "projection ordinals must be contiguous and unique")
		}
		s := spec(r)
		parts[i] = fmt.Sprintf("%d:%s:%s:%t", numberAsInt(s, "ordinal"), r.Name.Name, stringValue(s, "type"), boolValue(s, "not_null"))
	}
	return strings.Join(parts, "\x00"), nil
}
func allowedKeys(values map[string]any, keys ...string) bool {
	allowed := map[string]bool{}
	for _, key := range keys {
		allowed[key] = true
	}
	for key := range values {
		if !allowed[key] {
			return false
		}
	}
	return true
}

func validateSQLFragment(value string) error {
	value = strings.TrimSpace(value)
	if value == "" {
		return fmt.Errorf("empty fragment")
	}
	var quoted byte
	for i := 0; i < len(value); i++ {
		b := value[i]
		if quoted != 0 {
			if b == quoted {
				if i+1 < len(value) && value[i+1] == quoted {
					i++
					continue
				}
				quoted = 0
			}
			continue
		}
		if b == '\'' || b == '"' {
			quoted = b
			continue
		}
		if b == ';' || b == '$' || i+1 < len(value) && (value[i:i+2] == "--" || value[i:i+2] == "/*") {
			return fmt.Errorf("multiple statements, comments, and dollar quotes are forbidden")
		}
	}
	if quoted != 0 {
		return fmt.Errorf("unterminated quote")
	}
	first := strings.ToUpper(strings.Fields(value)[0])
	switch first {
	case "SELECT", "WITH", "VALUES", "TABLE":
	default:
		return fmt.Errorf("view definition must be one query expression")
	}
	upper := strings.ToUpper(value)
	for _, keyword := range []string{" DROP ", " ALTER ", " CREATE ", " INSERT ", " UPDATE ", " DELETE ", " GRANT ", " REVOKE ", " COPY ", " CALL ", " DO "} {
		if strings.Contains(" "+upper+" ", keyword) {
			return fmt.Errorf("non-query keyword %s is forbidden", strings.TrimSpace(keyword))
		}
	}
	return nil
}
