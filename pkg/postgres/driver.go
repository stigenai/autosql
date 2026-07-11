// Package postgres implements PostgreSQL live database inspection.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
)

const version = "0.1.0"

// Driver inspects PostgreSQL databases. It is safe for concurrent use.
type Driver struct{}

// New returns a PostgreSQL inspection driver.
func New() *Driver { return &Driver{} }

// Options controls the subset and capability level inspected by InspectURL.
// Include and Exclude use path.Match-style patterns against names such as
// "public.users" and "table:public.users". Advanced enables role and grant
// inspection, which can require broader catalog privileges.
type Options struct {
	Schemas  []string
	Include  []string
	Exclude  []string
	Advanced bool
}

// InspectURL is the convenient public API for inspecting a live database.
func InspectURL(ctx context.Context, url string, opts Options) (schema.Document, error) {
	request := plugin.InspectRequest{
		URL:     url,
		Schemas: append([]string(nil), opts.Schemas...),
		Options: map[string]string{
			"include": strings.Join(opts.Include, ","),
			"exclude": strings.Join(opts.Exclude, ","),
		},
	}
	if opts.Advanced {
		request.Options["roles"] = "true"
		request.Options["grants"] = "true"
	}
	return inspect(ctx, request)
}

func (*Driver) Info() plugin.Info {
	kinds := []schema.Kind{
		schema.KindSchema, schema.KindExtension, schema.KindEnum,
		schema.KindDomain, schema.KindComposite, schema.KindSequence, schema.KindTable,
		schema.KindColumn, schema.KindPrimaryKey, schema.KindUniqueConstraint,
		schema.KindCheckConstraint, schema.KindForeignKey, schema.KindIndex, schema.KindView,
		schema.KindMaterializedView, schema.KindFunction, schema.KindProcedure,
		schema.KindTrigger, schema.KindPolicy, schema.KindRole, schema.KindGrant,
	}
	caps := make([]plugin.Capability, 0, len(kinds))
	// Managed is restricted to the lifecycle-complete transition matrix.
	managed := map[schema.Kind]bool{schema.KindSchema: true, schema.KindTable: true, schema.KindView: true, schema.KindMaterializedView: true}
	for _, kind := range kinds {
		mode := plugin.ReadOnly
		if managed[kind] {
			mode = plugin.Managed
		}
		caps = append(caps, plugin.Capability{Kind: kind, Mode: mode})
	}
	return plugin.Info{Name: "postgres", Version: version, APIVersion: plugin.HostAPIVersion, Capabilities: caps}
}

func (d *Driver) Inspect(ctx context.Context, req plugin.InspectRequest) (schema.Document, error) {
	return inspect(ctx, req)
}

