// Package postgres implements PostgreSQL live database inspection.
package postgres

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"
	"github.com/jackc/pgx/v5"
	"google.golang.org/protobuf/reflect/protoreflect"
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

// InspectConn inspects through the supplied session, preserving session locks.
func InspectConn(ctx context.Context, conn *pgx.Conn, opts Options) (schema.Document, error) {
	request := inspectRequest(opts)
	return inspectConn(ctx, conn, request)
}

// InspectTx inspects the schema visible inside an existing transaction. It is
// used to prove an exact post-mutation fingerprint before DDL and its durable
// revision evidence are committed together.
func InspectTx(ctx context.Context, tx pgx.Tx, opts Options) (schema.Document, error) {
	if tx == nil {
		return schema.Document{}, errors.New("inspect PostgreSQL transaction: transaction is required")
	}
	return inspectSnapshot(ctx, tx, inspectRequest(opts))
}

func inspectRequest(opts Options) plugin.InspectRequest {
	request := plugin.InspectRequest{Schemas: append([]string(nil), opts.Schemas...), Options: map[string]string{"include": strings.Join(opts.Include, ","), "exclude": strings.Join(opts.Exclude, ",")}}
	if opts.Advanced {
		request.Options["roles"] = "true"
		request.Options["grants"] = "true"
	}
	return request
}

