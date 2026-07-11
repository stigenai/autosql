// Package postgres implements PostgreSQL live database inspection.
package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	raw, err := json.Marshal(doc)
	if err != nil {
		return schema.Document{}, fmt.Errorf("normalize PostgreSQL schema: %w", err)
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return schema.Document{}, fmt.Errorf("normalize PostgreSQL schema: %w", err)
	}
	parents := map[string]schema.Kind{}
	for _, r := range doc.Graph.Resources {
		parents[r.ID] = r.Kind
	}
	filtered := doc.Graph.Resources[:0]
	for _, r := range doc.Graph.Resources {
		if r.Kind == schema.KindColumn && (parents[r.Name.Parent] == schema.KindView || parents[r.Name.Parent] == schema.KindMaterializedView) {
			continue
		}
		filtered = append(filtered, r)
	}
	doc.Graph.Resources = filtered
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
	doc.Normalize()
	if err := doc.Validate(); err != nil {
		return schema.Document{}, fmt.Errorf("normalize PostgreSQL schema: %w", err)
	}
	return doc, nil
}

func normalizePostgresSpecForKind(kind schema.Kind, spec map[string]any) {
	normalizePostgresSpec(spec)
	if kind == schema.KindColumn {
		if nullable, ok := spec["nullable"].(bool); ok {
			spec["not_null"] = !nullable
			delete(spec, "nullable")
		}
		delete(spec, "position")
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
