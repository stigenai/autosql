package source

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"autosql/pkg/schema"
)

type Format string

const (
	FormatSQL       Format = "sql"
	FormatNative    Format = "autosql-json"
	FormatHCLSource Format = "hcl"
)

type Input struct {
	URI    string
	Format Format
	Data   []byte
}

// ConflictError identifies both definitions of a composed resource.
type ConflictError struct {
	ID, First, Second string
}

func (e *ConflictError) Error() string {
	return fmt.Sprintf("conflicting definition for %s: first defined at %s; redefined at %s", e.ID, e.First, e.Second)
}

// Load parses and deterministically composes desired-state sources. It never
// opens a database connection.
func Load(inputs ...Input) (schema.Document, error) {
	return LoadContext(context.Background(), inputs...)
}

// LoadContext is Load with cooperative cancellation during file parsing and
// composition. It does not leave local artifacts behind on cancellation.
func LoadContext(ctx context.Context, inputs ...Input) (schema.Document, error) {
	doc := schema.Document{Version: schema.SchemaVersion}
	byID := map[string]schema.Resource{}
	for _, in := range inputs {
		if err := ctx.Err(); err != nil {
			return doc, err
		}
		var part schema.Document
		var err error
		switch in.Format {
		case FormatNative:
			err = json.Unmarshal(in.Data, &part)
			if err == nil && part.Version != schema.SchemaVersion {
				err = fmt.Errorf("%s: %w: %q", in.URI, schema.ErrUnsupportedVersion, part.Version)
			}
		case FormatSQL:
			part, err = parseSQL(ctx, in.URI, string(in.Data), false)
		case FormatHCLSource:
			part, err = ParseHCLContext(ctx, in.URI, in.Data, nil)
		default:
			err = fmt.Errorf("%s: unsupported schema source format %q", in.URI, in.Format)
		}
		if err != nil {
			return doc, err
		}
		// Carry document-level metadata (e.g. the dialect annotation) so a
		// single inspected source round-trips through plan.Build, which
		// requires document metadata to match. hcl_imports is a parser-internal
		// marker and is not treated as composed source state.
		for k, v := range part.Annotations {
			if k == "hcl_imports" {
				continue
			}
			if doc.Annotations == nil {
				doc.Annotations = map[string]string{}
			}
			doc.Annotations[k] = v
		}
		for _, r := range part.Graph.Resources {
			if r.Source == nil {
				r.Source = &schema.SourceLocation{URI: in.URI}
			}
			if old, ok := byID[r.ID]; ok {
				if !resourceEquivalent(old, r) {
					return doc, &ConflictError{ID: r.ID, First: location(old.Source), Second: location(r.Source)}
				}
				continue
			}
			byID[r.ID] = r
		}
	}
	for _, r := range byID {
		doc.Graph.Resources = append(doc.Graph.Resources, r)
	}
	doc.Normalize()
	if err := doc.Validate(); err != nil {
		return doc, err
	}
	return doc, nil
}

func resourceEquivalent(a, b schema.Resource) bool {
	a.Source, b.Source = nil, nil
	a.Extra, b.Extra = nil, nil
	ab, _ := json.Marshal(a)
	bb, _ := json.Marshal(b)
	return bytes.Equal(ab, bb)
}

func location(p *schema.SourceLocation) string {
	if p == nil {
		return "<unknown>"
	}
	if p.Line == 0 {
		return p.URI
	}
	return fmt.Sprintf("%s:%d:%d", p.URI, p.Line, p.Column)
}

var createHead = regexp.MustCompile(`(?is)^CREATE\s+(?:OR\s+REPLACE\s+)?(?:UNLOGGED\s+)?(MATERIALIZED\s+VIEW|SCHEMA|TABLE|UNIQUE\s+INDEX|INDEX|VIEW|SEQUENCE|DOMAIN|EXTENSION|FUNCTION|PROCEDURE|TRIGGER|ROLE|USER)\s+(?:IF\s+NOT\s+EXISTS\s+)?`)