func (*Driver) Info() plugin.Info {
	kinds := []schema.Kind{
		schema.KindSchema, schema.KindExtension, schema.KindEnum,
		schema.KindDomain, schema.KindComposite, schema.KindSequence, schema.KindTable,
		schema.KindColumn, schema.KindPrimaryKey, schema.KindUniqueConstraint,
		schema.KindCheckConstraint, schema.KindForeignKey, schema.KindIndex, schema.KindView,
		schema.KindMaterializedView, schema.KindFunction, schema.KindProcedure,
		schema.KindTrigger, schema.KindPolicy, schema.KindRole, schema.KindGrant,
		schema.KindMembership, schema.KindDefaultPrivilege,
	}
	caps := make([]plugin.Capability, 0, len(kinds))
	all := []schema.Operation{schema.OperationCreate, schema.OperationAlter, schema.OperationDrop, schema.OperationRename}
	profiles := map[schema.Kind]plugin.Capability{
		schema.KindSchema:           {Kind: schema.KindSchema, Mode: plugin.Managed, Operations: all, Features: []string{"namespace.lifecycle", "owner.lifecycle"}},
		schema.KindExtension:        {Kind: schema.KindExtension, Mode: plugin.Managed, Operations: []schema.Operation{schema.OperationCreate, schema.OperationAlter, schema.OperationDrop, schema.OperationRename}, Features: []string{"extension.lifecycle", "extension.allowlist", "extension.exact_version", "extension.schema_policy", "extension.trust_policy"}},
		schema.KindComposite:        {Kind: schema.KindComposite, Mode: plugin.Managed, Operations: all, Features: []string{"composite.lifecycle", "composite.ordered_attributes", "composite.attribute_add_drop", "composite.attribute_rename", "composite.attribute_type_change", "composite.exact_type_dependencies"}},
		schema.KindEnum:             {Kind: schema.KindEnum, Mode: plugin.Managed, Operations: all, Features: []string{"enum.lifecycle", "enum.append_values"}},
		schema.KindDomain:           {Kind: schema.KindDomain, Mode: plugin.Managed, Operations: all, Features: []string{"domain.lifecycle", "domain.core_base_type", "domain.literal_check", "owner.lifecycle"}},
		schema.KindSequence:         {Kind: schema.KindSequence, Mode: plugin.Managed, Operations: all, Features: []string{"sequence.lifecycle", "sequence.options"}},
		schema.KindTable:            {Kind: schema.KindTable, Mode: plugin.Managed, Operations: all, Features: []string{"table.permanent", "table.partitioned.range", "table.partitioned.list", "table.partitioned.hash", "table.partition", "table.rls", "table.child_columns"}},
		schema.KindColumn:           {Kind: schema.KindColumn, Mode: plugin.Managed, Operations: all, Features: []string{"column.type_safe_casts", "column.default", "column.not_null", "column.ordinal_metadata", "column.identity_create", "column.generated_external_routines", "column.generated_stored_create"}},
		schema.KindPrimaryKey:       {Kind: schema.KindPrimaryKey, Mode: plugin.Managed, Operations: all, Features: []string{"constraint.primary_key", "constraint.lifecycle", "alter.explicit_rebuild"}},
		schema.KindUniqueConstraint: {Kind: schema.KindUniqueConstraint, Mode: plugin.Managed, Operations: all, Features: []string{"constraint.unique", "constraint.lifecycle", "alter.explicit_rebuild"}},
		schema.KindCheckConstraint:  {Kind: schema.KindCheckConstraint, Mode: plugin.Managed, Operations: all, Features: []string{"constraint.check", "constraint.lifecycle", "alter.explicit_rebuild"}},
		schema.KindForeignKey:       {Kind: schema.KindForeignKey, Mode: plugin.Managed, Operations: all, Features: []string{"constraint.foreign_key", "constraint.lifecycle", "alter.explicit_rebuild"}},
		schema.KindIndex:            {Kind: schema.KindIndex, Mode: plugin.Managed, Operations: all, Features: []string{"index.lifecycle", "index.expression", "index.partial", "index.include", "index.operator_class", "index.network_inet_ops", "index.storage_parameters", "index.tablespace", "alter.explicit_rebuild"}},
		schema.KindFunction:         {Kind: schema.KindFunction, Mode: plugin.Managed, Operations: all, Features: []string{"function.lifecycle", "routine.reviewed_source", "routine.overloads", "routine.sql", "routine.plpgsql", "routine.schema_bound_signature", "routine.fixed_runtime_search_path"}},
		schema.KindProcedure:        {Kind: schema.KindProcedure, Mode: plugin.Managed, Operations: all, Features: []string{"procedure.lifecycle", "procedure.transaction_control_guard", "routine.reviewed_source", "routine.overloads", "routine.sql", "routine.plpgsql", "routine.schema_bound_signature", "routine.fixed_runtime_search_path"}},
		schema.KindTrigger:          {Kind: schema.KindTrigger, Mode: plugin.Managed, Operations: all, Features: []string{"trigger.lifecycle", "trigger.enablement", "trigger.constraint", "trigger.transition_tables", "trigger.when", "trigger.schema_bound_target", "trigger.schema_bound_function"}},
		schema.KindPolicy:           {Kind: schema.KindPolicy, Mode: plugin.Managed, Operations: all, Features: []string{"policy.lifecycle", "policy.permissive_restrictive", "policy.exact_dependencies", "policy.deny_first_rls"}},
		schema.KindRole:             {Kind: schema.KindRole, Mode: plugin.Managed, Operations: all, Features: []string{"role.lifecycle", "role.external_password", "role.protected", "role.reassign_owned"}},
		schema.KindMembership:       {Kind: schema.KindMembership, Mode: plugin.Managed, Operations: all, Features: []string{"membership.lifecycle", "membership.admin_guard", "membership.pg16_options", "membership.cycle_guard"}},
		schema.KindGrant:            {Kind: schema.KindGrant, Mode: plugin.Managed, Operations: []schema.Operation{schema.OperationCreate, schema.OperationAlter, schema.OperationDrop}, Features: []string{"grant.lifecycle", "grant.option", "grant.partial_revoke", "grant.exact_dependencies"}},
		schema.KindDefaultPrivilege: {Kind: schema.KindDefaultPrivilege, Mode: plugin.Managed, Operations: []schema.Operation{schema.OperationCreate, schema.OperationAlter, schema.OperationDrop}, Features: []string{"default_privilege.lifecycle", "default_privilege.future_objects", "default_privilege.exact_dependencies"}},
		schema.KindView:             {Kind: schema.KindView, Mode: plugin.Managed, Operations: all, Features: []string{"view.provable_projection", "view.ast_dependency_graph", "view.captured_projection", "view.projection_type_dependencies", "view.schema_bound_query"}},
		schema.KindMaterializedView: {Kind: schema.KindMaterializedView, Mode: plugin.Managed, Operations: all, Features: []string{"materialized_view.provable_projection", "materialized_view.ast_dependency_graph", "materialized_view.captured_projection", "materialized_view.projection_type_dependencies", "materialized_view.schema_bound_query", "alter.explicit_rebuild"}},
	}
	for _, kind := range kinds {
		if capability, ok := profiles[kind]; ok {
			capability.Features = append(capability.Features, "metadata.comment")
			caps = append(caps, capability)
		} else {
			caps = append(caps, plugin.Capability{Kind: kind, Mode: plugin.ReadOnly})
		}
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
		if len(r.Spec) > 0 {
			spec := specMap(r.Spec)
			normalizePostgresSpecForKind(r.Kind, spec)
			normalized, e := json.Marshal(spec)
			if e != nil {
				return schema.Document{}, e
			}
			r.Spec = normalized
		}
	}
	if err := canonicalizeUsedTypes(&doc); err != nil {
		return schema.Document{}, fmt.Errorf("normalize PostgreSQL schema: %w", err)
	}
	if err := canonicalizeConstraintIndexTypeDependencies(&doc); err != nil {
		return schema.Document{}, fmt.Errorf("normalize PostgreSQL schema: %w", err)
	}
	if err := canonicalizeConstraintIndexBindings(&doc); err != nil {
		return schema.Document{}, fmt.Errorf("normalize PostgreSQL schema: %w", err)
	}
	if err := canonicalizeViewBindings(&doc); err != nil {
		return schema.Document{}, fmt.Errorf("normalize PostgreSQL schema: %w", err)
	}
	if err := canonicalizeRoutineBindings(&doc); err != nil {
		return schema.Document{}, fmt.Errorf("normalize PostgreSQL schema: %w", err)
	}
	if err := canonicalizePartitionTables(&doc); err != nil {
		return schema.Document{}, fmt.Errorf("normalize PostgreSQL schema: %w", err)
	}
	if err := canonicalizeColumnOrdinals(&doc); err != nil {
		return schema.Document{}, fmt.Errorf("normalize PostgreSQL schema: %w", err)
	}
	augmentProjectionColumns(&doc)
	canonicalizeTriggerDefinitions(&doc)
	doc.Normalize()
	if err := doc.Validate(); err != nil {
		return schema.Document{}, fmt.Errorf("normalize PostgreSQL schema: %w", err)
	}
	return doc, nil
}