func (*Driver) Normalize(_ context.Context, doc schema.Document) (schema.Document, error) {
	if dialect := doc.Annotations["dialect"]; dialect != "" && dialect != "postgresql" {
		return schema.Document{}, fmt.Errorf("normalize PostgreSQL schema: dialect %q is not PostgreSQL", dialect)
	}
	if doc.Annotations == nil {
		doc.Annotations = map[string]string{}
	}
	doc.Annotations["dialect"] = "postgresql"
	raw, err := json.Marshal(doc)
	if err != nil {
		return schema.Document{}, fmt.Errorf("normalize PostgreSQL schema: %w", err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return schema.Document{}, fmt.Errorf("normalize PostgreSQL schema: %w", err)
	}
	for idx := range doc.Graph.Resources {
		r := &doc.Graph.Resources[idx]
		// Serialized/public annotations are not trusted provenance.
		delete(r.Annotations, "autosql.io/generated-name")
		delete(r.Annotations, "autosql.io/name-origin")
		var spec map[string]any
		if len(r.Spec) > 0 && json.Unmarshal(r.Spec, &spec) == nil {
			normalizePostgresSpecForKind(r.Kind, spec)
			normalized, e := json.Marshal(spec)
			if e != nil {
				return schema.Document{}, e
			}
			r.Spec = normalized
		}
	}
	augmentProjectionColumns(&doc)
	doc.Normalize()
	if err := doc.Validate(); err != nil {
		return schema.Document{}, fmt.Errorf("normalize PostgreSQL schema: %w", err)
	}
	return doc, nil
}

var simpleViewFrom = regexp.MustCompile(`(?i)^SELECT\s+(.+)\s+FROM\s+([a-z_][a-z0-9_]*)\.([a-z_][a-z0-9_]*)$`)
var simpleLiteralView = regexp.MustCompile(`(?i)^SELECT\s+(.+)\s+AS\s+([a-z_][a-z0-9_]*)$`)

func augmentProjectionColumns(doc *schema.Document) {
	children := map[string]bool{}
	tables := map[string]schema.Resource{}
	columns := map[string][]schema.Resource{}
	for _, r := range doc.Graph.Resources {
		if r.Kind == schema.KindColumn {
			children[r.Name.Parent] = true
			columns[r.Name.Parent] = append(columns[r.Name.Parent], r)
		}
		if r.Kind == schema.KindTable {
			tables[r.Name.Schema+"."+r.Name.Name] = r
		}
	}
	var added []schema.Resource
	for idx := range doc.Graph.Resources {
		r := &doc.Graph.Resources[idx]
		if (r.Kind != schema.KindView && r.Kind != schema.KindMaterializedView) || children[r.ID] {
			continue
		}
		s := specMap(r.Spec)
		definition, _ := s["definition"].(string)
		var projections []schema.Resource
		if match := simpleViewFrom.FindStringSubmatch(definition); match != nil {
			table, ok := tables[match[2]+"."+match[3]]
			if !ok {
				continue
			}
			items := strings.Split(match[1], ",")
			if strings.TrimSpace(match[1]) == "*" {
				items = nil
				tableColumns := append([]schema.Resource(nil), columns[table.ID]...)
				sort.Slice(tableColumns, func(i, j int) bool {
					return numberAsInt(specMap(tableColumns[i].Spec), "ordinal") < numberAsInt(specMap(tableColumns[j].Spec), "ordinal")
				})
				for _, column := range tableColumns {
					items = append(items, column.Name.Name)
				}
				names := make([]string, len(items))
				for i, name := range items {
					names[i] = strings.TrimSpace(name)
				}
				s["definition"] = "SELECT " + strings.Join(names, ", ") + " FROM " + match[2] + "." + match[3]
				r.Spec, _ = json.Marshal(s)
			}
			for _, item := range items {
				name := strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(item), table.Name.Name+"."))
				for _, column := range columns[table.ID] {
					if column.Name.Name == name {
						column.Name.Parent = r.ID
						column.ID = schema.StableID(column.Kind, column.Name)
						column.Dependencies = []schema.Dependency{{Target: r.ID, Type: schema.DependencyContains}}
						cs := specMap(column.Spec)
						cs["not_null"] = false
						cs["ordinal"] = len(projections) + 1
						delete(cs, "default")
						delete(cs, "identity")
						delete(cs, "generated")
						column.Spec, _ = json.Marshal(cs)
						projections = append(projections, column)
					}
				}
			}
		}
		if match := simpleLiteralView.FindStringSubmatch(definition); match != nil {
			expr, name := strings.TrimSpace(match[1]), match[2]
			typ := ""
			if _, err := strconv.Atoi(expr); err == nil {
				typ = "integer"
			} else if len(expr) >= 2 && expr[0] == '\'' && expr[len(expr)-1] == '\'' {
				typ = "text"
				s["definition"] = "SELECT " + expr + "::text AS " + name
				r.Spec, _ = json.Marshal(s)
			}
			if typ != "" {
				column := schema.Resource{Kind: schema.KindColumn, Name: schema.Name{Schema: r.Name.Schema, Name: name, Parent: r.ID}, Dependencies: []schema.Dependency{{Target: r.ID, Type: schema.DependencyContains}}, Spec: json.RawMessage(`{"not_null":false,"ordinal":1,"type":"` + typ + `"}`)}
				column.ID = schema.StableID(column.Kind, column.Name)
				projections = append(projections, column)
			}
		}
		added = append(added, projections...)
	}
	doc.Graph.Resources = append(doc.Graph.Resources, added...)
}
func specMap(raw json.RawMessage) map[string]any {
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return out
}
func numberAsInt(values map[string]any, key string) int {
	if value, ok := values[key].(float64); ok {
		return int(value)
	}
	return 0
}

