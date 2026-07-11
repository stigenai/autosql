package postgres

import (
	"context"
	"encoding/json"
	"fmt"
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
	if enabled(request.Options, "concurrent_indexes", false) {
		return nil, fmt.Errorf("render PostgreSQL changes: %w: nontransactional execution is unavailable until phase-aware guarded apply", plugin.ErrUnsupported)
	}
	resources := map[string]schema.Resource{}
	for _, doc := range []schema.Document{request.Current, request.Desired} {
		for _, r := range doc.Graph.Resources {
			resources[r.ID] = r
		}
	}
	var output []plugin.Statement
	for _, change := range request.Changes.Changes {
		statements, err := renderChange(change, resources, request.Options)
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
		if err := plugin.RequireManaged(New().Info(), r.Kind); err != nil {
			return nil, err
		}
	}
	if parentOnlyRename || projectionChild {
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
		def, e := columnDefinition(r)
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
func columnDefinition(r schema.Resource) (string, error) {
	s := spec(r)
	t := stringValue(s, "type")
	if t == "" {
		return "", unsupported(r, "column type")
	}
	q := t
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
	if match := simpleViewFrom.FindStringSubmatch(definition); match != nil {
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
		for _, item := range strings.Split(match[1], ",") {
			name := strings.TrimSpace(item)
			if dot := strings.LastIndex(name, "."); dot >= 0 {
				name = name[dot+1:]
			}
			for _, r := range resources {
				if r.Kind == schema.KindColumn && r.Name.Parent == table.ID && r.Name.Name == name {
					expected[name] = stringValue(spec(r), "type")
				}
			}
		}
	} else if match := simpleLiteralView.FindStringSubmatch(definition); match != nil {
		expr, name := strings.TrimSpace(match[1]), match[2]
		if _, e := strconv.Atoi(expr); e == nil {
			expected[name] = "integer"
		} else if strings.HasSuffix(expr, "::text") && len(strings.TrimSuffix(expr, "::text")) >= 2 && strings.TrimSuffix(expr, "::text")[0] == '\'' {
			expected[name] = "text"
		}
	}
	if len(expected) != len(children) {
		return fmt.Errorf("projection count mismatch")
	}
	for _, child := range children {
		if expected[child.Name.Name] == "" || expected[child.Name.Name] != stringValue(spec(child), "type") || boolValue(spec(child), "not_null") {
			return fmt.Errorf("projection %s mismatch", child.Name.Name)
		}
	}
	return nil
}
func validateManagedMetadata(r schema.Resource) error {
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