func canonicalizeTriggerDefinitions(doc *schema.Document) {
	resources := make(map[string]schema.Resource, len(doc.Graph.Resources))
	for _, resource := range doc.Graph.Resources {
		resources[resource.ID] = resource
	}
	for index := range doc.Graph.Resources {
		resource := &doc.Graph.Resources[index]
		if resource.Kind != schema.KindTrigger {
			continue
		}
		parsed, err := parseTriggerDefinition(*resource, resources)
		if err != nil {
			continue
		}
		values := specMap(resource.Spec)
		values["definition"] = parsed.SQL
		normalized, err := json.Marshal(values)
		if err != nil {
			continue
		}
		resource.Spec = normalized
		resources[resource.ID] = *resource
	}
}

func canonicalizePartitionTables(doc *schema.Document) error {
	resources := make(map[string]schema.Resource, len(doc.Graph.Resources))
	columns := map[string][]schema.Resource{}
	for _, resource := range doc.Graph.Resources {
		resources[resource.ID] = resource
		if resource.Kind == schema.KindColumn {
			columns[resource.Name.Parent] = append(columns[resource.Name.Parent], resource)
		}
	}
	for index := range doc.Graph.Resources {
		table := &doc.Graph.Resources[index]
		if table.Kind != schema.KindTable {
			continue
		}
		values := specMap(table.Spec)
		parentID := stringValue(values, "partition_of")
		if parentID == "" {
			if boolValue(values, "partitioned") {
				for _, column := range columns[table.ID] {
					for _, dependency := range column.Dependencies {
						if dependency.Type != schema.DependencyContains {
							appendUniqueDependency(&table.Dependencies, dependency)
						}
					}
				}
			}
			continue
		}
		parent, ok := resources[parentID]
		if !ok || parent.Kind != schema.KindTable || !boolValue(spec(parent), "partitioned") {
			return fmt.Errorf("partition %s references a missing or nonpartitioned parent", table.Name.String())
		}
		appendUniqueDependency(&table.Dependencies, schema.Dependency{Target: parentID, Type: schema.DependencyReferences})
		if len(columns[table.ID]) != 0 {
			continue
		}
		parentColumns := append([]schema.Resource(nil), columns[parentID]...)
		sort.Slice(parentColumns, func(i, j int) bool {
			return numberAsInt(spec(parentColumns[i]), "ordinal") < numberAsInt(spec(parentColumns[j]), "ordinal")
		})
		for _, sourceColumn := range parentColumns {
			clone := sourceColumn
			clone.Name = schema.Name{Schema: table.Name.Schema, Parent: table.ID, Name: sourceColumn.Name.Name}
			clone.ID = schema.StableID(schema.KindColumn, clone.Name)
			clone.Dependencies = []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}
			for _, dependency := range sourceColumn.Dependencies {
				if dependency.Type != schema.DependencyContains {
					appendUniqueDependency(&clone.Dependencies, dependency)
				}
			}
			doc.Graph.Resources = append(doc.Graph.Resources, clone)
		}
	}
	return nil
}