// ParseSQL parses the supported PostgreSQL desired-state DDL subset.
func ParseSQL(uri, sqlText string) (schema.Document, error) {
	return ParseSQLContext(context.Background(), uri, sqlText)
}

// ParseSQLContext parses a SQL source with cooperative cancellation.
func ParseSQLContext(ctx context.Context, uri, sqlText string) (schema.Document, error) {
	return parseSQL(ctx, uri, sqlText, true)
}

func parseSQL(ctx context.Context, uri, sqlText string, validate bool) (schema.Document, error) {
	statements, err := SplitSQLContext(ctx, uri, sqlText)
	if err != nil {
		return schema.Document{}, err
	}
	doc := schema.Document{Version: schema.SchemaVersion}
	resources := map[string]schema.Resource{}
	for _, st := range statements {
		if err := ctx.Err(); err != nil {
			return doc, err
		}
		clean := strings.TrimSpace(stripComments(st.SQL))
		if clean == "" {
			continue
		}
		m := createHead.FindStringSubmatch(clean)
		if m == nil {
			return doc, fmt.Errorf("%s:%d:%d: unsupported SQL statement", uri, st.Position.Line, st.Position.Column)
		}
		kind := strings.ToUpper(strings.Join(strings.Fields(m[1]), " "))
		rest := strings.TrimSpace(clean[len(m[0]):])
		loc := &schema.SourceLocation{URI: uri, Line: st.Position.Line, Column: st.Position.Column}
		var made []schema.Resource
		switch kind {
		case "SCHEMA":
			q, _, e := takeQName(rest)
			if e != nil {
				err = e
				break
			}
			made = []schema.Resource{newResource(schema.KindSchema, schema.Name{Name: q.Name}, nil, loc, nil)}
		case "TABLE":
			made, err = parseCreateTable(rest, loc)
		case "INDEX", "UNIQUE INDEX":
			made, err = parseCreateIndex(rest, kind == "UNIQUE INDEX", loc)
		case "VIEW", "MATERIALIZED VIEW":
			made, err = parseView(rest, kind == "MATERIALIZED VIEW", loc)
		case "SEQUENCE", "DOMAIN", "EXTENSION", "FUNCTION", "PROCEDURE", "TRIGGER", "ROLE", "USER":
			made, err = parseGeneric(kind, rest, clean, loc)
		}
		if err != nil {
			return doc, fmt.Errorf("%s:%d:%d: %w", uri, st.Position.Line, st.Position.Column, err)
		}
		for _, r := range made {
			if old, exists := resources[r.ID]; exists && !resourceEquivalent(old, r) {
				return doc, &ConflictError{ID: r.ID, First: location(old.Source), Second: location(r.Source)}
			}
			resources[r.ID] = r
		}
	}
	ensureImplicitSchemas(resources)
	for _, r := range resources {
		doc.Graph.Resources = append(doc.Graph.Resources, r)
	}
	doc.Normalize()
	if validate {
		if err := doc.Validate(); err != nil {
			return doc, err
		}
	}
	return doc, nil
}

func ensureImplicitSchemas(resources map[string]schema.Resource) {
	for _, r := range resources {
		if r.Kind == schema.KindSchema || r.Name.Schema == "" {
			continue
		}
		name := schema.Name{Name: r.Name.Schema}
		id := schema.StableID(schema.KindSchema, name)
		if _, ok := resources[id]; !ok {
			resources[id] = newResource(schema.KindSchema, name, nil, r.Source, nil)
		}
	}
}

func ownedName(q qname) schema.Name {
	name := schema.Name{Schema: q.Schema, Name: q.Name}
	if q.Schema != "" {
		name.Parent = schema.StableID(schema.KindSchema, schema.Name{Name: q.Schema})
	}
	return name
}

