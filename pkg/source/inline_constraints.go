package source

import (
	"encoding/json"
	"errors"
	"slices"
	"strings"

	"autosql/pkg/schema"
	pg_query "github.com/pganalyze/pg_query_go/v6"
	"google.golang.org/protobuf/reflect/protoreflect"
)

func readInlineConstraints(item string) ([]*pg_query.Constraint, bool, error) {
	parsed, err := pg_query.Parse("CREATE TABLE autosql_inline (" + item + ")")
	if err != nil || len(parsed.Stmts) != 1 {
		return nil, false, errors.New("invalid SQL column definition")
	}
	statement := parsed.Stmts[0].Stmt.GetCreateStmt()
	if statement == nil || len(statement.TableElts) != 1 {
		return nil, false, errors.New("invalid SQL column definition")
	}
	column := statement.TableElts[0].GetColumnDef()
	if column == nil {
		return nil, false, errors.New("expected a SQL column definition")
	}
	return collectInlineConstraints(column.Constraints)
}

func collectInlineConstraints(nodes []*pg_query.Node) ([]*pg_query.Constraint, bool, error) {
	var result []*pg_query.Constraint
	notNull := false
	for _, node := range nodes {
		constraint := node.GetConstraint()
		if constraint == nil {
			return nil, false, errors.New("invalid inline SQL constraint")
		}
		if constraint.Contype == pg_query.ConstrType_CONSTR_NOTNULL || constraint.Contype == pg_query.ConstrType_CONSTR_PRIMARY {
			notNull = true
		}
		if len(result) > 0 && applyInlineConstraintAttribute(result[len(result)-1], constraint.Contype) {
			continue
		}
		if kind, _ := inlineConstraintKind(constraint.Contype); kind != "" {
			result = append(result, constraint)
		}
	}
	return result, notNull, nil
}

func applyInlineConstraintAttribute(c *pg_query.Constraint, kind pg_query.ConstrType) bool {
	switch kind {
	case pg_query.ConstrType_CONSTR_ATTR_DEFERRABLE:
		c.Deferrable = true
	case pg_query.ConstrType_CONSTR_ATTR_NOT_DEFERRABLE:
		c.Deferrable = false
	case pg_query.ConstrType_CONSTR_ATTR_DEFERRED:
		c.Initdeferred = true
	case pg_query.ConstrType_CONSTR_ATTR_IMMEDIATE:
		c.Initdeferred = false
	default:
		return false
	}
	return true
}

func inlineConstraintKind(kind pg_query.ConstrType) (schema.Kind, string) {
	switch kind {
	case pg_query.ConstrType_CONSTR_PRIMARY:
		return schema.KindPrimaryKey, "pkey"
	case pg_query.ConstrType_CONSTR_UNIQUE:
		return schema.KindUniqueConstraint, "key"
	case pg_query.ConstrType_CONSTR_FOREIGN:
		return schema.KindForeignKey, "fkey"
	case pg_query.ConstrType_CONSTR_CHECK:
		return schema.KindCheckConstraint, "check"
	default:
		return "", ""
	}
}

func inlineConstraintResources(constraints []*pg_query.Constraint, q qname, table, column schema.Resource, loc *schema.SourceLocation) ([]schema.Resource, error) {
	var result []schema.Resource
	for _, constraint := range constraints {
		kind, suffix := inlineConstraintKind(constraint.Contype)
		name := constraint.Conname
		if name == "" {
			name = q.Name + "_" + column.Name.Name + "_" + suffix
		}
		definition, err := inlineConstraintDefinition(constraint, column.Name.Name)
		if err != nil {
			return nil, err
		}
		columns := []string{column.Name.Name}
		if kind == schema.KindCheckConstraint {
			columns = inlineCheckColumns(constraint.RawExpr)
		}
		deps := []schema.Dependency{{Target: table.ID, Type: schema.DependencyContains}}
		for _, name := range columns {
			id := schema.StableID(schema.KindColumn, schema.Name{Schema: q.Schema, Name: name, Parent: table.ID})
			deps = append(deps, schema.Dependency{Target: id, Type: schema.DependencyReferences})
		}
		values := map[string]any{"definition": definition, "columns": columns, "deferrable": constraint.Deferrable, "initially_deferred": constraint.Initdeferred, "validated": true}
		if kind == schema.KindForeignKey {
			values["referenced_columns"] = inlineKeyNames(constraint.PkAttrs)
		}
		result = append(result, newResource(kind, schema.Name{Schema: q.Schema, Name: name, Parent: table.ID}, values, loc, deps))
	}
	return result, nil
}

func inlineKeyNames(nodes []*pg_query.Node) []string {
	result := make([]string, 0, len(nodes))
	for _, node := range nodes {
		result = append(result, node.GetString_().GetSval())
	}
	return result
}