func appendUniqueDependency(dependencies *[]schema.Dependency, candidate schema.Dependency) {
	for _, dependency := range *dependencies {
		if dependency.Target == candidate.Target && dependency.Type == candidate.Type {
			return
		}
	}
	*dependencies = append(*dependencies, candidate)
}

func canonicalizeUsedTypes(doc *schema.Document) error {
	resources := map[string]schema.Resource{}
	for _, r := range doc.Graph.Resources {
		resources[r.ID] = r
	}
	safeIdentifier := regexp.MustCompile(`^[a-z_][a-z0-9_]*$`)
	identifier := func(value string) string {
		if safeIdentifier.MatchString(value) {
			return value
		}
		return `"` + strings.ReplaceAll(value, `"`, `""`) + `"`
	}
	for i := range doc.Graph.Resources {
		r := &doc.Graph.Resources[i]
		if r.Kind != schema.KindColumn {
			continue
		}
		uses := 0
		for _, dep := range r.Dependencies {
			if dep.Type != schema.DependencyUses {
				continue
			}
			uses++
			target, ok := resources[dep.Target]
			if !ok {
				return fmt.Errorf("column %s uses missing type %s", r.Name.String(), dep.Target)
			}
			s := specMap(r.Spec)
			old, _ := s["type"].(string)
			if !typeReferenceMatches(old, r.Name.Schema, target.Name) {
				return fmt.Errorf("column %s type %q does not name uses target %s", r.Name.String(), old, target.Name.String())
			}
			array := ""
			if strings.HasSuffix(old, "[]") {
				array = "[]"
			}
			name := identifier(target.Name.Name)
			if target.Name.Schema != "public" {
				name = identifier(target.Name.Schema) + "." + name
			}
			s["type"] = name + array
			if value, ok := s["default"].(string); ok && stringValue(s, "generated") == "" && (target.Kind == schema.KindEnum || target.Kind == schema.KindDomain || target.Kind == schema.KindComposite) {
				expression, expressionErr := classifyDefaultExpression(value)
				if expressionErr == nil && userDefinedCastMatches(expression, target, r.Name.Schema, array != "") {
					canonical, canonicalErr := qualifyUserDefinedDefaultCast(value, target)
					if canonicalErr != nil {
						return fmt.Errorf("column %s default cast: %w", r.Name.String(), canonicalErr)
					}
					s["default"] = canonical
				}
			}
			r.Spec, _ = json.Marshal(s)
		}
		if uses > 1 {
			return fmt.Errorf("column %s has ambiguous uses targets", r.Name.String())
		}
	}
	return nil
}