func schemaDependency(schemaName string) []schema.Dependency {
	if schemaName == "" {
		return nil
	}
	return []schema.Dependency{{Target: schema.StableID(schema.KindSchema, schema.Name{Name: schemaName}), Type: schema.DependencyContains}}
}

type qname struct{ Schema, Name string }

func takeQName(s string) (qname, string, error) {
	parts := []string{}
	for {
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		if s == "" {
			return qname{}, s, errors.New("expected object name")
		}
		var id string
		if s[0] == '"' {
			var b strings.Builder
			i := 1
			for ; i < len(s); i++ {
				if s[i] == '"' {
					if i+1 < len(s) && s[i+1] == '"' {
						b.WriteByte('"')
						i++
						continue
					}
					i++
					break
				}
				b.WriteByte(s[i])
			}
			if i > len(s) || i == len(s) && s[len(s)-1] != '"' {
				return qname{}, s, errors.New("unterminated quoted identifier")
			}
			id = b.String()
			s = s[i:]
		} else {
			i := 0
			for i < len(s) && (s[i] == '_' || s[i] == '$' || unicode.IsLetter(rune(s[i])) || unicode.IsDigit(rune(s[i]))) {
				i++
			}
			if i == 0 {
				return qname{}, s, errors.New("expected identifier")
			}
			id = strings.ToLower(s[:i])
			s = s[i:]
		}
		parts = append(parts, id)
		s = strings.TrimLeftFunc(s, unicode.IsSpace)
		if !strings.HasPrefix(s, ".") {
			break
		}
		s = s[1:]
	}
	if len(parts) > 2 {
		return qname{}, s, errors.New("object names may contain at most schema and name")
	}
	if len(parts) == 1 {
		return qname{Name: parts[0]}, s, nil
	}
	return qname{Schema: parts[0], Name: parts[1]}, s, nil
}

func parseCreateTable(rest string, loc *schema.SourceLocation) ([]schema.Resource, error) {
	q, rest, err := takeQName(rest)
	if err != nil {
		return nil, err
	}
	rest = strings.TrimSpace(rest)
	if len(rest) < 2 || rest[0] != '(' {
		return nil, errors.New("CREATE TABLE requires a parenthesized definition")
	}
	body, end, err := balanced(rest, 0)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(rest[end:]) != "" { /* storage clauses retained on table */
	}
	tn := ownedName(q)
	table := newResource(schema.KindTable, tn, map[string]any{"options": strings.TrimSpace(rest[end:])}, loc, schemaDependency(q.Schema))
	out := []schema.Resource{table}
	columns := map[string]string{}
	items := splitTopLevel(body[1:len(body)-1], ',')
	ordinal := 0
	for _, item := range items {
		item = strings.TrimSpace(item)
		if item == "" {
			continue
		}
		upper := strings.ToUpper(item)
		if strings.HasPrefix(upper, "CONSTRAINT ") || strings.HasPrefix(upper, "PRIMARY KEY") || strings.HasPrefix(upper, "UNIQUE") || strings.HasPrefix(upper, "CHECK") || strings.HasPrefix(upper, "FOREIGN KEY") {
			continue
		}
		ordinal++
		cn, tail, e := takeQName(item)
		if e != nil || cn.Schema != "" {
			return nil, fmt.Errorf("invalid column definition %q", item)
		}
		typeName, attrs := splitColumnType(tail)
		if typeName == "" {
			return nil, fmt.Errorf("column %s has no type", cn.Name)
		}
		nullable := !containsWords(attrs, "NOT NULL")
		spec := map[string]any{"type": normalizeSpace(typeName), "nullable": nullable, "ordinal": ordinal}
		if def := clauseValue(attrs, "DEFAULT", []string{"NOT NULL", "NULL", "PRIMARY KEY", "UNIQUE", "REFERENCES", "CHECK", "GENERATED"}); def != "" {
			spec["default"] = def
		}
		name := schema.Name{Schema: q.Schema, Name: cn.Name, Parent: table.ID}
		col := newResource(schema.KindColumn, name, spec, loc, []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}})
		columns[cn.Name] = col.ID
		out = append(out, col)
		inline := []struct {
			phrase string
			kind   schema.Kind
			suffix string
		}{
			{"PRIMARY KEY", schema.KindPrimaryKey, "pkey"},
			{"UNIQUE", schema.KindUniqueConstraint, "key"},
			{"REFERENCES", schema.KindForeignKey, "fkey"},
			{"CHECK", schema.KindCheckConstraint, "check"},
		}
		for _, candidate := range inline {
			if !containsWords(attrs, candidate.phrase) {
				continue
			}
			constraintName := q.Name + "_" + cn.Name + "_" + candidate.suffix
			out = append(out, newResource(candidate.kind,
				schema.Name{Schema: q.Schema, Name: constraintName, Parent: table.ID},
				map[string]any{"definition": normalizeSpace(candidate.phrase + " " + attrs)}, loc,
				[]schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}, {Target: col.ID, Type: schema.DependencyReferences}}))
		}
	}
	for _, item := range items {
		item = strings.TrimSpace(item)
		upper := strings.ToUpper(item)
		if strings.HasPrefix(upper, "CONSTRAINT ") || strings.HasPrefix(upper, "PRIMARY KEY") || strings.HasPrefix(upper, "UNIQUE") || strings.HasPrefix(upper, "CHECK") || strings.HasPrefix(upper, "FOREIGN KEY") {
			r, e := parseTableConstraint(item, q, table, columns, loc)
			if e != nil {
				return nil, e
			}
			out = append(out, r)
		}
	}
	return out, nil
}