func normalizePostgresSpecForKind(kind schema.Kind, spec map[string]any) {
	normalizePostgresSpec(spec)
	if kind == schema.KindColumn {
		if nullable, ok := spec["nullable"].(bool); ok {
			spec["not_null"] = !nullable
			delete(spec, "nullable")
		}
		if position, ok := spec["position"]; ok {
			spec["ordinal"] = position
			delete(spec, "position")
		}
	}
	if kind == schema.KindTable {
		if options, ok := spec["options"].(string); ok {
			if strings.TrimSpace(options) != "" {
				return
			}
			delete(spec, "options")
			spec["partitioned"] = false
			spec["persistence"] = "p"
			spec["row_security"] = false
			spec["force_row_security"] = false
		}
	}
	if kind == schema.KindView || kind == schema.KindMaterializedView {
		if definition, ok := spec["definition"].(string); ok {
			definition = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(definition), ";"))
			if len(definition) >= 2 && strings.EqualFold(definition[:2], "AS") && (len(definition) == 2 || definition[2] == ' ') {
				definition = strings.TrimSpace(definition[2:])
			}
			spec["definition"] = normalizeSQLSpace(definition)
			if match := simpleViewFrom.FindStringSubmatch(spec["definition"].(string)); match != nil {
				items := strings.Split(match[1], ",")
				for i, item := range items {
					items[i] = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(item), match[3]+"."))
				}
				spec["definition"] = "SELECT " + strings.Join(items, ", ") + " FROM " + match[2] + "." + match[3]
			}
		}
	}
}