func canonicalizeConstraintIndexTypeDependencies(doc *schema.Document) error {
	resources := make(map[string]schema.Resource, len(doc.Graph.Resources))
	for _, resource := range doc.Graph.Resources {
		resources[resource.ID] = resource
	}
	for index := range doc.Graph.Resources {
		resource := &doc.Graph.Resources[index]
		if resource.Kind != schema.KindCheckConstraint && resource.Kind != schema.KindIndex {
			continue
		}
		var expressionRoot protoreflect.Message
		var err error
		if resource.Kind == schema.KindIndex {
			var parsed parsedIndex
			parsed, err = parseIndexDefinition(*resource, resources)
			if err == nil {
				expressionRoot = parsed.statement.ProtoReflect()
			}
		} else {
			var parsed parsedConstraint
			parsed, err = parseConstraintDefinition(*resource, resources)
			if err == nil {
				expressionRoot = parsed.statement.ProtoReflect()
			}
		}
		if err != nil {
			// Normalization historically leaves unsupported constraint/index grammar to
			// the scoped renderer. Preserve that contract when no dependency can
			// be proven from a parser-bound expression.
			continue
		}
		targets, err := expressionTypeDependencies(expressionRoot, resource.Name.Schema, *resource, resources)
		if err != nil {
			return err
		}
		for _, target := range targets {
			exists := false
			for _, dependency := range resource.Dependencies {
				exists = exists || dependency.Target == target && dependency.Type == schema.DependencyUses
			}
			if !exists {
				resource.Dependencies = append(resource.Dependencies, schema.Dependency{Target: target, Type: schema.DependencyUses})
			}
		}
		resources[resource.ID] = *resource
	}
	return nil
}

func canonicalizeConstraintIndexBindings(doc *schema.Document) error {
	resources := make(map[string]schema.Resource, len(doc.Graph.Resources))
	for _, resource := range doc.Graph.Resources {
		resources[resource.ID] = resource
	}
	for index := range doc.Graph.Resources {
		resource := &doc.Graph.Resources[index]
		if resource.Kind != schema.KindCheckConstraint && resource.Kind != schema.KindForeignKey && resource.Kind != schema.KindIndex {
			continue
		}
		if resource.Kind == schema.KindForeignKey && stringValue(spec(*resource), "definition") == "" {
			continue
		}
		hasManagedType := false
		for _, dependency := range resource.Dependencies {
			target := resources[dependency.Target]
			hasManagedType = hasManagedType || dependency.Type == schema.DependencyUses && (target.Kind == schema.KindEnum || target.Kind == schema.KindDomain || target.Kind == schema.KindComposite)
		}
		if !hasManagedType && resource.Kind != schema.KindForeignKey {
			continue
		}
		definition := ""
		if resource.Kind == schema.KindIndex {
			parsed, err := parseIndexDefinition(*resource, resources)
			if err != nil {
				return err
			}
			if err := schemaBindIndexTypeCasts(&parsed, *resource, resources); err != nil {
				return err
			}
			definition = parsed.SQL
		} else {
			_, renderedDefinition, err := renderConstraintCreate(*resource, resources)
			if err != nil {
				return err
			}
			definition = renderedDefinition
		}
		values := specMap(resource.Spec)
		values["definition"] = definition
		normalized, err := json.Marshal(values)
		if err != nil {
			return err
		}
		resource.Spec = normalized
		resources[resource.ID] = *resource
	}
	return nil
}

func canonicalizeColumnOrdinals(doc *schema.Document) error {
	groups := map[string][]*schema.Resource{}
	for i := range doc.Graph.Resources {
		r := &doc.Graph.Resources[i]
		if r.Kind == schema.KindColumn {
			groups[r.Name.Parent] = append(groups[r.Name.Parent], r)
		}
	}
	for _, columns := range groups {
		seen := map[int]bool{}
		maxOrdinal := 0
		for _, column := range columns {
			ordinal := numberAsInt(specMap(column.Spec), "ordinal")
			if ordinal < 1 {
				continue
			}
			if seen[ordinal] {
				return fmt.Errorf("columns under %q require unique positive ordinals", column.Name.Parent)
			}
			seen[ordinal] = true
			if ordinal > maxOrdinal {
				maxOrdinal = ordinal
			}
		}
		for _, column := range columns {
			s := specMap(column.Spec)
			if numberAsInt(s, "ordinal") < 1 {
				maxOrdinal++
				s["ordinal"] = maxOrdinal
				column.Spec, _ = json.Marshal(s)
			}
		}
		sort.SliceStable(columns, func(i, j int) bool {
			return numberAsInt(specMap(columns[i].Spec), "ordinal") < numberAsInt(specMap(columns[j].Spec), "ordinal")
		})
		for i, column := range columns {
			s := specMap(column.Spec)
			s["ordinal"] = i + 1
			column.Spec, _ = json.Marshal(s)
		}
	}
	return nil
}