func parseTableConstraint(item string, q qname, table schema.Resource, columns map[string]string, loc *schema.SourceLocation) (schema.Resource, error) {
	work := strings.TrimSpace(item)
	cname := ""
	if strings.HasPrefix(strings.ToUpper(work), "CONSTRAINT ") {
		n, tail, e := takeQName(strings.TrimSpace(work[len("CONSTRAINT"):]))
		if e != nil {
			return schema.Resource{}, e
		}
		cname = n.Name
		work = strings.TrimSpace(tail)
	}
	u := strings.ToUpper(work)
	kind := schema.KindCheckConstraint
	switch {
	case strings.HasPrefix(u, "PRIMARY KEY"):
		kind = schema.KindPrimaryKey
	case strings.HasPrefix(u, "UNIQUE"):
		kind = schema.KindUniqueConstraint
	case strings.HasPrefix(u, "FOREIGN KEY"):
		kind = schema.KindForeignKey
	case strings.HasPrefix(u, "CHECK"):
		kind = schema.KindCheckConstraint
	default:
		return schema.Resource{}, fmt.Errorf("unsupported constraint %q", item)
	}
	if cname == "" {
		cname = string(kind) + "_" + fmt.Sprint(len(columns))
	}
	deps := []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}
	for name, id := range columns {
		if identifierMentioned(work, name) {
			deps = append(deps, schema.Dependency{Target: id, Type: schema.DependencyReferences})
		}
	}
	sort.Slice(deps, func(i, j int) bool { return deps[i].Target < deps[j].Target })
	return newResource(kind, schema.Name{Schema: q.Schema, Name: cname, Parent: table.ID}, map[string]any{"definition": normalizeSpace(work)}, loc, deps), nil
}