func normalizePostgresSpec(spec map[string]any) {
	for _, key := range []string{"type", "base_type"} {
		if value, ok := spec[key].(string); ok {
			spec[key] = postgresTypeAlias(value)
		}
	}
	if value, ok := spec["default"].(string); ok {
		if normalized := postgresDefault(value); strings.EqualFold(normalized, "NULL") {
			delete(spec, "default")
		} else {
			spec["default"] = normalized
		}
	}
	if value, ok := spec["definition"].(string); ok {
		spec["definition"] = normalizeSQLSpace(value)
	}
	// Composite attributes are an established PostgreSQL spec shape. Do not
	// recurse through arbitrary unknown objects: their semantics are opaque.
	if attributes, ok := spec["attributes"].([]any); ok {
		for _, attribute := range attributes {
			if object, ok := attribute.(map[string]any); ok {
				if value, ok := object["type"].(string); ok {
					object["type"] = postgresTypeAlias(value)
				}
				if value, ok := object["default"].(string); ok {
					if normalized := postgresDefault(value); strings.EqualFold(normalized, "NULL") {
						delete(object, "default")
					} else {
						object["default"] = normalized
					}
				}
			}
		}
	}
}
func postgresTypeAlias(value string) string {
	original := strings.TrimSpace(value)
	// Quoted identifiers and user-defined names are case-sensitive. Only fold
	// names from PostgreSQL's documented built-in alias set.
	if strings.Contains(original, `"`) {
		return original
	}
	s := strings.ToLower(original)
	s = strings.TrimPrefix(s, "pg_catalog.")
	array := ""
	for strings.HasSuffix(s, "[]") {
		array += "[]"
		s = strings.TrimSpace(strings.TrimSuffix(s, "[]"))
	}
	suffix := ""
	if i := strings.IndexByte(s, '('); i >= 0 && strings.HasSuffix(s, ")") {
		suffix = s[i:]
		s = strings.TrimSpace(s[:i])
	}
	aliases := map[string]string{
		"int2": "smallint", "smallint": "smallint", "int4": "integer", "int": "integer", "integer": "integer",
		"int8": "bigint", "bigint": "bigint", "float4": "real", "real": "real", "float8": "double precision",
		"double precision": "double precision", "bool": "boolean", "boolean": "boolean", "varchar": "character varying",
		"character varying": "character varying", "timestamp without time zone": "timestamp", "timestamp": "timestamp",
		"timestamp with time zone": "timestamptz", "timestamptz": "timestamptz",
	}
	if normalized, ok := aliases[s]; ok {
		return normalized + suffix + array
	}
	return original
}
func postgresDefault(value string) string {
	s := normalizeSQLSpace(value)
	for strings.HasPrefix(s, "(") && strings.HasSuffix(s, ")") && balancedOuter(s) {
		s = strings.TrimSpace(s[1 : len(s)-1])
	}
	lower := strings.ToLower(s)
	if lower == "now()" || lower == "transaction_timestamp()" || lower == "current_timestamp" || lower == "current_timestamp()" {
		return "CURRENT_TIMESTAMP"
	}
	if strings.HasPrefix(lower, "current_timestamp(") && strings.HasSuffix(lower, ")") {
		return "CURRENT_TIMESTAMP" + lower[len("current_timestamp"):]
	}
	if lower == "null" || strings.HasPrefix(lower, "null::") {
		return "NULL"
	}
	for _, cast := range []string{"::character varying", "::varchar", "::text", "::integer", "::bigint", "::boolean"} {
		if strings.HasSuffix(strings.ToLower(s), cast) {
			base := strings.TrimSpace(s[:len(s)-len(cast)])
			if strings.HasPrefix(base, "'") || base == "true" || base == "false" || base == "NULL" || base == "null" {
				return base
			}
		}
	}
	return s
}
func balancedOuter(s string) bool {
	depth := 0
	quoted := false
	for i, r := range s {
		if r == '\'' {
			quoted = !quoted
		}
		if quoted {
			continue
		}
		if r == '(' {
			depth++
		}
		if r == ')' {
			depth--
			if depth == 0 && i < len(s)-1 {
				return false
			}
		}
	}
	return depth == 0
}
func normalizeSQLSpace(s string) string {
	var out strings.Builder
	space := false
	quote := rune(0)
	for _, r := range strings.TrimSpace(s) {
		if quote != 0 {
			out.WriteRune(r)
			if r == quote {
				quote = 0
			}
			continue
		}
		if r == '\'' || r == '"' {
			if space && out.Len() > 0 {
				out.WriteByte(' ')
			}
			space = false
			quote = r
			out.WriteRune(r)
			continue
		}
		if r == ' ' || r == '\n' || r == '\t' || r == '\r' {
			space = true
			continue
		}
		if space && out.Len() > 0 {
			out.WriteByte(' ')
		}
		space = false
		out.WriteRune(r)
	}
	return out.String()
}
func enabled(options map[string]string, key string, defaultValue bool) bool {
	v, ok := options[key]
	if !ok {
		return defaultValue
	}
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "1", "true", "yes", "on", "enabled":
		return true
	case "0", "false", "no", "off", "disabled":
		return false
	default:
		return defaultValue
	}
}

var ErrPermission = errors.New("PostgreSQL inspection permission denied")

// PermissionError describes an object that the connected role cannot inspect.
type PermissionError struct {
	Resource  string
	Privilege string
	Cause     error
}

func (e *PermissionError) Error() string {
	return fmt.Sprintf("%v: cannot inspect %s; grant %s to the inspection role", ErrPermission, e.Resource, e.Privilege)
}
func (e *PermissionError) Unwrap() error        { return e.Cause }
func (e *PermissionError) Is(target error) bool { return target == ErrPermission }