var simpleViewFrom = regexp.MustCompile(`(?i)^SELECT\s+(.+?)\s+FROM\s+([a-z_][a-z0-9_]*)\.([a-z_][a-z0-9_]*)$`)
var simpleLiteralView = regexp.MustCompile(`(?i)^SELECT\s+(.+)\s+AS\s+([a-z_][a-z0-9_]*)$`)
var directProjection = regexp.MustCompile(`(?i)^(?:\*|(?:[a-z_][a-z0-9_]*\.)?[a-z_][a-z0-9_]*)$`)
var provenLiteral = regexp.MustCompile(`(?i)^(?:[0-9]+|'(?:''|[^'])*'(?:::text)?)$`)

func sqlTokens(definition string) []string {
	var tokens []string
	for i := 0; i < len(definition); {
		if definition[i] == '\'' || definition[i] == '"' {
			quote := definition[i]
			i++
			for i < len(definition) {
				if definition[i] == quote {
					if i+1 < len(definition) && definition[i+1] == quote {
						i += 2
						continue
					}
					i++
					break
				}
				i++
			}
			continue
		}
		if (definition[i] >= 'A' && definition[i] <= 'Z') || (definition[i] >= 'a' && definition[i] <= 'z') || definition[i] == '_' {
			start := i
			for i < len(definition) && ((definition[i] >= 'A' && definition[i] <= 'Z') || (definition[i] >= 'a' && definition[i] <= 'z') || (definition[i] >= '0' && definition[i] <= '9') || definition[i] == '_') {
				i++
			}
			tokens = append(tokens, strings.ToUpper(definition[start:i]))
			continue
		}
		i++
	}
	return tokens
}

func conservativeQueryTokens(definition string, withFrom bool) bool {
	tokens := sqlTokens(definition)
	selects, froms := 0, 0
	for _, token := range tokens {
		switch token {
		case "SELECT":
			selects++
		case "FROM":
			froms++
		case "TABLE", "WITH", "JOIN", "UNION", "INTERSECT", "EXCEPT":
			return false
		}
	}
	if withFrom {
		return selects == 1 && froms == 1
	}
	return selects == 1 && froms == 0
}

func simpleViewMatch(definition string) []string {
	match := simpleViewFrom.FindStringSubmatch(definition)
	if match == nil {
		return nil
	}
	if !conservativeQueryTokens(definition, true) {
		return nil
	}
	items := strings.Split(match[1], ",")
	for _, item := range items {
		item = strings.TrimSpace(item)
		if !directProjection.MatchString(item) || item == "*" && len(items) != 1 {
			return nil
		}
		if dot := strings.IndexByte(item, '.'); dot >= 0 && !strings.EqualFold(item[:dot], match[3]) {
			return nil
		}
	}
	return match
}

func simpleLiteralMatch(definition string) []string {
	match := simpleLiteralView.FindStringSubmatch(definition)
	if match == nil || !conservativeQueryTokens(definition, false) || !provenLiteral.MatchString(strings.TrimSpace(match[1])) {
		return nil
	}
	return match
}