func parseCreateIndex(rest string, unique bool, loc *schema.SourceLocation) ([]schema.Resource, error) {
	idx, rest, e := takeQName(rest)
	if e != nil {
		return nil, e
	}
	rest = strings.TrimSpace(rest)
	if !strings.HasPrefix(strings.ToUpper(rest), "ON ") {
		return nil, errors.New("CREATE INDEX requires ON")
	}
	rest = strings.TrimSpace(rest[len("ON "):])
	tbl, tail, e := takeQName(rest)
	if e != nil {
		return nil, e
	}
	if idx.Schema == "" {
		idx.Schema = tbl.Schema
	}
	tableID := schema.StableID(schema.KindTable, ownedName(tbl))
	idxName := ownedName(idx)
	deps := append(schemaDependency(idx.Schema), schema.Dependency{Target: tableID, Type: schema.DependencyReferences})
	r := newResource(schema.KindIndex, idxName, map[string]any{"unique": unique, "definition": normalizeSpace(tail)}, loc, deps)
	return []schema.Resource{r}, nil
}

func parseView(rest string, materialized bool, loc *schema.SourceLocation) ([]schema.Resource, error) {
	q, tail, e := takeQName(rest)
	if e != nil {
		return nil, e
	}
	kind := schema.KindView
	if materialized {
		kind = schema.KindMaterializedView
	}
	return []schema.Resource{newResource(kind, ownedName(q), map[string]any{"definition": normalizeSpace(tail)}, loc, schemaDependency(q.Schema))}, nil
}

func parseGeneric(kind, rest, full string, loc *schema.SourceLocation) ([]schema.Resource, error) {
	q, tail, e := takeQName(rest)
	if e != nil {
		return nil, e
	}
	if (kind == "FUNCTION" || kind == "PROCEDURE") && strings.HasPrefix(strings.TrimSpace(tail), "(") {
		args, _, err := balanced(strings.TrimSpace(tail), 0)
		if err != nil {
			return nil, err
		}
		q.Name += "(" + normalizeArgumentTypes(args[1:len(args)-1]) + ")"
	}
	k := map[string]schema.Kind{"SEQUENCE": schema.KindSequence, "DOMAIN": schema.KindDomain, "EXTENSION": schema.KindExtension, "FUNCTION": schema.KindFunction, "PROCEDURE": schema.KindProcedure, "TRIGGER": schema.KindTrigger, "ROLE": schema.KindRole, "USER": schema.KindRole}[kind]
	name := ownedName(q)
	deps := schemaDependency(q.Schema)
	if k == schema.KindRole {
		name.Parent = ""
		name.Schema = ""
		deps = nil
	}
	return []schema.Resource{newResource(k, name, map[string]any{"definition": normalizeSpace(full)}, loc, deps)}, nil
}

func normalizeArgumentTypes(args string) string {
	if strings.TrimSpace(args) == "" {
		return ""
	}
	parts := splitTopLevel(args, ',')
	for i, p := range parts {
		fields := strings.Fields(strings.TrimSpace(p))
		if len(fields) > 1 {
			mode := strings.ToUpper(fields[0])
			if mode == "IN" || mode == "OUT" || mode == "INOUT" || mode == "VARIADIC" {
				fields = fields[1:]
			}
		}
		// PostgreSQL permits an argument name before its type. This heuristic
		// intentionally preserves multi-word built-in types.
		if len(fields) > 1 && !isTypeKeyword(fields[0]) {
			fields = fields[1:]
		}
		parts[i] = strings.ToLower(strings.Join(fields, " "))
	}
	return strings.Join(parts, ",")
}

func isTypeKeyword(s string) bool {
	switch strings.ToLower(s) {
	case "double", "character", "timestamp", "time", "bit", "numeric", "decimal", "smallint", "integer", "bigint", "real", "boolean", "text", "bytea", "json", "jsonb", "uuid", "date", "interval":
		return true
	default:
		return strings.ContainsAny(s, ".[]")
	}
}

func newResource(kind schema.Kind, name schema.Name, spec map[string]any, loc *schema.SourceLocation, deps []schema.Dependency) schema.Resource {
	if spec == nil {
		spec = map[string]any{}
	}
	raw, _ := json.Marshal(spec)
	return schema.Resource{ID: schema.StableID(kind, name), Kind: kind, Name: name, Spec: raw, Source: loc, Dependencies: deps}
}

