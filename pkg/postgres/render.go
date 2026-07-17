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
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

func (*Driver) Render(ctx context.Context, request plugin.RenderRequest) ([]plugin.Statement, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if err := request.Changes.Validate(); err != nil {
		return nil, fmt.Errorf("render PostgreSQL changes: %w", err)
	}
	// A validated empty change set cannot mutate the database. Do not reject a
	// no-op merely because unchanged inspected resources contain metadata or
	// read-only semantics that would be unsafe to render as an actual change.
	if len(request.Changes.Changes) == 0 {
		return nil, nil
	}
	if err := validateExtensionMemberChanges(request); err != nil {
		return nil, fmt.Errorf("render PostgreSQL changes: %w", err)
	}
	if err := validateExternalGeneratedRoutineChanges(request); err != nil {
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
	extensionTransitions := extensionTransitionIDs(request)
	roleRenamePresent := false
	for _, change := range request.Changes.Changes {
		roleRenamePresent = roleRenamePresent || change.Before != nil && change.After != nil && change.After.Kind == schema.KindRole && change.Before.ID != change.After.ID
	}
	for _, doc := range []schema.Document{request.Current, request.Desired} {
		for _, r := range doc.Graph.Resources {
			resources[r.ID] = r
		}
	}
	var output []plugin.Statement
	for _, change := range request.Changes.Changes {
		options := request.Options
		if roleRenamePresent {
			options = cloneOptions(options)
			options["__membership_has_role_rename"] = "true"
		}
		if rebuilds[change.ID] {
			options = cloneOptions(options)
			options["__view_rebuild"] = "true"
		}
		resource := change.After
		if resource == nil {
			resource = change.Before
		}
		if resource != nil {
			if owner := extensionOwnerID(*resource, resources); owner != "" && extensionTransitions[owner] {
				output = append(output, plugin.Statement{ChangeID: change.ID, Transactional: true, Kind: plugin.StatementTopology})
				continue
			}
		}
		statements, err := renderChange(change, resources, options)
		if err != nil {
			return nil, fmt.Errorf("render PostgreSQL changes: %w", err)
		}
		output = append(output, statements...)
	}
	return output, nil
}

func extensionTransitionIDs(request plugin.RenderRequest) map[string]bool {
	out := map[string]bool{}
	for _, change := range request.Changes.Changes {
		if change.Before != nil && change.Before.Kind == schema.KindExtension {
			out[change.Before.ID] = true
		}
		if change.After != nil && change.After.Kind == schema.KindExtension {
			out[change.After.ID] = true
		}
	}
	return out
}

func extensionOwnerID(resource schema.Resource, resources map[string]schema.Resource) string {
	for _, dependency := range resource.Dependencies {
		if dependency.Type == schema.DependencyOwns && resources[dependency.Target].Kind == schema.KindExtension {
			return dependency.Target
		}
	}
	return ""
}

func validateExtensionMemberChanges(request plugin.RenderRequest) error {
	resources := resourceMapForRender(request.Current)
	for id, resource := range resourceMapForRender(request.Desired) {
		resources[id] = resource
	}
	transitions := extensionTransitionIDs(request)
	for _, change := range request.Changes.Changes {
		resource := change.After
		if resource == nil {
			resource = change.Before
		}
		if resource == nil || resource.Kind == schema.KindExtension {
			continue
		}
		if owner := extensionOwnerID(*resource, resources); owner != "" && !transitions[owner] {
			return unsupported(*resource, "extension-owned resource must be changed through its owning extension")
		}
	}
	return nil
}

func validateExternalGeneratedRoutineChanges(request plugin.RenderRequest) error {
	transitions := extensionTransitionIDs(request)
	required := map[string]bool{}
	for _, document := range []schema.Document{request.Current, request.Desired} {
		resources := resourceMapForRender(document)
		for _, resource := range document.Graph.Resources {
			if resource.Kind != schema.KindColumn || stringValue(spec(resource), "generated") != "s" {
				continue
			}
			for _, dependency := range resource.Dependencies {
				if target, ok := resources[dependency.Target]; ok && dependency.Type == schema.DependencyReferences && target.Kind == schema.KindFunction {
					required[target.ID] = true
				}
			}
		}
	}
	for _, change := range request.Changes.Changes {
		resource := change.After
		if resource == nil {
			resource = change.Before
		}
		if resource != nil && resource.Kind == schema.KindFunction && required[resource.ID] && stringValue(spec(*resource), "extension") != "" && !transitions[extensionOwnerID(*resource, resourceMapForRender(request.Current))] && !transitions[extensionOwnerID(*resource, resourceMapForRender(request.Desired))] {
			classification := "application-owned"
			if stringValue(spec(*resource), "extension") != "" {
				classification = "extension-owned"
			}
			return unsupported(*resource, classification+" generated-routine prerequisite must already exist with the exact inspected fingerprint")
		}
	}
	return nil
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
	parentOnlyRename := change.Operation == schema.OperationRename && change.Before != nil && change.After != nil && change.Before.Name.Name == change.After.Name.Name && change.Before.Name.Parent != change.After.Name.Parent && r.Kind != schema.KindExtension
	membershipRename := change.Operation == schema.OperationRename && r.Kind == schema.KindMembership
	membershipRoleAlter := change.Operation == schema.OperationAlter && r.Kind == schema.KindMembership && options["__membership_has_role_rename"] == "true" && membershipOptionsEqual(*change.Before, *change.After)
	projectionChild := r.Kind == schema.KindColumn && isManagedProjectionParent(r.Name.Parent, resources)
	commentOnlyAlter := change.Operation == schema.OperationAlter && change.Before != nil && change.After != nil && !resourceSQLSemanticsChanged(*change.Before, *change.After) && change.Before.Annotations["comment"] != change.After.Annotations["comment"]
	if !parentOnlyRename && !membershipRename && !membershipRoleAlter && !projectionChild && !commentOnlyAlter {
		if err := plugin.RequireManagedOperation(New().Info(), r.Kind, change.Operation); err != nil {
			return nil, err
		}
	}
	if parentOnlyRename || membershipRename || membershipRoleAlter || projectionChild {
		out := []plugin.Statement{{ChangeID: change.ID, Transactional: true, Kind: plugin.StatementTopology}}
		comments, err := renderCommentChange(change, resources)
		if err != nil {
			return nil, err
		}
		for _, sql := range comments {
			out = append(out, plugin.Statement{SQL: terminate(sql), ChangeID: change.ID, Transactional: true, Kind: plugin.StatementExecutable})
		}
		return out, nil
	}
	if change.Operation == schema.OperationAlter && r.Kind == schema.KindColumn && columnOrdinalOnly(*change.Before, *change.After) {
		out := []plugin.Statement{{ChangeID: change.ID, Transactional: true, Kind: plugin.StatementTopology}}
		comments, err := renderCommentChange(change, resources)
		if err != nil {
			return nil, err
		}
		for _, sql := range comments {
			out = append(out, plugin.Statement{SQL: terminate(sql), ChangeID: change.ID, Transactional: true, Kind: plugin.StatementExecutable})
		}
		return out, nil
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
		if resourceSQLSemanticsChanged(*change.Before, *change.After) {
			sqls, err = renderAlter(*change.Before, *change.After, resources, options)
		}
	default:
		err = fmt.Errorf("%w: operation %s", plugin.ErrUnsupported, change.Operation)
	}
	if err != nil {
		return nil, err
	}
	comments, err := renderCommentChange(change, resources)
	if err != nil {
		return nil, err
	}
	sqls = append(sqls, comments...)
	if len(sqls) == 0 {
		return nil, unsupported(*r, "alteration has no renderable semantics")
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
		if !allowedKeys(s, "owner") {
			return nil, unsupported(r, "unknown schema semantics")
		}
		q := "CREATE SCHEMA " + quote(r.Name.Name)
		if owner := stringValue(s, "owner"); owner != "" {
			q += " AUTHORIZATION " + quote(owner)
		}
		return []string{q}, nil
	case schema.KindExtension:
		if err := validateExtensionOptions(r, options); err != nil {
			return nil, err
		}
		q := "CREATE EXTENSION " + quote(r.Name.Name)
		if r.Name.Schema != "" {
			q += " WITH SCHEMA " + quote(r.Name.Schema)
		}
		if v := stringValue(s, "version"); v != "" {
			q += " VERSION " + literal(v)
		}
		if boolValue(s, "cascade") {
			q += " CASCADE"
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
		out := []string{"CREATE TYPE " + name + " AS ENUM (" + strings.Join(qs, ", ") + ")"}
		return appendOwnerCreate(out, r, "TYPE"), nil
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
		return appendOwnerCreate([]string{q}, r, "DOMAIN"), nil
	case schema.KindComposite:
		attrs, parseErr := parseCompositeAttributes(r)
		if parseErr != nil {
			return nil, parseErr
		}
		parts := []string{}
		for _, attribute := range attrs {
			part := quote(attribute.Name) + " " + attribute.Type
			if attribute.Collation != "" {
				part += " COLLATE " + attribute.Collation
			}
			parts = append(parts, part)
		}
		return appendOwnerCreate([]string{"CREATE TYPE " + name + " AS (" + strings.Join(parts, ", ") + ")"}, r, "TYPE"), nil
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
		return appendOwnerCreate([]string{q}, r, "SEQUENCE"), nil
	case schema.KindTable:
		if !allowedKeys(s, "partitioned", "persistence", "row_security", "force_row_security", "owner") {
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
		return appendOwnerCreate(out, r, "TABLE"), nil
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
		parsed, e := parseConstraintDefinition(r, resources)
		if e != nil {
			return nil, e
		}
		if parsed.constraint.GetNullsNotDistinct() {
			if e := requirePostgresMajor(r, options, 15, "NULLS NOT DISTINCT constraint"); e != nil {
				return nil, e
			}
		}
		d := stringValue(s, "definition")
		return []string{"ALTER TABLE " + parent + " ADD CONSTRAINT " + quote(r.Name.Name) + " " + d}, nil
	case schema.KindIndex:
		if err != nil {
			return nil, err
		}
		return renderCreateIndex(r, resources, options)
	case schema.KindFunction, schema.KindProcedure:
		if stringValue(s, "extension") != "" {
			return nil, unsupported(r, "extension-owned routine must be provisioned by its extension")
		}
		parsed, e := validateRoutineSource(r, options)
		if e != nil {
			return nil, e
		}
		out := []string{parsed.SQL}
		if owner := stringValue(s, "owner"); owner != "" {
			signature, signatureErr := routineSignature(r)
			if signatureErr != nil {
				return nil, signatureErr
			}
			out = append(out, "ALTER "+routineKeyword(r)+" "+signature+" OWNER TO "+quote(owner))
		}
		return out, nil
	case schema.KindTrigger:
		parsed, e := parseTriggerDefinition(r, resources)
		if e != nil {
			return nil, e
		}
		out := []string{parsed.SQL}
		if stringValue(s, "enabled") != "O" {
			enable, enableErr := renderTriggerEnable(r, resources)
			if enableErr != nil {
				return nil, enableErr
			}
			out = append(out, enable)
		}
		return out, nil
	case schema.KindPolicy:
		parsed, e := parsePolicy(r, resources)
		if e != nil {
			return nil, e
		}
		return []string{parsed.SQL}, nil
	case schema.KindRole:
		return renderRoleCreate(r, options)
	case schema.KindMembership:
		return renderMembershipGrant(r, resources, options)
	case schema.KindGrant:
		return renderGrantCreate(r, resources)
	case schema.KindDefaultPrivilege:
		return renderDefaultPrivilegeCreate(r, resources)
	case schema.KindView, schema.KindMaterializedView:
		if !allowedKeys(s, "definition", "owner") {
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
		return appendOwnerCreate([]string{"CREATE " + kind + " " + name + " AS " + d}, r, kind), nil
	default:
		return nil, unsupported(r, "create")
	}
}

func appendOwnerCreate(statements []string, resource schema.Resource, keyword string) []string {
	if owner := stringValue(spec(resource), "owner"); owner != "" {
		statements = append(statements, "ALTER "+keyword+" "+qualified(resource.Name)+" OWNER TO "+quote(owner))
	}
	return statements
}

func ownerOnlyChange(before, after map[string]any) bool {
	b := make(map[string]any, len(before))
	a := make(map[string]any, len(after))
	for key, value := range before {
		if key != "owner" {
			b[key] = value
		}
	}
	for key, value := range after {
		if key != "owner" {
			a[key] = value
		}
	}
	return slices.Equal(canonicalMapEntries(b), canonicalMapEntries(a))
}

func ownerAlterKeyword(kind schema.Kind) string {
	switch kind {
	case schema.KindSchema:
		return "SCHEMA"
	case schema.KindEnum, schema.KindComposite:
		return "TYPE"
	case schema.KindDomain:
		return "DOMAIN"
	case schema.KindSequence:
		return "SEQUENCE"
	case schema.KindTable:
		return "TABLE"
	case schema.KindView:
		return "VIEW"
	case schema.KindMaterializedView:
		return "MATERIALIZED VIEW"
	default:
		return ""
	}
}

func renderDrop(r schema.Resource, resources map[string]schema.Resource, options map[string]string) ([]string, error) {
	name := qualified(r.Name)
	parent, err := parentName(r, resources)
	switch r.Kind {
	case schema.KindSchema:
		return []string{"DROP SCHEMA " + name}, nil
	case schema.KindExtension:
		if !enabled(options, "allow_extension_drop", false) {
			return nil, unsupported(r, "extension drop requires allow_extension_drop=true")
		}
		return []string{"DROP EXTENSION " + quote(r.Name.Name)}, nil
	case schema.KindComposite:
		if !enabled(options, "allow_composite_drop", false) {
			return nil, unsupported(r, "composite type drop requires allow_composite_drop=true")
		}
		return []string{"DROP TYPE " + name}, nil
	case schema.KindEnum:
		return []string{"DROP TYPE " + name}, nil
	case schema.KindDomain:
		return []string{"DROP DOMAIN " + name}, nil
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
	case schema.KindFunction, schema.KindProcedure:
		signature, e := routineSignature(r)
		if e != nil {
			return nil, e
		}
		return []string{"DROP " + routineKeyword(r) + " " + signature}, nil
	case schema.KindTrigger:
		if err != nil {
			return nil, err
		}
		return []string{"DROP TRIGGER " + quote(r.Name.Name) + " ON " + parent}, nil
	case schema.KindPolicy:
		if err != nil {
			return nil, err
		}
		return []string{"DROP POLICY " + quote(r.Name.Name) + " ON " + parent}, nil
	case schema.KindRole:
		return renderRoleDrop(r, resources, options)
	case schema.KindMembership:
		return renderMembershipRevoke(r, resources, options)
	case schema.KindGrant:
		return renderGrantDrop(r, resources)
	case schema.KindDefaultPrivilege:
		return renderDefaultPrivilegeDrop(r, resources)
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
	case schema.KindFunction, schema.KindProcedure:
		signature, e := routineSignature(before)
		if e != nil {
			return nil, e
		}
		identitySuffix := "(" + stringValue(spec(after), "identity_arguments") + ")"
		newBaseName := strings.TrimSuffix(after.Name.Name, identitySuffix)
		if newBaseName == "" || newBaseName == after.Name.Name {
			return nil, unsupported(after, "function rename target identity is not canonical")
		}
		return []string{"ALTER " + routineKeyword(after) + " " + signature + " RENAME TO " + quote(newBaseName)}, nil
	case schema.KindTrigger:
		if err != nil {
			return nil, err
		}
		return []string{"ALTER TRIGGER " + quote(before.Name.Name) + " ON " + parent + " RENAME TO " + quote(after.Name.Name)}, nil
	case schema.KindPolicy:
		if err != nil {
			return nil, err
		}
		return []string{"ALTER POLICY " + quote(before.Name.Name) + " ON " + parent + " RENAME TO " + quote(after.Name.Name)}, nil
	case schema.KindRole:
		if protectedRole(before.Name.Name) || protectedRole(after.Name.Name) {
			return nil, unsupported(after, "system/protected roles cannot be renamed")
		}
		return []string{"ALTER ROLE " + quote(before.Name.Name) + " RENAME TO " + quote(after.Name.Name)}, nil
	case schema.KindSequence:
		return []string{"ALTER SEQUENCE " + old + " RENAME TO " + newName}, nil
	case schema.KindView:
		return []string{"ALTER VIEW " + old + " RENAME TO " + newName}, nil
	case schema.KindMaterializedView:
		return []string{"ALTER MATERIALIZED VIEW " + old + " RENAME TO " + newName}, nil
	case schema.KindEnum, schema.KindComposite:
		return []string{"ALTER TYPE " + old + " RENAME TO " + newName}, nil
	case schema.KindExtension:
		if before.Name.Name != after.Name.Name {
			return nil, unsupported(after, "PostgreSQL extensions cannot be renamed")
		}
		if !boolValue(spec(before), "relocatable") {
			return nil, unsupported(after, "extension control metadata marks it non-relocatable")
		}
		return []string{"ALTER EXTENSION " + quote(after.Name.Name) + " SET SCHEMA " + quote(after.Name.Schema)}, nil
	case schema.KindDomain:
		return []string{"ALTER DOMAIN " + old + " RENAME TO " + newName}, nil
	default:
		return nil, unsupported(after, "rename")
	}
}

func renderAlter(before, after schema.Resource, resources map[string]schema.Resource, options map[string]string) ([]string, error) {
	if before.Kind != after.Kind {
		return nil, unsupported(after, "kind change")
	}
	bs, as := spec(before), spec(after)
	if stringValue(bs, "owner") != stringValue(as, "owner") && ownerOnlyChange(bs, as) {
		keyword := ownerAlterKeyword(after.Kind)
		if keyword == "" || stringValue(as, "owner") == "" {
			return nil, unsupported(after, "owner removal or ownership transfer is unsupported for this kind")
		}
		return []string{"ALTER " + keyword + " " + qualified(after.Name) + " OWNER TO " + quote(stringValue(as, "owner"))}, nil
	}
	name := qualified(after.Name)
	parent, err := parentName(after, resources)
	switch after.Kind {
	case schema.KindColumn:
		if err != nil {
			return nil, err
		}
		var out []string
		if stringValue(bs, "generated") != "" || stringValue(as, "generated") != "" {
			return nil, unsupported(after, "generated-column alteration")
		}
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
		if enabled(options, "concurrent_indexes", false) {
			return renderConcurrentIndexRebuild(before, after, resources, options)
		}
		drop, _ := renderDrop(before, resources, options)
		create, e := renderCreate(after, resources, options)
		return append(drop, create...), e
	case schema.KindPolicy:
		// PostgreSQL cannot alter command or permissive mode. A transactional
		// deny-first replacement keeps RLS enabled and never widens access
		// between the two statements.
		drop, e := renderDrop(before, resources, options)
		if e != nil {
			return nil, e
		}
		create, e := renderCreate(after, resources, options)
		return append(drop, create...), e
	case schema.KindRole:
		return renderRoleAlter(before, after, options)
	case schema.KindMembership:
		drop, e := renderMembershipRevoke(before, resources, options)
		if e != nil {
			return nil, e
		}
		create, e := renderMembershipGrant(after, resources, options)
		return append(drop, create...), e
	case schema.KindGrant:
		return renderGrantAlter(before, after, resources)
	case schema.KindDefaultPrivilege:
		return renderDefaultPrivilegeAlter(before, after, resources)
	case schema.KindFunction, schema.KindProcedure:
		if stringValue(as, "extension") != "" {
			return nil, unsupported(after, "extension-owned routine must be maintained by its extension")
		}
		parsed, e := validateRoutineSource(after, options)
		if e != nil {
			return nil, e
		}
		sql := parsed.SQL
		if !parsed.statement.GetReplace() {
			upper := strings.ToUpper(sql)
			createPrefix := "CREATE " + routineKeyword(after)
			position := strings.Index(upper, createPrefix)
			if position != 0 {
				return nil, unsupported(after, "routine replacement source is not canonical")
			}
			sql = "CREATE OR REPLACE " + routineKeyword(after) + sql[len(createPrefix):]
		}
		out := []string{sql}
		if beforeOwner, afterOwner := stringValue(bs, "owner"), stringValue(as, "owner"); beforeOwner != afterOwner {
			if afterOwner == "" {
				return nil, unsupported(after, "function owner removal")
			}
			signature, signatureErr := routineSignature(after)
			if signatureErr != nil {
				return nil, signatureErr
			}
			out = append(out, "ALTER "+routineKeyword(after)+" "+signature+" OWNER TO "+quote(afterOwner))
		}
		return out, nil
	case schema.KindTrigger:
		if triggerEnableOnly(bs, as) {
			statement, e := renderTriggerEnable(after, resources)
			if e != nil {
				return nil, e
			}
			return []string{statement}, nil
		}
		if !enabled(options, "allow_rebuild", false) {
			return nil, unsupported(after, "trigger definition change requires allow_rebuild=true")
		}
		drop, e := renderDrop(before, resources, options)
		if e != nil {
			return nil, e
		}
		create, e := renderCreate(after, resources, options)
		return append(drop, create...), e
	case schema.KindPrimaryKey, schema.KindUniqueConstraint, schema.KindCheckConstraint, schema.KindForeignKey:
		if constraintValidationOnly(bs, as) {
			if err != nil {
				return nil, err
			}
			return []string{"ALTER TABLE " + parent + " VALIDATE CONSTRAINT " + quote(after.Name.Name)}, nil
		}
		if !enabled(options, "allow_rebuild", false) {
			return nil, unsupported(after, "rebuild requires allow_rebuild=true")
		}
		drop, e := renderDrop(before, resources, options)
		if e != nil {
			return nil, e
		}
		create, e := renderCreate(after, resources, options)
		return append(drop, create...), e
	case schema.KindMaterializedView:
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
		if err := validateExtensionOptions(after, options); err != nil {
			return nil, err
		}
		version := stringValue(as, "version")
		if version == "" || version == stringValue(bs, "version") {
			return nil, unsupported(after, "extension alteration")
		}
		return []string{"ALTER EXTENSION " + quote(after.Name.Name) + " UPDATE TO " + literal(version)}, nil
	case schema.KindComposite:
		return renderCompositeAlter(before, after, options)
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
		if !allowedKeys(bs, "partitioned", "persistence", "row_security", "force_row_security", "owner") || !allowedKeys(as, "partitioned", "persistence", "row_security", "force_row_security", "owner") {
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

func constraintValidationOnly(before, after map[string]any) bool {
	beforeValidated, beforeOK := before["validated"].(bool)
	afterValidated, afterOK := after["validated"].(bool)
	if !beforeOK || !afterOK || beforeValidated || !afterValidated {
		return false
	}
	for key, beforeValue := range before {
		if key == "validated" {
			continue
		}
		afterValue, ok := after[key]
		if !ok {
			return false
		}
		if key == "definition" {
			beforeDefinition, beforeString := beforeValue.(string)
			afterDefinition, afterString := afterValue.(string)
			if !beforeString || !afterString || strings.TrimSuffix(beforeDefinition, " NOT VALID") != afterDefinition {
				return false
			}
			continue
		}
		if fmt.Sprint(beforeValue) != fmt.Sprint(afterValue) {
			return false
		}
	}
	for key := range after {
		if key != "validated" {
			if _, ok := before[key]; !ok {
				return false
			}
		}
	}
	return true
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

func renderCreateIndex(r schema.Resource, resources map[string]schema.Resource, options map[string]string) ([]string, error) {
	parsed, err := parseIndexDefinition(r, resources)
	if err != nil {
		return nil, err
	}
	if err := validateIndexAvailability(r, parsed, options); err != nil {
		return nil, err
	}
	if enabled(options, "concurrent_indexes", false) {
		parsed.statement.Concurrent = true
		sql, deparseErr := pg_query.Deparse(parsed.tree)
		if deparseErr != nil {
			return nil, unsupported(r, "concurrent index definition could not be canonicalized")
		}
		parsed.SQL = sql
	}
	if parsed.statement.GetNullsNotDistinct() {
		if err := requirePostgresMajor(r, options, 15, "NULLS NOT DISTINCT index"); err != nil {
			return nil, err
		}
	}
	return []string{parsed.SQL}, nil
}

func renderConcurrentIndexRebuild(before, after schema.Resource, resources map[string]schema.Resource, options map[string]string) ([]string, error) {
	parsed, err := parseIndexDefinition(after, resources)
	if err != nil {
		return nil, err
	}
	if err := validateIndexAvailability(after, parsed, options); err != nil {
		return nil, err
	}
	shadow := "autosql_rebuild_" + strings.TrimPrefix(after.ID, string(after.Kind)+":")
	if len(shadow) > 63 {
		shadow = shadow[:63]
	}
	parsed.statement.Idxname = shadow
	parsed.statement.Concurrent = true
	create, err := pg_query.Deparse(parsed.tree)
	if err != nil {
		return nil, unsupported(after, "concurrent shadow index could not be canonicalized")
	}
	drop := "DROP INDEX CONCURRENTLY " + qualified(before.Name)
	rename := "ALTER INDEX " + quote(after.Name.Schema) + "." + quote(shadow) + " RENAME TO " + quote(after.Name.Name)
	return []string{create, drop, rename}, nil
}

func requirePostgresMajor(resource schema.Resource, options map[string]string, minimum int, feature string) error {
	value := strings.TrimSpace(options["postgres_version"])
	major, err := strconv.Atoi(strings.SplitN(value, ".", 2)[0])
	if err != nil || major < minimum {
		return unsupported(resource, fmt.Sprintf("%s requires postgres_version >= %d", feature, minimum))
	}
	return nil
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
	decoder := json.NewDecoder(strings.NewReader(string(r.Spec)))
	decoder.UseNumber()
	_ = decoder.Decode(&m)
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
func validPositiveOrdinal(values map[string]any, key string) bool {
	value, ok := numberValue(values, key)
	if !ok {
		return false
	}
	ordinal, canonical := canonicalUnsigned(value)
	return canonical && ordinal > 0
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

func resourceSQLSemanticsChanged(before, after schema.Resource) bool {
	strip := func(resource schema.Resource) schema.Resource {
		resource.Source = nil
		if len(resource.Annotations) > 0 {
			annotations := make(map[string]string, len(resource.Annotations))
			for key, value := range resource.Annotations {
				if key != "comment" {
					annotations[key] = value
				}
			}
			resource.Annotations = annotations
		}
		return resource
	}
	left, leftErr := schema.ResourceFingerprint(strip(before))
	right, rightErr := schema.ResourceFingerprint(strip(after))
	return leftErr != nil || rightErr != nil || left != right
}

func renderCommentChange(change schema.Change, resources map[string]schema.Resource) ([]string, error) {
	if change.Operation == schema.OperationDrop || change.After == nil {
		return nil, nil
	}
	beforeComment := ""
	if change.Before != nil {
		beforeComment = change.Before.Annotations["comment"]
	}
	afterComment := change.After.Annotations["comment"]
	if change.Operation != schema.OperationCreate && beforeComment == afterComment {
		return nil, nil
	}
	if change.Operation == schema.OperationCreate && afterComment == "" {
		return nil, nil
	}
	target, err := commentTarget(*change.After, resources)
	if err != nil {
		return nil, err
	}
	value := "NULL"
	if afterComment != "" {
		value = literal(afterComment)
	}
	return []string{"COMMENT ON " + target + " IS " + value}, nil
}

func commentTarget(resource schema.Resource, resources map[string]schema.Resource) (string, error) {
	name := qualified(resource.Name)
	switch resource.Kind {
	case schema.KindSchema:
		return "SCHEMA " + quote(resource.Name.Name), nil
	case schema.KindExtension:
		return "EXTENSION " + quote(resource.Name.Name), nil
	case schema.KindEnum:
		return "TYPE " + name, nil
	case schema.KindComposite:
		return "TYPE " + name, nil
	case schema.KindDomain:
		return "DOMAIN " + name, nil
	case schema.KindSequence:
		return "SEQUENCE " + name, nil
	case schema.KindTable:
		return "TABLE " + name, nil
	case schema.KindView:
		return "VIEW " + name, nil
	case schema.KindMaterializedView:
		return "MATERIALIZED VIEW " + name, nil
	case schema.KindColumn:
		parent, err := parentName(resource, resources)
		if err != nil {
			return "", err
		}
		return "COLUMN " + parent + "." + quote(resource.Name.Name), nil
	case schema.KindPrimaryKey, schema.KindUniqueConstraint, schema.KindCheckConstraint, schema.KindForeignKey:
		parent, err := parentName(resource, resources)
		if err != nil {
			return "", err
		}
		return "CONSTRAINT " + quote(resource.Name.Name) + " ON " + parent, nil
	case schema.KindIndex:
		return "INDEX " + name, nil
	case schema.KindFunction, schema.KindProcedure:
		signature, err := routineSignature(resource)
		if err != nil {
			return "", err
		}
		return routineKeyword(resource) + " " + signature, nil
	case schema.KindTrigger:
		parent, err := parentName(resource, resources)
		if err != nil {
			return "", err
		}
		return "TRIGGER " + quote(resource.Name.Name) + " ON " + parent, nil
	case schema.KindPolicy:
		parent, err := parentName(resource, resources)
		if err != nil {
			return "", err
		}
		return "POLICY " + quote(resource.Name.Name) + " ON " + parent, nil
	case schema.KindRole:
		return "ROLE " + quote(resource.Name.Name), nil
	default:
		return "", unsupported(resource, "comments are not managed for this resource kind")
	}
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
	for key := range r.Annotations {
		if key != "comment" {
			return unsupported(r, "annotation "+key+" is not renderable")
		}
	}
	if len(r.Extra) > 0 || len(r.Name.Extra) > 0 {
		return unsupported(r, "extension metadata is not renderable")
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
	scope := defaultRenderScope(request)
	roleRenames := map[string]string{}
	for _, change := range request.Changes.Changes {
		if change.Before != nil && change.After != nil && change.After.Kind == schema.KindRole && change.Before.ID != change.After.ID {
			roleRenames[change.Before.ID] = change.After.ID
		}
	}
	for docIndex, doc := range []schema.Document{request.Current, request.Desired} {
		resources := resourceMapForRender(doc)
		if docIndex == 1 && len(roleRenames) > 0 {
			for id, resource := range resources {
				if resource.Kind != schema.KindMembership {
					continue
				}
				resource.Dependencies = append([]schema.Dependency(nil), resource.Dependencies...)
				for index := range resource.Dependencies {
					if target := roleRenames[resource.Dependencies[index].Target]; target != "" {
						resource.Dependencies[index].Target = target
					}
				}
				resources[id] = resource
			}
		}
		if e := validateCoreColumnOrdinals(resources); e != nil {
			return e
		}
		for _, r := range doc.Graph.Resources {
			if rewritten, ok := resources[r.ID]; ok {
				r = rewritten
			}
			mode := New().Info().Capability(r.Kind).Mode
			if mode == plugin.Managed {
				if scope[r.ID] {
					if e := validateManagedMetadata(r); e != nil {
						return e
					}
					if e := validateCanonicalIdentity(r, resources); e != nil {
						return e
					}
					if e := validateSemanticDependencies(r, resources); e != nil {
						return e
					}
					if e := validateOwnerDependency(r, resources); e != nil {
						return e
					}
				}
				s := spec(r)
				switch r.Kind {
				case schema.KindSchema:
					if !allowedKeys(s, "owner") {
						return unsupported(r, "unknown schema semantics")
					}
				case schema.KindExtension:
					if docIndex == 1 && scope[r.ID] {
						if e := validateExtensionOptions(r, request.Options); e != nil {
							return e
						}
					}
				case schema.KindTable:
					if !allowedKeys(s, "partitioned", "persistence", "row_security", "force_row_security", "owner") {
						return unsupported(r, "unknown table semantics")
					}
					if e := validateTableSpec(s); e != nil {
						return unsupported(r, e.Error())
					}
					if boolValue(s, "partitioned") || stringValue(s, "persistence") != "p" && stringValue(s, "persistence") != "" {
						return unsupported(r, "table storage is outside managed matrix")
					}
				case schema.KindEnum:
					if docIndex == 1 && scope[r.ID] && (!allowedKeys(s, "values", "owner") || len(stringSlice(s, "values")) == 0) {
						return unsupported(r, "enum values must be a non-empty string list")
					}
				case schema.KindDomain:
					if docIndex == 1 && scope[r.ID] {
						if e := validateDomainSpec(r); e != nil {
							return e
						}
					}
				case schema.KindComposite:
					if docIndex == 1 && scope[r.ID] {
						if e := validateCompositeSpec(r, resources); e != nil {
							return e
						}
					}
				case schema.KindSequence:
					if docIndex == 1 && scope[r.ID] {
						if e := validateSequenceSpec(r); e != nil {
							return e
						}
					}
				case schema.KindView, schema.KindMaterializedView:
					if !allowedKeys(s, "definition", "owner") {
						return unsupported(r, "unknown view semantics")
					}
					if e := validateSQLFragment(stringValue(s, "definition")); e != nil {
						return unsupported(r, e.Error())
					}
					if e := validateProjectionShape(r, resources); e != nil {
						return unsupported(r, e.Error())
					}
				case schema.KindColumn:
					if !allowedKeys(s, "type", "default", "not_null", "ordinal", "identity", "generated") {
						return unsupported(r, "unknown column semantics")
					}
					if _, ok := s["type"].(string); !ok {
						return unsupported(r, "column type must be a string")
					}
					if _, ok := s["not_null"].(bool); !ok {
						return unsupported(r, "column not_null must be boolean")
					}
					if !validPositiveOrdinal(s, "ordinal") {
						return unsupported(r, "column ordinal must be a positive integer")
					}
					// Current resources and unchanged desired resources are structural
					// context, not SQL inputs. Validate default renderability only for
					// the desired mutation/dependency closure.
					if docIndex == 1 && scope[r.ID] {
						if e := validateIdentityColumn(r); e != nil {
							return e
						}
						if e := validateCoreColumnType(r, resources); e != nil {
							return e
						}
						if stringValue(s, "generated") != "" {
							if e := validateGeneratedColumnCreate(r, resources); e != nil {
								return e
							}
						} else if d := stringValue(s, "default"); d != "" {
							if e := validateColumnDefault(r, d, resources); e != nil {
								return e
							}
						}
					}
				case schema.KindPrimaryKey, schema.KindUniqueConstraint, schema.KindCheckConstraint, schema.KindForeignKey, schema.KindIndex:
					if docIndex == 1 && scope[r.ID] {
						if e := validateConstraintIndexSpec(r, resources); e != nil {
							return e
						}
					}
				case schema.KindFunction, schema.KindProcedure:
					if !allowedKeys(s, "name", "identity_arguments", "arguments", "result", "returns_set", "language", "volatility", "strict", "security_definer", "leakproof", "parallel", "cost", "rows", "configuration", "owner", "definition", "body_digest", "extension") {
						return unsupported(r, "unknown routine semantics")
					}
				case schema.KindTrigger:
					if docIndex == 1 && scope[r.ID] {
						if e := validateTriggerSpec(r, resources); e != nil {
							return e
						}
					}
				case schema.KindPolicy:
					if docIndex == 1 && scope[r.ID] {
						if _, e := parsePolicy(r, resources); e != nil {
							return e
						}
					}
				case schema.KindRole:
					if docIndex == 1 && scope[r.ID] {
						if e := validateRoleSpec(r, request.Options); e != nil {
							return e
						}
					}
				case schema.KindMembership:
					if docIndex == 1 && scope[r.ID] {
						membershipOptions := request.Options
						if len(roleRenames) > 0 {
							membershipOptions = cloneOptions(request.Options)
							membershipOptions["__membership_role_rename"] = "true"
							for before, after := range roleRenames {
								membershipOptions["__role_rename."+before] = after
							}
						}
						if e := validateMembershipSpec(r, resources, membershipOptions); e != nil {
							return e
						}
					}
				case schema.KindGrant:
					if docIndex == 1 && scope[r.ID] {
						if _, e := parseGrant(r, resources); e != nil {
							return e
						}
					}
				case schema.KindDefaultPrivilege:
					if docIndex == 1 && scope[r.ID] {
						if _, e := parseDefaultPrivilege(r, resources); e != nil {
							return e
						}
					}
				}
			} else if r.Kind == schema.KindColumn && isManagedProjectionParent(r.Name.Parent, resources) {
				if scope[r.ID] {
					if e := validateManagedMetadata(r); e != nil {
						return e
					}
				}
				if e := validateProjectionResource(r, r.Name.Parent); e != nil {
					return e
				}
			}
		}
		if docIndex == 1 {
			if e := validateMembershipCycles(resources); e != nil {
				return e
			}
		}
	}
	return nil
}

func validateIdentityColumn(resource schema.Resource) error {
	values := spec(resource)
	identity := stringValue(values, "identity")
	if identity == "" {
		return nil
	}
	if identity != "a" && identity != "d" {
		return unsupported(resource, "identity must normalize to a or d")
	}
	if stringValue(values, "default") != "" || stringValue(values, "generated") != "" {
		return unsupported(resource, "identity cannot be combined with default or generated semantics")
	}
	return nil
}

var canonicalDomainCheck = regexp.MustCompile(`^CHECK \(\(*VALUE (?:=|<>|!=|<|<=|>|>=) (?:-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?|'(?:''|[^'])*')\)*\)$`)

func validateDomainSpec(resource schema.Resource) error {
	s := spec(resource)
	if !allowedKeys(s, "base_type", "default", "not_null", "constraints", "owner") {
		return unsupported(resource, "unknown domain semantics")
	}
	baseName := postgresTypeAlias(stringValue(s, "base_type"))
	base, ok := parseCoreColumnType(baseName)
	if !ok || base.array {
		return unsupported(resource, "domain base_type is outside canonical core grammar")
	}
	if _, ok := s["not_null"].(bool); !ok {
		return unsupported(resource, "domain not_null must be boolean")
	}
	if value := stringValue(s, "default"); value != "" {
		probe := resource
		probe.Kind = schema.KindColumn
		probe.Spec, _ = json.Marshal(map[string]any{"type": baseName, "default": value})
		if err := validateCoreDefault(probe, value); err != nil {
			return unsupported(resource, "domain default is outside canonical base-type grammar")
		}
	}
	constraints, present := s["constraints"]
	if !present {
		return nil
	}
	values, ok := constraints.([]any)
	if !ok {
		return unsupported(resource, "domain constraints must be a string list")
	}
	for _, raw := range values {
		constraint, ok := raw.(string)
		if !ok || !canonicalDomainCheck.MatchString(constraint) {
			return unsupported(resource, "domain constraint is outside canonical literal-check grammar")
		}
	}
	return nil
}

func validateSequenceSpec(resource schema.Resource) error {
	s := spec(resource)
	if !allowedKeys(s, "start", "increment", "min", "max", "cache", "cycle", "owner") {
		return unsupported(resource, "unknown sequence semantics")
	}
	for _, key := range []string{"start", "increment", "min", "max", "cache"} {
		if _, present := s[key]; present {
			value, ok := numberValue(s, key)
			number, parseErr := strconv.ParseInt(value, 10, 64)
			if !ok || !canonicalIntegerDefault.MatchString(value) || parseErr != nil || number == 0 && key == "increment" || number <= 0 && key == "cache" {
				return unsupported(resource, "sequence "+key+" must be a canonical integer")
			}
		}
	}
	if _, present := s["cycle"]; present {
		if _, ok := s["cycle"].(bool); !ok {
			return unsupported(resource, "sequence cycle must be boolean")
		}
	}
	return nil
}

func defaultRenderScope(request plugin.RenderRequest) map[string]bool {
	scope := map[string]bool{}
	for _, change := range request.Changes.Changes {
		scope[change.ResourceID] = true
		if change.Before != nil {
			scope[change.Before.ID] = true
		}
		if change.After != nil {
			scope[change.After.ID] = true
		}
	}
	// Type and sequence changes can require dependent columns to be copied or
	// rebuilt. Containment edges are intentionally excluded: a schema or table
	// change does not render every unchanged child column.
	for changed := true; changed; {
		changed = false
		for _, doc := range []schema.Document{request.Current, request.Desired} {
			for _, resource := range doc.Graph.Resources {
				for _, dependency := range resource.Dependencies {
					if dependency.Type != schema.DependencyUses && dependency.Type != schema.DependencyReferences {
						continue
					}
					if scope[resource.ID] && !scope[dependency.Target] {
						scope[dependency.Target], changed = true, true
					}
					if scope[dependency.Target] && !scope[resource.ID] {
						scope[resource.ID], changed = true, true
					}
				}
			}
		}
	}
	return scope
}
func validateCoreColumnType(r schema.Resource, resources map[string]schema.Resource) error {
	for _, dep := range r.Dependencies {
		if dep.Type == schema.DependencyUses {
			return nil
		}
	}
	if _, ok := parseCoreColumnType(stringValue(spec(r), "type")); !ok {
		return unsupported(r, "column type is outside canonical core grammar")
	}
	return nil
}

type coreColumnType struct {
	base, modifier string
	array          bool
}

func parseCoreColumnType(value string) (coreColumnType, bool) {
	typ := coreColumnType{}
	if strings.HasSuffix(value, "[]") {
		typ.array = true
		value = strings.TrimSuffix(value, "[]")
	}
	if strings.Contains(value, "[]") {
		return coreColumnType{}, false
	}
	if open := strings.IndexByte(value, '('); open >= 0 {
		if !strings.HasSuffix(value, ")") || strings.Count(value, "(") != 1 || strings.Count(value, ")") != 1 {
			return coreColumnType{}, false
		}
		typ.base = value[:open]
		typ.modifier = value[open+1 : len(value)-1]
	} else {
		typ.base = value
	}
	allowed := map[string]bool{
		"smallint": true, "integer": true, "bigint": true, "real": true, "double precision": true, "numeric": true,
		"boolean": true, "text": true, "character": true, "character varying": true, "bit": true, "bit varying": true,
		"date": true, "time": true, "timetz": true, "timestamp": true, "timestamptz": true,
		"interval": true, "interval year": true, "interval month": true, "interval day": true, "interval hour": true,
		"interval minute": true, "interval second": true, "interval year to month": true, "interval day to hour": true,
		"interval day to minute": true, "interval day to second": true, "interval hour to minute": true,
		"interval hour to second": true, "interval minute to second": true,
		"uuid": true, "json": true, "jsonb": true, "bytea": true,
		"cidr": true, "inet": true, "macaddr": true,
	}
	if !allowed[typ.base] {
		return coreColumnType{}, false
	}
	if typ.modifier == "" {
		return typ, true
	}
	switch typ.base {
	case "character", "character varying":
		length, ok := canonicalUnsigned(typ.modifier)
		return typ, ok && length >= 1 && length <= 10485760
	case "bit", "bit varying":
		length, ok := canonicalUnsigned(typ.modifier)
		return typ, ok && length >= 1 && length <= 83886080
	case "numeric":
		parts := strings.Split(typ.modifier, ",")
		if len(parts) < 1 || len(parts) > 2 {
			return coreColumnType{}, false
		}
		precision, ok := canonicalUnsigned(parts[0])
		if !ok || precision < 1 || precision > 1000 {
			return coreColumnType{}, false
		}
		if len(parts) == 2 {
			scale, scaleOK := canonicalUnsigned(parts[1])
			if !scaleOK || scale > precision {
				return coreColumnType{}, false
			}
		}
		return typ, true
	case "time", "timetz", "timestamp", "timestamptz":
		precision, ok := canonicalUnsigned(typ.modifier)
		return typ, ok && precision <= 6
	default:
		if typ.base == "interval" || strings.HasSuffix(typ.base, "second") {
			precision, ok := canonicalUnsigned(typ.modifier)
			return typ, ok && precision <= 6
		}
		return coreColumnType{}, false
	}
}

func canonicalUnsigned(value string) (int, bool) {
	if value == "" || len(value) > 1 && value[0] == '0' {
		return 0, false
	}
	for _, ch := range value {
		if ch < '0' || ch > '9' {
			return 0, false
		}
	}
	n, err := strconv.Atoi(value)
	return n, err == nil
}

func validateCoreDefault(r schema.Resource, value string) error {
	typ, validType := parseCoreColumnType(stringValue(spec(r), "type"))
	if !validType {
		return unsupported(r, fmt.Sprintf("default rejected: normalized type %q is outside canonical core grammar", stringValue(spec(r), "type")))
	}
	expr, err := classifyDefaultExpression(value)
	if err != nil {
		return unsupported(r, fmt.Sprintf("default rejected for normalized type %q: %s", stringValue(spec(r), "type"), err.Error()))
	}
	if containsDefaultOperator(expr) {
		if err := validateCoreOperatorDefault(typ, expr, value); err != nil {
			return unsupported(r, fmt.Sprintf("default rejected for normalized type %q: %s", stringValue(spec(r), "type"), err.Error()))
		}
		return nil
	}
	if !coreDefaultAllowed(typ, expr, value) {
		return unsupported(r, fmt.Sprintf("default rejected for normalized type %q: %s is not allowlisted", stringValue(spec(r), "type"), defaultExpressionClass(expr)))
	}
	return nil
}

func defaultExpressionClass(expr defaultExpression) string {
	switch expr.Kind {
	case defaultExpressionLiteral:
		return "literal"
	case defaultExpressionCast:
		if expr.Cast != nil {
			return "cast to " + strings.Join(expr.Cast.Type.Name.Parts, ".")
		}
		return "cast"
	case defaultExpressionFunction:
		if expr.Function != nil {
			return "function " + strings.Join(expr.Function.Name.Parts, ".")
		}
		return "function"
	case defaultExpressionOperator:
		if expr.Operator != nil {
			if expr.Operator.Left == nil {
				return "unary operator " + expr.Operator.Name
			}
			return "binary operator " + expr.Operator.Name
		}
		return "operator"
	case defaultExpressionReference:
		return "reference"
	case defaultExpressionArray:
		return "array expression"
	default:
		return "expression"
	}
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
		if stringValue(spec(r), "generated") != "" {
			if err := validateGeneratedDependencies(r, resources); err != nil {
				return err
			}
		} else if err := validateDefaultRoutineDependencies(r, resources); err != nil {
			return err
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
	case schema.KindComposite:
		expectedType = schema.DependencyUses
		attributes, err := parseCompositeAttributes(r)
		if err != nil {
			return err
		}
		for _, attribute := range attributes {
			for id, candidate := range resources {
				if candidate.ID == r.ID {
					continue
				}
				switch candidate.Kind {
				case schema.KindEnum, schema.KindDomain, schema.KindComposite:
					if typeReferenceMatches(attribute.Type, r.Name.Schema, candidate.Name) {
						expected = append(expected, id)
					}
				}
			}
		}
	case schema.KindPrimaryKey, schema.KindUniqueConstraint, schema.KindCheckConstraint, schema.KindForeignKey, schema.KindIndex:
		expectedType = schema.DependencyContains
		var err error
		expected, err = constraintIndexExpectedDependencies(r, resources)
		if err != nil {
			return err
		}
	case schema.KindTrigger:
		expectedType = schema.DependencyContains
		var err error
		expected, err = triggerExpectedDependencies(r, resources)
		if err != nil {
			return err
		}
	case schema.KindPolicy:
		expectedType = schema.DependencyContains
		var err error
		expected, err = policyExpectedDependencies(r, resources)
		if err != nil {
			return err
		}
	default:
		return nil
	}
	var actual []string
	for _, dep := range r.Dependencies {
		if extensionDependency(dep, resources) {
			continue
		}
		dependentObject := r.Kind == schema.KindPrimaryKey || r.Kind == schema.KindUniqueConstraint || r.Kind == schema.KindCheckConstraint || r.Kind == schema.KindForeignKey || r.Kind == schema.KindIndex || r.Kind == schema.KindTrigger || r.Kind == schema.KindPolicy
		if dep.Type == expectedType || dependentObject && dep.Type == schema.DependencyReferences {
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
		if r.Name.Schema != "" || r.Name.Parent != "" {
			return unsupported(r, "schema name/parent/dependencies are noncanonical")
		}
		for _, dependency := range r.Dependencies {
			if dependency.Type != schema.DependencyOwns {
				return unsupported(r, "schema dependencies are noncanonical")
			}
		}
	case schema.KindRole:
		if r.Name.Schema != "" || r.Name.Parent != "" || len(r.Dependencies) != 0 {
			return unsupported(r, "role identity/dependencies are noncanonical")
		}
	case schema.KindMembership:
		if r.Name.Schema != "" || r.Name.Parent != "" {
			return unsupported(r, "membership identity is noncanonical")
		}
	case schema.KindGrant:
		if r.Name.Parent == "" {
			return unsupported(r, "grant target parent is required")
		}
	case schema.KindExtension, schema.KindTable, schema.KindView, schema.KindMaterializedView, schema.KindEnum, schema.KindDomain, schema.KindComposite, schema.KindSequence:
		parent, ok := resources[r.Name.Parent]
		if !ok || parent.Kind != schema.KindSchema || parent.Name.Name != r.Name.Schema || r.Name.Schema == "" {
			return unsupported(r, "schema parent is noncanonical")
		}
		contains := 0
		for _, dep := range r.Dependencies {
			if extensionDependency(dep, resources) {
				continue
			}
			if dep.Target == parent.ID && dep.Type == schema.DependencyContains {
				contains++
				continue
			}
			if (r.Kind == schema.KindView || r.Kind == schema.KindMaterializedView) && dep.Type == schema.DependencyReferences {
				if _, ok := resources[dep.Target]; ok {
					continue
				}
			}
			if dep.Type == schema.DependencyOwns {
				continue
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
			if extensionDependency(dep, resources) {
				continue
			}
			if dep.Target == parent.ID && dep.Type == schema.DependencyContains {
				contains++
				continue
			}
			if dep.Type == schema.DependencyUses {
				if _, ok := resources[dep.Target]; ok {
					continue
				}
			}
			if dep.Type == schema.DependencyReferences {
				if target, ok := resources[dep.Target]; ok {
					if target.Kind == schema.KindSequence {
						continue
					}
					if stringValue(spec(r), "generated") == "" && stringValue(spec(r), "default") != "" && target.Kind == schema.KindFunction {
						continue
					}
					if stringValue(spec(r), "generated") == "s" && (target.Kind == schema.KindFunction || target.Kind == schema.KindColumn && target.Name.Parent == r.Name.Parent) {
						continue
					}
				}
			}
			return unsupported(r, "column dependencies are noncanonical")
		}
		if contains != 1 {
			return unsupported(r, "column requires exactly one parent containment dependency")
		}
	case schema.KindPolicy:
		parent, ok := resources[r.Name.Parent]
		if !ok || parent.Kind != schema.KindTable || r.Name.Schema != parent.Name.Schema {
			return unsupported(r, "policy table parent is noncanonical")
		}
	}
	return nil
}
func validateOwnerDependency(resource schema.Resource, resources map[string]schema.Resource) error {
	if resource.Kind == schema.KindDefaultPrivilege {
		return nil
	}
	owner := stringValue(spec(resource), "owner")
	var actual []string
	for _, dependency := range resource.Dependencies {
		if dependency.Type == schema.DependencyOwns && resources[dependency.Target].Kind == schema.KindRole {
			actual = append(actual, dependency.Target)
		}
	}
	if owner == "" {
		if len(actual) != 0 {
			return unsupported(resource, "owner dependency exists without owner")
		}
		return nil
	}
	// Routine ownership predates managed cluster-role inspection. Preserve
	// stable adoption when roles were intentionally not selected; advanced
	// inventories carry the exact OWNS edge and are checked below.
	if (resource.Kind == schema.KindFunction || resource.Kind == schema.KindProcedure) && len(actual) == 0 {
		return nil
	}
	expected, err := roleOwnerDependency(owner, resources)
	if err != nil || len(actual) != 1 || actual[0] != expected {
		return unsupported(resource, "owner dependency does not exactly match declared owner")
	}
	return nil
}

func extensionDependency(dependency schema.Dependency, resources map[string]schema.Resource) bool {
	return resources[dependency.Target].Kind == schema.KindExtension && (dependency.Type == schema.DependencyOwns || dependency.Type == schema.DependencyUses || dependency.Type == schema.DependencyReferences)
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
	if !validPositiveOrdinal(s, "ordinal") {
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