func augmentProjectionColumns(doc *schema.Document) {
	children := map[string]bool{}
	tables := map[string]schema.Resource{}
	columns := map[string][]schema.Resource{}
	for _, r := range doc.Graph.Resources {
		if r.Kind == schema.KindColumn {
			children[r.Name.Parent] = true
			columns[r.Name.Parent] = append(columns[r.Name.Parent], r)
		}
		if r.Kind == schema.KindTable || r.Kind == schema.KindView || r.Kind == schema.KindMaterializedView {
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
		if match := simpleViewMatch(definition); match != nil {
			table, ok := tables[match[2]+"."+match[3]]
			if !ok {
				continue
			}
			hasReference := false
			for _, dep := range r.Dependencies {
				hasReference = hasReference || dep.Target == table.ID && dep.Type == schema.DependencyReferences
			}
			if !hasReference {
				r.Dependencies = append(r.Dependencies, schema.Dependency{Target: table.ID, Type: schema.DependencyReferences})
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
		if match := simpleLiteralMatch(definition); match != nil {
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
	decoder := json.NewDecoder(strings.NewReader(string(raw)))
	decoder.UseNumber()
	_ = decoder.Decode(&out)
	return out
}
func numberAsInt(values map[string]any, key string) int {
	switch value := values[key].(type) {
	case float64:
		return int(value)
	case json.Number:
		result, _ := strconv.Atoi(value.String())
		return result
	}
	return 0
}

func normalizePostgresSpecForKind(kind schema.Kind, spec map[string]any) {
	// Routine definitions are reviewed and authorized as exact source. Preserve
	// their line structure before the generic definition normalizer collapses
	// SQL whitespace: a PL/pgSQL -- comment is semantic through end-of-line.
	var routineDefinition string
	var hasRoutineDefinition bool
	if kind == schema.KindFunction || kind == schema.KindProcedure {
		routineDefinition, hasRoutineDefinition = spec["definition"].(string)
	}
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
		if identity, ok := spec["identity"].(string); ok {
			switch identity {
			case "always":
				spec["identity"] = "a"
			case "by_default":
				spec["identity"] = "d"
			}
		}
	}
	if kind == schema.KindTable {
		if options, ok := spec["options"].(string); ok {
			if strings.TrimSpace(options) != "" {
				return
			}
			delete(spec, "options")
		}
		if _, ok := spec["partitioned"]; !ok {
			spec["partitioned"] = false
		}
		if _, ok := spec["persistence"]; !ok {
			spec["persistence"] = "p"
		}
		if _, ok := spec["row_security"]; !ok {
			spec["row_security"] = false
		}
		if _, ok := spec["force_row_security"]; !ok {
			spec["force_row_security"] = false
		}
	}
	if kind == schema.KindComposite {
		if attributes, ok := spec["attributes"].([]any); ok {
			for index, raw := range attributes {
				attribute, ok := raw.(map[string]any)
				if !ok {
					continue
				}
				if nullable, ok := attribute["not_null"].(bool); ok && !nullable {
					delete(attribute, "not_null")
				}
				attribute["ordinal"] = index + 1
				if typ, ok := attribute["type"].(string); ok {
					attribute["type"] = postgresTypeAlias(typ)
				}
				if collation, ok := attribute["collation"].(string); ok && strings.TrimSpace(collation) == "" {
					delete(attribute, "collation")
				}
			}
		}
	}
	if kind == schema.KindView || kind == schema.KindMaterializedView {
		if definition, ok := spec["definition"].(string); ok {
			definition = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(definition), ";"))
			if len(definition) >= 2 && strings.EqualFold(definition[:2], "AS") && (len(definition) == 2 || definition[2] == ' ') {
				definition = strings.TrimSpace(definition[2:])
			}
			spec["definition"] = normalizeSQLSpace(definition)
			if match := simpleViewMatch(spec["definition"].(string)); match != nil {
				items := strings.Split(match[1], ",")
				for i, item := range items {
					items[i] = strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(item), match[3]+"."))
				}
				spec["definition"] = "SELECT " + strings.Join(items, ", ") + " FROM " + match[2] + "." + match[3]
			}
		}
	}
	if kind == schema.KindFunction || kind == schema.KindProcedure {
		if hasRoutineDefinition {
			definition := normalizeRoutineSource(routineDefinition)
			spec["definition"] = definition
			spec["body_digest"] = routineDefinitionDigest(definition)
		}
	}
}

func normalizeRoutineSource(definition string) string {
	definition = strings.ReplaceAll(definition, "\r\n", "\n")
	definition = strings.ReplaceAll(definition, "\r", "\n")
	return strings.TrimSpace(definition)
}