func balanced(s string, start int) (string, int, error) {
	depth := 0
	quote := byte(0)
	for i := start; i < len(s); i++ {
		b := s[i]
		if quote != 0 {
			if b == quote {
				if i+1 < len(s) && s[i+1] == quote {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		if b == '\'' || b == '"' {
			quote = b
			continue
		}
		if b == '(' {
			depth++
		}
		if b == ')' {
			depth--
			if depth == 0 {
				return s[start : i+1], i + 1, nil
			}
		}
	}
	return "", 0, errors.New("unbalanced parentheses")
}
func splitTopLevel(s string, sep byte) []string {
	var out []string
	start, depth := 0, 0
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		b := s[i]
		if quote != 0 {
			if b == quote {
				if i+1 < len(s) && s[i+1] == quote {
					i++
					continue
				}
				quote = 0
			}
			continue
		}
		if b == '\'' || b == '"' {
			quote = b
			continue
		}
		if b == '(' {
			depth++
		}
		if b == ')' {
			depth--
		}
		if b == sep && depth == 0 {
			out = append(out, s[start:i])
			start = i + 1
		}
	}
	return append(out, s[start:])
}

var columnClause = regexp.MustCompile(`(?i)\s+(NOT\s+NULL|NULL|DEFAULT|PRIMARY\s+KEY|UNIQUE|REFERENCES|CHECK|GENERATED)\b`)

func splitColumnType(s string) (string, string) {
	s = strings.TrimSpace(s)
	m := columnClause.FindStringIndex(s)
	if m == nil {
		return s, ""
	}
	return strings.TrimSpace(s[:m[0]]), strings.TrimSpace(s[m[0]:])
}
func containsWords(s, w string) bool {
	return regexp.MustCompile(`(?i)(^|\s)`+strings.ReplaceAll(w, " ", `\s+`)+`(\s|$)`).FindStringIndex(s) != nil
}
func clauseValue(s, key string, stops []string) string {
	u := strings.ToUpper(s)
	p := strings.Index(u, key)
	if p < 0 {
		return ""
	}
	v := strings.TrimSpace(s[p+len(key):])
	vu := strings.ToUpper(v)
	end := len(v)
	for _, stop := range stops {
		if q := strings.Index(vu, " "+stop); q >= 0 && q < end {
			end = q
		}
	}
	return strings.TrimSpace(v[:end])
}
func identifierMentioned(s, name string) bool {
	return regexp.MustCompile(`(?i)(^|[^a-zA-Z0-9_$])`+regexp.QuoteMeta(name)+`([^a-zA-Z0-9_$]|$)`).FindStringIndex(s) != nil
}
func normalizeSpace(s string) string { return strings.Join(strings.Fields(s), " ") }
func stripComments(s string) string {
	var b strings.Builder
	line, block := false, 0
	quote := byte(0)
	for i := 0; i < len(s); i++ {
		c := s[i]
		if line {
			if c == '\n' {
				line = false
				b.WriteByte(' ')
			}
			continue
		}
		if block > 0 {
			if i+1 < len(s) && s[i:i+2] == "/*" {
				block++
				i++
				continue
			}
			if i+1 < len(s) && s[i:i+2] == "*/" {
				block--
				i++
				continue
			}
			continue
		}
		if quote != 0 {
			b.WriteByte(c)
			if c == quote {
				if i+1 < len(s) && s[i+1] == quote {
					b.WriteByte(s[i+1])
					i++
				} else {
					quote = 0
				}
			}
			continue
		}
		if c == '\'' || c == '"' {
			quote = c
			b.WriteByte(c)
			continue
		}
		if i+1 < len(s) && s[i:i+2] == "--" {
			line = true
			i++
			continue
		}
		if i+1 < len(s) && s[i:i+2] == "/*" {
			block = 1
			i++
			continue
		}
		b.WriteByte(c)
	}
	return b.String()
}