func inlineCheckColumns(node *pg_query.Node) []string {
	result := []string{}
	walkInlineExpression(node.ProtoReflect(), func(message protoreflect.Message) {
		ref, ok := message.Interface().(*pg_query.ColumnRef)
		if ok && len(ref.Fields) > 0 {
			result = append(result, ref.Fields[len(ref.Fields)-1].GetString_().GetSval())
		}
	})
	slices.Sort(result)
	return slices.Compact(result)
}

func walkInlineExpression(message protoreflect.Message, visit func(protoreflect.Message)) {
	visit(message)
	message.Range(func(field protoreflect.FieldDescriptor, value protoreflect.Value) bool {
		if field.Kind() != protoreflect.MessageKind {
			return true
		}
		if field.IsList() {
			for i := 0; i < value.List().Len(); i++ {
				walkInlineExpression(value.List().Get(i).Message(), visit)
			}
		} else {
			walkInlineExpression(value.Message(), visit)
		}
		return true
	})
}

// Inline references are bound after every table in this SQL source is known.
// Leave unresolved or ambiguous keys to graph/renderer validation; never guess
// a key name or weaken the renderer's exact dependency check.
func bindInlineForeignKeys(resources map[string]schema.Resource) {
	for id, resource := range resources {
		if resource.Kind != schema.KindForeignKey {
			continue
		}
		var values struct {
			Definition        string   `json:"definition"`
			Columns           []string `json:"columns"`
			ReferencedColumns []string `json:"referenced_columns"`
		}
		if json.Unmarshal(resource.Spec, &values) != nil || values.Columns == nil {
			continue
		}
		tree, err := pg_query.Parse("ALTER TABLE autosql_inline ADD " + values.Definition)
		if err != nil {
			continue
		}
		constraint := tree.Stmts[0].Stmt.GetAlterTableStmt().Cmds[0].GetAlterTableCmd().Def.GetConstraint()
		resource.Dependencies = inlineForeignDependencies(resource, constraint, values.ReferencedColumns, resources)
		resources[id] = resource
	}
}

func inlineForeignDependencies(resource schema.Resource, constraint *pg_query.Constraint, columns []string, resources map[string]schema.Resource) []schema.Dependency {
	q := qname{Schema: constraint.Pktable.Schemaname, Name: constraint.Pktable.Relname}
	if q.Schema == "" {
		q.Schema = resource.Name.Schema
	}
	table := schema.StableID(schema.KindTable, ownedName(q))
	deps := resource.Dependencies
	deps = append(deps, schema.Dependency{Target: table, Type: schema.DependencyReferences})
	for _, name := range columns {
		id := schema.StableID(schema.KindColumn, schema.Name{Schema: q.Schema, Name: name, Parent: table})
		deps = append(deps, schema.Dependency{Target: id, Type: schema.DependencyReferences})
	}
	var keys []string
	for id, candidate := range resources {
		if candidate.Name.Parent != table || (candidate.Kind != schema.KindPrimaryKey && candidate.Kind != schema.KindUniqueConstraint) {
			continue
		}
		var values struct {
			Columns []string `json:"columns"`
		}
		if json.Unmarshal(candidate.Spec, &values) == nil && slices.Equal(values.Columns, columns) {
			keys = append(keys, id)
		}
	}
	if len(keys) == 1 {
		deps = append(deps, schema.Dependency{Target: keys[0], Type: schema.DependencyReferences})
	}
	return deps
}

func inlineConstraintDefinition(constraint *pg_query.Constraint, column string) (string, error) {
	key := &pg_query.Node{Node: &pg_query.Node_String_{String_: &pg_query.String{Sval: column}}}
	switch constraint.Contype {
	case pg_query.ConstrType_CONSTR_PRIMARY, pg_query.ConstrType_CONSTR_UNIQUE:
		constraint.Keys = []*pg_query.Node{key}
	case pg_query.ConstrType_CONSTR_FOREIGN:
		constraint.FkAttrs = []*pg_query.Node{key}
	}
	constraint.Conname = ""
	definitionNode := &pg_query.Node{Node: &pg_query.Node_Constraint{Constraint: constraint}}
	command := &pg_query.Node{Node: &pg_query.Node_AlterTableCmd{AlterTableCmd: &pg_query.AlterTableCmd{Subtype: pg_query.AlterTableType_AT_AddConstraint, Def: definitionNode}}}
	statement := &pg_query.AlterTableStmt{Relation: &pg_query.RangeVar{Relname: "autosql_inline", Inh: true}, Cmds: []*pg_query.Node{command}}
	tree := &pg_query.ParseResult{Stmts: []*pg_query.RawStmt{{Stmt: &pg_query.Node{Node: &pg_query.Node_AlterTableStmt{AlterTableStmt: statement}}}}}
	sql, err := pg_query.Deparse(tree)
	if err != nil {
		return "", errors.New("inline constraint cannot be rendered")
	}
	_, definition, ok := strings.Cut(sql, " ADD ")
	if !ok {
		return "", errors.New("inline constraint changed statement shape")
	}
	return definition, nil
}