func routineDefinitionDigest(definition string) string {
	digest := sha256.Sum256([]byte(definition))
	return fmt.Sprintf("sha256:%x", digest[:])
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
	array := ""
	for strings.HasSuffix(original, "[]") {
		// PostgreSQL array bounds and declared dimensionality are not enforced
		// by the type system and format_type reports the canonical array type.
		array = "[]"
		original = strings.TrimSpace(strings.TrimSuffix(original, "[]"))
	}
	// Quoted identifiers and user-defined names are case-sensitive. Only fold
	// names from PostgreSQL's documented built-in alias set.
	if strings.Contains(original, `"`) {
		return original + array
	}
	s := strings.ToLower(original)
	s = strings.TrimPrefix(s, "pg_catalog.")
	for _, qualified := range []struct{ prefix, suffix, canonical string }{
		{"timestamp", " without time zone", "timestamp"}, {"timestamp", " with time zone", "timestamptz"},
		{"time", " without time zone", "time"}, {"time", " with time zone", "timetz"},
	} {
		if strings.HasPrefix(s, qualified.prefix+"(") && strings.HasSuffix(s, qualified.suffix) {
			modifier := strings.TrimSuffix(strings.TrimPrefix(s, qualified.prefix), qualified.suffix)
			return qualified.canonical + modifier + array
		}
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
		"character varying": "character varying", "bpchar": "character", "char": "character", "character": "character",
		"bit": "bit", "varbit": "bit varying", "bit varying": "bit varying",
		"timestamp without time zone": "timestamp", "timestamp": "timestamp", "timestamp with time zone": "timestamptz", "timestamptz": "timestamptz",
		"time without time zone": "time", "time": "time", "time with time zone": "timetz", "timetz": "timetz",
		"numeric": "numeric", "decimal": "numeric", "date": "date", "interval": "interval", "text": "text",
		"uuid": "uuid", "json": "json", "jsonb": "jsonb", "bytea": "bytea",
		"cidr": "cidr", "inet": "inet", "macaddr": "macaddr",
	}
	if normalized, ok := aliases[s]; ok {
		return normalized + suffix + array
	}
	return original + array
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
	for _, temporal := range []struct{ lower, canonical string }{
		{"current_date", "CURRENT_DATE"}, {"current_time", "CURRENT_TIME"}, {"localtime", "LOCALTIME"}, {"localtimestamp", "LOCALTIMESTAMP"},
	} {
		if lower == temporal.lower {
			return temporal.canonical
		}
		if strings.HasPrefix(lower, temporal.lower+"(") && strings.HasSuffix(lower, ")") {
			return temporal.canonical + lower[len(temporal.lower):]
		}
	}
	if lower == "gen_random_uuid()" || lower == "pg_catalog.gen_random_uuid()" {
		return "pg_catalog.gen_random_uuid()"
	}
	if expression, err := classifyDefaultExpression(s); err == nil && expression.Kind == defaultExpressionCast && expression.Cast != nil {
		castType, castOK := coreDefaultCastType(expression.Cast.Type)
		if castOK && castType.base == "text" && !castType.array && expression.Cast.Expression.Kind == defaultExpressionFunction && isGenRandomUUIDFunction(expression.Cast.Expression.Function, false) {
			return "pg_catalog.gen_random_uuid()::text"
		}
	}
	for _, clock := range []string{"now()", "transaction_timestamp()", "current_timestamp", "current_timestamp()"} {
		if lower == "timezone('utc'::text, "+clock+")" || lower == "pg_catalog.timezone('utc'::text, "+clock+")" {
			return "pg_catalog.timezone('utc'::text, CURRENT_TIMESTAMP)"
		}
	}
	if lower == "null" || strings.HasPrefix(lower, "null::") {
		return "NULL"
	}
	for _, cast := range []string{"::character varying", "::varchar", "::text", "::integer", "::bigint", "::boolean"} {
		if strings.HasSuffix(strings.ToLower(s), cast) {
			base := strings.TrimSpace(s[:len(s)-len(cast)])
			if (cast == "::integer" || cast == "::bigint") && len(base) >= 2 && base[0] == '\'' && base[len(base)-1] == '\'' {
				inner := base[1 : len(base)-1]
				if regexp.MustCompile(`^-?[0-9]+$`).MatchString(inner) {
					return inner
				}
			}
			if cast == "::boolean" && (base == "'true'" || base == "'false'") {
				return base[1 : len(base)-1]
			}
			if strings.HasPrefix(base, "'") || base == "true" || base == "false" || base == "NULL" || base == "null" {
				return base
			}
		}
	}
	if expression, err := classifyDefaultExpression(s); err == nil && containsDefaultOperator(expression) {
		if canonical, err := canonicalOperatorDefault(expression); err == nil {
			return canonical
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
