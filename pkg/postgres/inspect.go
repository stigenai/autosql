package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"regexp"
	"strings"
	"time"

	"autosql/pkg/plugin"
	"autosql/pkg/schema"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

type inspector struct {
	conn      catalogQueryer
	resources []schema.Resource
	byOID     map[uint32]string
	byCatalog map[catalogOID]string
	schemas   map[string]string
	columns   map[columnCatalogKey]string
	advanced  bool
}

type catalogOID struct {
	catalog string
	oid     uint32
}

type columnCatalogKey struct {
	relation uint32
	position int16
}

func inspect(ctx context.Context, req plugin.InspectRequest) (schema.Document, error) {
	if strings.TrimSpace(req.URL) == "" {
		return schema.Document{}, errors.New("inspect PostgreSQL database: URL is required")
	}
	conn, err := pgx.Connect(ctx, req.URL)
	if err != nil {
		return schema.Document{}, classify("database", "CONNECT", req.URL, err)
	}
	defer conn.Close(context.Background())
	return inspectConn(ctx, conn, req)
}

func inspectConn(ctx context.Context, conn *pgx.Conn, req plugin.InspectRequest) (schema.Document, error) {
	if conn.PgConn().TxStatus() != 'I' {
		return schema.Document{}, errors.New("inspect PostgreSQL database: connection must be idle")
	}
	begin := func(ctx context.Context) (catalogQueryer, func(context.Context) error, error) {
		tx, err := conn.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
		if err != nil {
			return nil, nil, err
		}
		return tx, tx.Rollback, nil
	}
	return inspectTransactions(ctx, req, begin, inspectSnapshot)
}

type catalogQueryer interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
}
type snapshotBegin func(context.Context) (catalogQueryer, func(context.Context) error, error)
type snapshotInspect func(context.Context, catalogQueryer, plugin.InspectRequest) (schema.Document, error)

type snapshotRollbackError struct{ cause error }

func (e *snapshotRollbackError) Error() string {
	return "inspect PostgreSQL database: rollback catalog snapshot"
}
func (e *snapshotRollbackError) Unwrap() error { return e.cause }

func inspectTransactions(ctx context.Context, req plugin.InspectRequest, begin snapshotBegin, run snapshotInspect) (schema.Document, error) {
	return inspectTransactionsWithRetry(ctx, req, begin, run, catalogRetryPolicy{
		maxAttempts: 12,
		maxBackoff:  2 * time.Second,
		delay: func(retry int) time.Duration {
			d := 5 * time.Millisecond * time.Duration(1<<min(retry, 6))
			return min(d, 250*time.Millisecond)
		},
		sleep: sleepCatalogRetry,
	})
}

type catalogRetryPolicy struct {
	maxAttempts int
	maxBackoff  time.Duration
	delay       func(int) time.Duration
	sleep       func(context.Context, time.Duration) error
}

type catalogRetryExhaustedError struct {
	attempts int
	cause    error
}

func (e *catalogRetryExhaustedError) Error() string {
	return fmt.Sprintf("PostgreSQL catalog remained unstable after %d inspection attempts: %v", e.attempts, e.cause)
}
func (e *catalogRetryExhaustedError) Unwrap() error { return e.cause }

func sleepCatalogRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func inspectTransactionsWithRetry(ctx context.Context, req plugin.InspectRequest, begin snapshotBegin, run snapshotInspect, retry catalogRetryPolicy) (schema.Document, error) {
	if retry.maxAttempts < 1 || retry.maxBackoff < 0 || retry.delay == nil || retry.sleep == nil {
		return schema.Document{}, errors.New("inspect PostgreSQL database: invalid catalog retry policy")
	}
	var last error
	var backoff time.Duration
	for attempt := 0; attempt < retry.maxAttempts; attempt++ {
		queryer, rollback, err := begin(ctx)
		if err != nil {
			return schema.Document{}, err
		}
		doc, inspectErr := run(ctx, queryer, req)
		rollbackErr := rollback(context.WithoutCancel(ctx))
		if rollbackErr != nil && !errors.Is(rollbackErr, pgx.ErrTxClosed) {
			typed := &snapshotRollbackError{cause: rollbackErr}
			if inspectErr != nil {
				return schema.Document{}, errors.Join(inspectErr, typed)
			}
			return schema.Document{}, typed
		}
		if inspectErr == nil {
			return doc, nil
		}
		last = inspectErr
		if !transientCatalogOID(inspectErr) {
			return schema.Document{}, inspectErr
		}
		if attempt+1 == retry.maxAttempts {
			return schema.Document{}, &catalogRetryExhaustedError{attempts: attempt + 1, cause: inspectErr}
		}
		delay := retry.delay(attempt)
		if delay < 0 || backoff+delay > retry.maxBackoff {
			return schema.Document{}, &catalogRetryExhaustedError{attempts: attempt + 1, cause: inspectErr}
		}
		backoff += delay
		if err = retry.sleep(ctx, delay); err != nil {
			return schema.Document{}, err
		}
	}
	return schema.Document{}, &catalogRetryExhaustedError{attempts: retry.maxAttempts, cause: last}
}

func transientCatalogOID(err error) bool {
	var disappeared *catalogDisappearanceError
	if errors.As(err, &disappeared) {
		return true
	}
	var pgErr *pgconn.PgError
	return errors.As(err, &pgErr) && pgErr.Code == "XX000" && (strings.Contains(pgErr.Message, "could not open relation with OID") || strings.Contains(pgErr.Message, "cache lookup failed for"))
}

type catalogDisappearanceError struct {
	resource string
	oid      uint32
}

func (e *catalogDisappearanceError) Error() string {
	return fmt.Sprintf("PostgreSQL catalog %s disappeared during inspection (OID %d)", e.resource, e.oid)
}

func inspectSnapshot(ctx context.Context, conn catalogQueryer, req plugin.InspectRequest) (schema.Document, error) {
	i := &inspector{conn: conn, byOID: map[uint32]string{}, byCatalog: map[catalogOID]string{}, schemas: map[string]string{}, columns: map[columnCatalogKey]string{}, advanced: enabled(req.Options, "roles", false)}
	steps := []struct {
		resource, privilege string
		fn                  func(context.Context) error
	}{
		{"schemas", "USAGE on the selected schemas", i.inspectSchemas},
		{"extensions", "USAGE on the extension schemas", i.inspectExtensions},
		{"types", "USAGE on the selected schemas and types", i.inspectTypes},
		{"type dependencies", "USAGE on composite attribute types", i.inspectTypeDependencies},
		{"sequences", "USAGE on the selected schemas", i.inspectSequences},
		{"relations", "USAGE on schemas and SELECT on catalog metadata", i.inspectRelations},
		{"partitions", "USAGE on schemas and SELECT on partition metadata", i.inspectPartitions},
		{"relation dependencies", "USAGE on schemas and SELECT on catalog dependency metadata", i.inspectRelationDependencies},
		{"columns", "USAGE on schemas and SELECT on catalog metadata", i.inspectColumns},
		{"column default dependencies", "USAGE on schemas and SELECT on catalog dependency metadata", i.inspectColumnDefaultDependencies},
		{"constraints", "USAGE on schemas and SELECT on catalog metadata", i.inspectConstraints},
		{"indexes", "USAGE on schemas and SELECT on catalog metadata", i.inspectIndexes},
		{"routines", "USAGE on schemas and routines", i.inspectRoutines},
		{"routine dependencies", "USAGE on schemas, routines, types, and relations", i.inspectRoutineDependencies},
		{"column routine dependencies", "USAGE on column defaults and routines", i.inspectColumnRoutineDependencies},
		{"expression dependencies", "USAGE on constraints, indexes, routines, and types", i.inspectExpressionDependencies},
		{"generated column dependencies", "USAGE on schemas, columns, and routines", i.inspectGeneratedColumnDependencies},
		{"triggers", "USAGE on schemas and tables", i.inspectTriggers},
	}
	if enabled(req.Options, "roles", false) {
		steps = append(steps, struct {
			resource, privilege string
			fn                  func(context.Context) error
		}{"roles", "pg_read_all_settings or CREATEROLE", i.inspectRoles})
		steps = append(steps, struct {
			resource, privilege string
			fn                  func(context.Context) error
		}{"role memberships", "visibility of pg_auth_members and pg_roles", i.inspectMemberships})
	}
	if enabled(req.Options, "policies", true) {
		steps = append(steps, struct {
			resource, privilege string
			fn                  func(context.Context) error
		}{"row security policies", "USAGE on schemas and ownership or privileges on policy tables", i.inspectPolicies})
	}
	if enabled(req.Options, "grants", false) {
		steps = append(steps, struct {
			resource, privilege string
			fn                  func(context.Context) error
		}{"grants", "USAGE on schemas and visibility of information_schema grants", i.inspectGrants})
		steps = append(steps, struct {
			resource, privilege string
			fn                  func(context.Context) error
		}{"default privileges", "visibility of pg_default_acl", i.inspectDefaultPrivileges})
	}
	steps = append(steps, struct {
		resource, privilege string
		fn                  func(context.Context) error
	}{"extension ownership", "USAGE on extension-owned catalog metadata", i.inspectExtensionOwnership})
	for _, step := range steps {
		if err := step.fn(ctx); err != nil {
			return schema.Document{}, classify(step.resource, step.privilege, req.URL, err)
		}
	}
	if i.advanced {
		i.attachOwnerDependencies()
	}
	doc := schema.Document{Version: schema.SchemaVersion, Graph: schema.Graph{Resources: i.resources}, Annotations: map[string]string{"dialect": "postgresql"}}
	doc = filterDocument(doc, req.Schemas, splitPatterns(req.Options["include"]), splitPatterns(req.Options["exclude"]))
	doc.Normalize()
	if err := doc.Validate(); err != nil {
		return schema.Document{}, fmt.Errorf("inspect PostgreSQL database: build canonical document: %w", err)
	}
	return doc, nil
}

func (i *inspector) inspectRelationDependencies(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select distinct rw.ev_class::oid,d.refclassid::regclass::text,d.refobjid::oid from pg_rewrite rw join pg_depend d on d.classid='pg_rewrite'::regclass and d.objid=rw.oid join pg_class v on v.oid=rw.ev_class where v.relkind in ('v','m') and d.refclassid in ('pg_class'::regclass,'pg_type'::regclass) and d.deptype='n' and not (d.refclassid='pg_class'::regclass and d.refobjid=rw.ev_class) order by 1,2,3`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var from, to uint32
		var catalog string
		if err := rows.Scan(&from, &catalog, &to); err != nil {
			return err
		}
		fromID, toID := i.byOID[from], i.byOID[to]
		if fromID == "" || toID == "" {
			continue
		}
		dependencyType := schema.DependencyReferences
		if catalog == "pg_type" {
			dependencyType = schema.DependencyUses
		}
		for idx := range i.resources {
			if i.resources[idx].ID != fromID {
				continue
			}
			exists := false
			for _, dep := range i.resources[idx].Dependencies {
				exists = exists || dep.Target == toID && dep.Type == dependencyType
			}
			if !exists {
				i.resources[idx].Dependencies = append(i.resources[idx].Dependencies, schema.Dependency{Target: toID, Type: dependencyType})
			}
		}
	}
	return rows.Err()
}

func safeError(action, dsn string, err error) error {
	msg := err.Error()
	if dsn != "" {
		msg = strings.ReplaceAll(msg, dsn, redactDSN(dsn))
	}
	return fmt.Errorf("%s (%s): %s", action, redactDSN(dsn), msg)
}

func redactDSN(dsn string) string {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		return "<redacted PostgreSQL DSN>"
	}
	host := cfg.Host
	if host == "" {
		host = "localhost"
	}
	db := cfg.Database
	if db == "" {
		db = "postgres"
	}
	user := cfg.User
	if user == "" {
		user = "<default>"
	}
	return fmt.Sprintf("postgresql://%s@%s/%s", user, host, db)
}

func classify(resource, privilege, dsn string, err error) error {
	var pe *pgconn.PgError
	if errors.As(err, &pe) && pe.Code == "42501" {
		return &PermissionError{Resource: resource, Privilege: privilege, Cause: pe}
	}
	if transientCatalogOID(err) {
		return &catalogOIDError{message: safeError("inspect "+resource, dsn, err).Error(), cause: err}
	}
	return safeError("inspect "+resource, dsn, err)
}

type catalogOIDError struct {
	message string
	cause   error
}

func (e *catalogOIDError) Error() string { return e.message }
func (e *catalogOIDError) Unwrap() error { return e.cause }

func (i *inspector) add(kind schema.Kind, name schema.Name, spec any, deps []schema.Dependency, comment *string) string {
	b, _ := json.Marshal(spec)
	r := schema.Resource{Kind: kind, Name: name, Dependencies: deps, Spec: b}
	r.ID = schema.StableID(kind, name)
	if comment != nil && *comment != "" {
		r.Annotations = map[string]string{"comment": *comment}
	}
	i.resources = append(i.resources, r)
	return r.ID
}

func (i *inspector) recordOID(catalog string, oid uint32, id string) {
	i.byOID[oid] = id
	i.byCatalog[catalogOID{catalog: catalog, oid: oid}] = id
}

// inspectExtensionOwnership classifies modeled pg_depend extension members.
// Unreferenced members are represented by their extension version only, which
// prevents CREATE EXTENSION from being followed by duplicate member DDL.
// Members referenced by application resources remain as inert prerequisites
// with an OWNS edge to the extension.
func (i *inspector) inspectExtensionOwnership(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select d.classid::regclass::text,d.objid::oid,d.refobjid::oid from pg_depend d where d.refclassid='pg_extension'::regclass and d.deptype='e' order by 1,2,3`)
	if err != nil {
		return err
	}
	defer rows.Close()
	members := map[string]string{}
	ownedCatalog := map[catalogOID]string{}
	for rows.Next() {
		var catalog string
		var objectOID, extensionOID uint32
		if err := rows.Scan(&catalog, &objectOID, &extensionOID); err != nil {
			return err
		}
		member := i.byCatalog[catalogOID{catalog: catalog, oid: objectOID}]
		extension := i.byCatalog[catalogOID{catalog: "pg_extension", oid: extensionOID}]
		if extension != "" {
			ownedCatalog[catalogOID{catalog: catalog, oid: objectOID}] = extension
		}
		if member != "" && extension != "" && member != extension {
			members[member] = extension
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	// Lift dependencies on unmodeled extension members (operators, casts,
	// base types) to the managed extension resource itself.
	dependencyRows, err := i.conn.Query(ctx, `select d.classid::regclass::text,d.objid::oid,d.refclassid::regclass::text,d.refobjid::oid from pg_depend d where d.deptype in ('n','a') order by 1,2,3,4`)
	if err != nil {
		return err
	}
	defer dependencyRows.Close()
	for dependencyRows.Next() {
		var fromCatalog, targetCatalog string
		var fromOID, targetOID uint32
		if err := dependencyRows.Scan(&fromCatalog, &fromOID, &targetCatalog, &targetOID); err != nil {
			return err
		}
		from := i.byCatalog[catalogOID{catalog: fromCatalog, oid: fromOID}]
		extension := ownedCatalog[catalogOID{catalog: targetCatalog, oid: targetOID}]
		if from == "" || extension == "" || members[from] != "" {
			continue
		}
		dependencyType := schema.DependencyReferences
		if targetCatalog == "pg_type" {
			dependencyType = schema.DependencyUses
		}
		i.appendDependency(from, schema.Dependency{Target: extension, Type: dependencyType})
	}
	if err := dependencyRows.Err(); err != nil {
		return err
	}
	// A relation owned by an extension also owns its modeled child columns,
	// constraints, indexes, triggers, and policies even when pg_depend records
	// ownership only at the parent object.
	for changed := true; changed; {
		changed = false
		for _, resource := range i.resources {
			if members[resource.ID] != "" || members[resource.Name.Parent] == "" {
				continue
			}
			members[resource.ID] = members[resource.Name.Parent]
			changed = true
		}
	}
	if len(members) == 0 {
		return nil
	}
	keep := map[string]bool{}
	for _, resource := range i.resources {
		if members[resource.ID] != "" {
			continue
		}
		for _, dependency := range resource.Dependencies {
			if members[dependency.Target] != "" {
				keep[dependency.Target] = true
			}
		}
	}
	// Retained prerequisites need their member parents and member dependencies.
	for changed := true; changed; {
		changed = false
		for _, resource := range i.resources {
			if !keep[resource.ID] {
				continue
			}
			for _, target := range append([]string{resource.Name.Parent}, dependencyTargets(resource.Dependencies)...) {
				if members[target] != "" && !keep[target] {
					keep[target] = true
					changed = true
				}
			}
		}
	}
	out := make([]schema.Resource, 0, len(i.resources)-len(members)+len(keep))
	for _, resource := range i.resources {
		extension := members[resource.ID]
		if extension != "" && !keep[resource.ID] {
			continue
		}
		if extension != "" {
			resource.Dependencies = append(resource.Dependencies, schema.Dependency{Target: extension, Type: schema.DependencyOwns})
		}
		out = append(out, resource)
	}
	i.resources = out
	return nil
}

func (i *inspector) appendDependency(resourceID string, dependency schema.Dependency) {
	for index := range i.resources {
		if i.resources[index].ID != resourceID {
			continue
		}
		for _, existing := range i.resources[index].Dependencies {
			if existing.Target == dependency.Target && existing.Type == dependency.Type {
				return
			}
		}
		i.resources[index].Dependencies = append(i.resources[index].Dependencies, dependency)
		return
	}
}

func dependencyTargets(dependencies []schema.Dependency) []string {
	out := make([]string, 0, len(dependencies))
	for _, dependency := range dependencies {
		out = append(out, dependency.Target)
	}
	return out
}

func dep(target string, typ schema.DependencyType) []schema.Dependency {
	if target == "" {
		return nil
	}
	return []schema.Dependency{{Target: target, Type: typ}}
}

func (i *inspector) name(schemaName, name, parent string) schema.Name {
	return schema.Name{Schema: schemaName, Name: name, Parent: parent}
}

func (i *inspector) inspectSchemas(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select oid, nspname, pg_get_userbyid(nspowner), obj_description(oid, 'pg_namespace') from pg_namespace where nspname <> 'information_schema' and nspname !~ '^pg_' order by nspname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var name, owner string
		var comment *string
		if err := rows.Scan(&oid, &name, &owner, &comment); err != nil {
			return err
		}
		specification := map[string]any{}
		if i.advanced {
			specification["owner"] = owner
		}
		id := i.add(schema.KindSchema, i.name("", name, ""), specification, nil, comment)
		i.schemas[name] = id
		i.recordOID("pg_namespace", oid, id)
	}
	return rows.Err()
}

func (i *inspector) inspectExtensions(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select e.oid,e.extname,e.extversion,n.nspname,pg_get_userbyid(e.extowner),v.relocatable,v.trusted,v.superuser,coalesce(v.requires,'{}'::name[]),obj_description(e.oid,'pg_extension') from pg_extension e join pg_namespace n on n.oid=e.extnamespace join pg_available_extension_versions v on v.name=e.extname and v.version=e.extversion order by e.extname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var name, version, ns, owner string
		var relocatable, trusted, superuser bool
		var requires []string
		var comment *string
		if err := rows.Scan(&oid, &name, &version, &ns, &owner, &relocatable, &trusted, &superuser, &requires, &comment); err != nil {
			return err
		}
		parent := i.schemas[ns]
		if parent == "" {
			continue
		}
		if !i.advanced {
			owner = ""
		}
		id := i.add(schema.KindExtension, i.name(ns, name, parent), extensionRequirementSpec(ExtensionAvailability{Name: name, Version: version, Relocatable: relocatable, Trusted: trusted, Superuser: superuser, Requires: requires}, owner), dep(parent, schema.DependencyContains), comment)
		i.recordOID("pg_extension", oid, id)
	}
	return rows.Err()
}

func (i *inspector) inspectTypes(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `
select t.oid,n.nspname,t.typname,t.typtype::text,format_type(t.typbasetype,t.typtypmod),t.typnotnull,pg_get_userbyid(t.typowner),
       pg_get_expr(t.typdefaultbin,0),obj_description(t.oid,'pg_type')
from pg_type t join pg_namespace n on n.oid=t.typnamespace
where n.nspname <> 'information_schema' and n.nspname !~ '^pg_' and t.typtype in ('e','d','c')
and not exists (select 1 from pg_class c where c.reltype=t.oid and c.relkind in ('r','p','v','m','f'))
order by n.nspname,t.typname`)
	if err != nil {
		return err
	}
	type typeRow struct {
		oid            uint32
		ns, name, kind string
		base, owner    string
		notnull        bool
		def, comment   *string
	}
	var found []typeRow
	for rows.Next() {
		var r typeRow
		if err := rows.Scan(&r.oid, &r.ns, &r.name, &r.kind, &r.base, &r.notnull, &r.owner, &r.def, &r.comment); err != nil {
			rows.Close()
			return err
		}
		found = append(found, r)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return err
	}
	rows.Close()
	for _, r := range found {
		parent := i.schemas[r.ns]
		if parent == "" {
			continue
		}
		var k schema.Kind
		spec := map[string]any{}
		if i.advanced {
			spec["owner"] = r.owner
		}
		switch r.kind {
		case "e":
			k = schema.KindEnum
			labels, err := i.enumLabels(ctx, r.oid)
			if err != nil {
				return err
			}
			spec["values"] = labels
		case "d":
			k = schema.KindDomain
			spec["base_type"] = r.base
			spec["not_null"] = r.notnull
			if r.def != nil {
				spec["default"] = *r.def
			}
			constraints, err := i.domainConstraints(ctx, r.oid)
			if err != nil {
				return err
			}
			if len(constraints) > 0 {
				spec["constraints"] = constraints
			}
		case "c":
			k = schema.KindComposite
			attrs, err := i.compositeAttrs(ctx, r.oid)
			if err != nil {
				return err
			}
			spec["attributes"] = attrs
		}
		id := i.add(k, i.name(r.ns, r.name, parent), spec, dep(parent, schema.DependencyContains), r.comment)
		i.recordOID("pg_type", r.oid, id)
	}
	return nil
}

func (i *inspector) domainConstraints(ctx context.Context, oid uint32) ([]string, error) {
	rows, err := i.conn.Query(ctx, `select pg_get_constraintdef(oid,true) from pg_constraint where contypid=$1 order by conname`, oid)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var definition string
		if err := rows.Scan(&definition); err != nil {
			return nil, err
		}
		out = append(out, definition)
	}
	return out, rows.Err()
}

func (i *inspector) enumLabels(ctx context.Context, oid uint32) ([]string, error) {
	rows, e := i.conn.Query(ctx, `select enumlabel from pg_enum where enumtypid=$1 order by enumsortorder`, oid)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if e = rows.Scan(&s); e != nil {
			return nil, e
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
func (i *inspector) compositeAttrs(ctx context.Context, oid uint32) ([]map[string]any, error) {
	rows, e := i.conn.Query(ctx, `select a.attname,format_type(a.atttypid,a.atttypmod),a.attnum,case when a.attcollation<>0 and a.attcollation<>at.typcollation then quote_ident(cn.nspname)||'.'||quote_ident(co.collname) else '' end from pg_attribute a join pg_type t on t.typrelid=a.attrelid join pg_type at on at.oid=a.atttypid left join pg_collation co on co.oid=a.attcollation left join pg_namespace cn on cn.oid=co.collnamespace where t.oid=$1 and a.attnum>0 and not a.attisdropped order by a.attnum`, oid)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var n, t, collation string
		var ordinal int16
		if e = rows.Scan(&n, &t, &ordinal, &collation); e != nil {
			return nil, e
		}
		_ = ordinal // physical attnum may contain gaps after DROP ATTRIBUTE
		attribute := map[string]any{"name": n, "type": t, "ordinal": len(out) + 1}
		if collation != "" {
			attribute["collation"] = collation
		}
		out = append(out, attribute)
	}
	return out, rows.Err()
}

func (i *inspector) inspectTypeDependencies(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select parent.oid,case when attribute_type.typelem<>0 then attribute_type.typelem else a.atttypid end,coalesce((select d.refobjid from pg_depend d where d.classid='pg_type'::regclass and d.objid=case when attribute_type.typelem<>0 then attribute_type.typelem else a.atttypid end and d.refclassid='pg_extension'::regclass and d.deptype='e' limit 1),0) from pg_type parent join pg_attribute a on a.attrelid=parent.typrelid join pg_type attribute_type on attribute_type.oid=a.atttypid where parent.typtype='c' and a.attnum>0 and not a.attisdropped order by parent.oid,a.attnum`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var parentOID, targetOID, extensionOID uint32
		if err := rows.Scan(&parentOID, &targetOID, &extensionOID); err != nil {
			return err
		}
		parent := i.byCatalog[catalogOID{catalog: "pg_type", oid: parentOID}]
		target := i.byCatalog[catalogOID{catalog: "pg_type", oid: targetOID}]
		if parent != "" && target != "" && parent != target {
			i.appendDependency(parent, schema.Dependency{Target: target, Type: schema.DependencyUses})
		} else if parent != "" && extensionOID != 0 {
			if extension := i.byCatalog[catalogOID{catalog: "pg_extension", oid: extensionOID}]; extension != "" {
				i.appendDependency(parent, schema.Dependency{Target: extension, Type: schema.DependencyUses})
			}
		}
	}
	return rows.Err()
}

func (i *inspector) inspectSequences(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select c.oid,n.nspname,c.relname,s.seqstart,s.seqincrement,s.seqmin,s.seqmax,s.seqcache,s.seqcycle,pg_get_userbyid(c.relowner),obj_description(c.oid,'pg_class') from pg_class c join pg_namespace n on n.oid=c.relnamespace join pg_sequence s on s.seqrelid=c.oid where c.relkind='S' and n.nspname <> 'information_schema' and n.nspname !~ '^pg_' and not exists (select 1 from pg_depend d where d.classid='pg_class'::regclass and d.objid=c.oid and d.refclassid='pg_class'::regclass and d.deptype='i' and d.refobjsubid>0) order by n.nspname,c.relname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var ns, name, owner string
		var start, inc, min, max, cache int64
		var cycle bool
		var comment *string
		if err := rows.Scan(&oid, &ns, &name, &start, &inc, &min, &max, &cache, &cycle, &owner, &comment); err != nil {
			return err
		}
		p := i.schemas[ns]
		if p == "" {
			continue
		}
		specification := map[string]any{"start": start, "increment": inc, "min": min, "max": max, "cache": cache, "cycle": cycle}
		if i.advanced {
			specification["owner"] = owner
		}
		id := i.add(schema.KindSequence, i.name(ns, name, p), specification, dep(p, schema.DependencyContains), comment)
		i.recordOID("pg_class", oid, id)
	}
	return rows.Err()
}

func (i *inspector) inspectRelations(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select c.oid,n.nspname,c.relname,c.relkind::text,c.relpersistence::text,c.relrowsecurity,c.relforcerowsecurity,pg_get_viewdef(c.oid,true),pg_get_userbyid(c.relowner),obj_description(c.oid,'pg_class') from pg_class c join pg_namespace n on n.oid=c.relnamespace where c.relkind in ('r','p','v','m') and n.nspname <> 'information_schema' and n.nspname !~ '^pg_' order by n.nspname,c.relname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var ns, name, relkind, persistence, owner string
		var rls, force bool
		var definition, comment *string
		if err := rows.Scan(&oid, &ns, &name, &relkind, &persistence, &rls, &force, &definition, &owner, &comment); err != nil {
			return err
		}
		p := i.schemas[ns]
		if p == "" {
			continue
		}
		kind := schema.KindTable
		spec := map[string]any{}
		if i.advanced {
			spec["owner"] = owner
		}
		if relkind == "v" {
			kind = schema.KindView
			spec["definition"] = value(definition)
		} else if relkind == "m" {
			kind = schema.KindMaterializedView
			spec["definition"] = value(definition)
		} else {
			spec["partitioned"] = relkind == "p"
			spec["persistence"] = persistence
			spec["row_security"] = rls
			spec["force_row_security"] = force
		}
		id := i.add(kind, i.name(ns, name, p), spec, dep(p, schema.DependencyContains), comment)
		i.recordOID("pg_class", oid, id)
	}
	return rows.Err()
}

func (i *inspector) inspectPartitions(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select child.oid,parent.oid,pg_get_expr(child.relpartbound,child.oid,true),pg_get_partkeydef(parent.oid) from pg_inherits inh join pg_class child on child.oid=inh.inhrelid join pg_class parent on parent.oid=inh.inhparent join pg_namespace n on n.oid=child.relnamespace where child.relispartition and n.nspname <> 'information_schema' and n.nspname !~ '^pg_' order by child.oid`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var childOID, parentOID uint32
		var bound, key string
		if err := rows.Scan(&childOID, &parentOID, &bound, &key); err != nil {
			return err
		}
		childID, parentID := i.byOID[childOID], i.byOID[parentOID]
		if childID == "" || parentID == "" {
			continue
		}
		strategy, columns, parseErr := parseInspectedPartitionKey(key)
		if parseErr != nil {
			return parseErr
		}
		for index := range i.resources {
			resource := &i.resources[index]
			if resource.ID == parentID {
				values := specMap(resource.Spec)
				values["partitioned"], values["partition_strategy"], values["partition_columns"] = true, strategy, columns
				resource.Spec, _ = json.Marshal(values)
			}
			if resource.ID == childID {
				values := specMap(resource.Spec)
				values["partition_of"], values["partition_bound"] = parentID, bound
				resource.Spec, _ = json.Marshal(values)
				appendUniqueDependency(&resource.Dependencies, schema.Dependency{Target: parentID, Type: schema.DependencyReferences})
			}
		}
	}
	return rows.Err()
}

func parseInspectedPartitionKey(value string) (string, []string, error) {
	match := regexp.MustCompile(`(?i)^\s*(RANGE|LIST|HASH)\s*\((.*)\)\s*$`).FindStringSubmatch(value)
	if match == nil {
		return "", nil, fmt.Errorf("unsupported inspected partition key %q", value)
	}
	parts := strings.Split(match[2], ",")
	columns := make([]string, 0, len(parts))
	for _, part := range parts {
		column := strings.Trim(strings.TrimSpace(part), `"`)
		if column == "" || strings.ContainsAny(column, " ()") {
			return "", nil, fmt.Errorf("partition expression %q is outside the managed column-key grammar", part)
		}
		columns = append(columns, column)
	}
	return strings.ToLower(match[1]), columns, nil
}

func (i *inspector) inspectColumns(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select a.attrelid,a.attnum,n.nspname,c.relname,a.attname,format_type(a.atttypid,a.atttypmod),a.attnotnull,pg_get_expr(ad.adbin,ad.adrelid),a.attidentity::text,a.attgenerated::text,col_description(a.attrelid,a.attnum),case when t.typelem<>0 then t.typelem else a.atttypid end from pg_attribute a join pg_class c on c.oid=a.attrelid join pg_namespace n on n.oid=c.relnamespace join pg_type t on t.oid=a.atttypid left join pg_attrdef ad on ad.adrelid=a.attrelid and ad.adnum=a.attnum where a.attnum>0 and not a.attisdropped and c.relkind in ('r','p','v','m') and n.nspname <> 'information_schema' and n.nspname !~ '^pg_' order by n.nspname,c.relname,a.attnum`)
	if err != nil {
		return err
	}
	defer rows.Close()
	ordinals := map[uint32]int{}
	for rows.Next() {
		var rel, typeoid uint32
		var pos int16
		var ns, table, name, typ, identity, generated string
		var nn bool
		var def, comment *string
		if err := rows.Scan(&rel, &pos, &ns, &table, &name, &typ, &nn, &def, &identity, &generated, &comment, &typeoid); err != nil {
			return err
		}
		p := i.byOID[rel]
		if p == "" {
			continue
		}
		ordinals[rel]++
		spec := map[string]any{"ordinal": ordinals[rel], "type": typ, "not_null": nn}
		if def != nil {
			spec["default"] = *def
		}
		if identity != "" {
			spec["identity"] = identity
		}
		if generated != "" {
			spec["generated"] = generated
		}
		deps := dep(p, schema.DependencyContains)
		if tid := i.byOID[typeoid]; tid != "" {
			deps = append(deps, schema.Dependency{Target: tid, Type: schema.DependencyUses})
		}
		id := i.add(schema.KindColumn, i.name(ns, name, p), spec, deps, comment)
		i.columns[columnCatalogKey{relation: rel, position: pos}] = id
	}
	return rows.Err()
}

func (i *inspector) inspectColumnDefaultDependencies(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `
select distinct ad.adrelid,ad.adnum,n.nspname,c.relname,a.attname,d.refobjid
from pg_attrdef ad
join pg_attribute a on a.attrelid=ad.adrelid and a.attnum=ad.adnum
join pg_class c on c.oid=ad.adrelid
join pg_namespace n on n.oid=c.relnamespace
join pg_depend d on d.classid='pg_attrdef'::regclass and d.objid=ad.oid and d.refclassid='pg_class'::regclass
join pg_class sequence on sequence.oid=d.refobjid and sequence.relkind='S'
where n.nspname <> 'information_schema' and n.nspname !~ '^pg_'
order by ad.adrelid,ad.adnum,d.refobjid`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var relation, sequenceOID uint32
		var position int16
		var namespace, table, column string
		if err := rows.Scan(&relation, &position, &namespace, &table, &column, &sequenceOID); err != nil {
			return err
		}
		parent, target := i.byOID[relation], i.byOID[sequenceOID]
		if parent == "" || target == "" {
			continue
		}
		columnID := schema.StableID(schema.KindColumn, i.name(namespace, column, parent))
		for index := range i.resources {
			if i.resources[index].ID == columnID {
				i.resources[index].Dependencies = append(i.resources[index].Dependencies, schema.Dependency{Target: target, Type: schema.DependencyReferences})
				break
			}
		}
	}
	return rows.Err()
}

func (i *inspector) inspectGeneratedColumnDependencies(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `
select ad.adrelid,ad.adnum,'column',d.refobjid,d.refobjsubid
from pg_attrdef ad
join pg_attribute a on a.attrelid=ad.adrelid and a.attnum=ad.adnum
join pg_depend d on d.classid='pg_attrdef'::regclass and d.objid=ad.oid
where a.attgenerated='s' and d.refclassid='pg_class'::regclass and d.refobjsubid>0
  and not (d.refobjid=ad.adrelid and d.refobjsubid=ad.adnum)
union all
select ad.adrelid,ad.adnum,'routine',d.refobjid,0
from pg_attrdef ad
join pg_attribute a on a.attrelid=ad.adrelid and a.attnum=ad.adnum
join pg_depend d on d.classid='pg_attrdef'::regclass and d.objid=ad.oid
join pg_proc p on p.oid=d.refobjid
join pg_namespace n on n.oid=p.pronamespace
where a.attgenerated='s' and d.refclassid='pg_proc'::regclass
  and n.nspname <> 'information_schema' and n.nspname !~ '^pg_'
order by 1,2,3,4,5`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var relation, targetOID uint32
		var position, targetPosition int16
		var targetKind string
		if err := rows.Scan(&relation, &position, &targetKind, &targetOID, &targetPosition); err != nil {
			return err
		}
		from := i.columns[columnCatalogKey{relation: relation, position: position}]
		to := ""
		switch targetKind {
		case "column":
			to = i.columns[columnCatalogKey{relation: targetOID, position: targetPosition}]
		case "routine":
			to = i.byOID[targetOID]
		}
		if from == "" || to == "" {
			return &catalogDisappearanceError{resource: "generated column dependency", oid: targetOID}
		}
		for index := range i.resources {
			if i.resources[index].ID != from {
				continue
			}
			exists := false
			for _, dependency := range i.resources[index].Dependencies {
				exists = exists || dependency.Target == to && dependency.Type == schema.DependencyReferences
			}
			if !exists {
				i.resources[index].Dependencies = append(i.resources[index].Dependencies, schema.Dependency{Target: to, Type: schema.DependencyReferences})
			}
			break
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	resources := resourceMapForRender(schema.Document{Graph: schema.Graph{Resources: i.resources}})
	for index := range i.resources {
		resource := &i.resources[index]
		if resource.Kind != schema.KindColumn || stringValue(spec(*resource), "generated") != "s" {
			continue
		}
		expected, err := expectedGeneratedDependencies(*resource, resources)
		if err != nil {
			return err
		}
		for target := range expected {
			exists := false
			for _, dependency := range resource.Dependencies {
				exists = exists || dependency.Target == target && dependency.Type == schema.DependencyReferences
			}
			if !exists {
				resource.Dependencies = append(resource.Dependencies, schema.Dependency{Target: target, Type: schema.DependencyReferences})
			}
		}
	}
	return nil
}

func (i *inspector) inspectConstraints(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select x.oid,x.conrelid,x.confrelid,x.conindid,n.nspname,c.relname,x.conname,x.contype::text,pg_get_constraintdef(x.oid,true),x.condeferrable,x.condeferred,x.convalidated,obj_description(x.oid,'pg_constraint'),coalesce((select array_agg(a.attname order by k.ordinality) from unnest(x.conkey) with ordinality k(attnum,ordinality) join pg_attribute a on a.attrelid=x.conrelid and a.attnum=k.attnum),'{}'::text[]),coalesce((select array_agg(a.attname order by k.ordinality) from unnest(x.confkey) with ordinality k(attnum,ordinality) join pg_attribute a on a.attrelid=x.confrelid and a.attnum=k.attnum),'{}'::text[]) from pg_constraint x join pg_class c on c.oid=x.conrelid join pg_namespace n on n.oid=c.relnamespace where x.contype in ('p','u','c','f') and n.nspname <> 'information_schema' and n.nspname !~ '^pg_' order by n.nspname,c.relname,x.conname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	backingConstraints := map[uint32]string{}
	foreignBackings := map[string]uint32{}
	for rows.Next() {
		var oid, rel, foreign, backing uint32
		var ns, table, name, typ, definition string
		var deferrable, deferred, validated bool
		var comment *string
		var columns, referencedColumns []string
		if err := rows.Scan(&oid, &rel, &foreign, &backing, &ns, &table, &name, &typ, &definition, &deferrable, &deferred, &validated, &comment, &columns, &referencedColumns); err != nil {
			return err
		}
		p := i.byOID[rel]
		if p == "" {
			continue
		}
		kind := schema.KindCheckConstraint
		switch typ {
		case "p":
			kind = schema.KindPrimaryKey
		case "u":
			kind = schema.KindUniqueConstraint
		case "f":
			kind = schema.KindForeignKey
		}
		deps := dep(p, schema.DependencyContains)
		for _, column := range columns {
			if target := i.columnID(rel, column); target != "" {
				deps = append(deps, schema.Dependency{Target: target, Type: schema.DependencyReferences})
			}
		}
		if target := i.byOID[foreign]; kind == schema.KindForeignKey && target != "" {
			deps = append(deps, schema.Dependency{Target: target, Type: schema.DependencyReferences})
			for _, column := range referencedColumns {
				if target := i.columnID(foreign, column); target != "" {
					deps = append(deps, schema.Dependency{Target: target, Type: schema.DependencyReferences})
				}
			}
		}
		specification := map[string]any{"definition": definition, "deferrable": deferrable, "initially_deferred": deferred, "validated": validated, "columns": columns}
		if kind == schema.KindForeignKey {
			specification["referenced_columns"] = referencedColumns
		}
		id := i.add(kind, i.name(ns, name, p), specification, deps, comment)
		i.recordOID("pg_constraint", oid, id)
		if kind == schema.KindPrimaryKey || kind == schema.KindUniqueConstraint {
			backingConstraints[backing] = id
		} else if kind == schema.KindForeignKey {
			foreignBackings[id] = backing
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for id, backing := range foreignBackings {
		target := backingConstraints[backing]
		if target == "" {
			continue
		}
		for index := range i.resources {
			if i.resources[index].ID == id {
				i.resources[index].Dependencies = append(i.resources[index].Dependencies, schema.Dependency{Target: target, Type: schema.DependencyReferences})
				break
			}
		}
	}
	return nil
}

func (i *inspector) inspectIndexes(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select x.indexrelid,x.indrelid,n.nspname,t.relname,c.relname,am.amname,x.indisunique,x.indisvalid,x.indisready,pg_get_indexdef(x.indexrelid),obj_description(x.indexrelid,'pg_class'),coalesce((select array_agg(a.attname order by k.ordinality) from unnest(x.indkey::smallint[]) with ordinality k(attnum,ordinality) join pg_attribute a on a.attrelid=x.indrelid and a.attnum=k.attnum where k.attnum > 0),'{}'::text[]) from pg_index x join pg_class c on c.oid=x.indexrelid join pg_class t on t.oid=x.indrelid join pg_namespace n on n.oid=t.relnamespace join pg_am am on am.oid=c.relam left join pg_constraint con on con.conindid=x.indexrelid where con.oid is null and n.nspname <> 'information_schema' and n.nspname !~ '^pg_' order by n.nspname,t.relname,c.relname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid, rel uint32
		var ns, table, name, method string
		var definition *string
		var unique, valid, ready bool
		var comment *string
		var columns []string
		if err := rows.Scan(&oid, &rel, &ns, &table, &name, &method, &unique, &valid, &ready, &definition, &comment, &columns); err != nil {
			return err
		}
		if definition == nil {
			return &catalogDisappearanceError{resource: "index definition", oid: oid}
		}
		p := i.byOID[rel]
		if p == "" {
			continue
		}
		deps := dep(p, schema.DependencyContains)
		for _, column := range columns {
			if target := i.columnID(rel, column); target != "" {
				deps = append(deps, schema.Dependency{Target: target, Type: schema.DependencyReferences})
			}
		}
		id := i.add(schema.KindIndex, i.name(ns, name, p), map[string]any{"method": method, "unique": unique, "valid": valid, "ready": ready, "definition": *definition, "columns": columns}, deps, comment)
		i.recordOID("pg_class", oid, id)
	}
	return rows.Err()
}

func (i *inspector) columnID(relation uint32, name string) string {
	for key, id := range i.columns {
		if key.relation != relation {
			continue
		}
		for _, resource := range i.resources {
			if resource.ID == id && resource.Name.Name == name {
				return id
			}
		}
	}
	return ""
}

func (i *inspector) inspectRoutines(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select p.oid,n.nspname,p.proname,p.prokind::text,pg_get_function_identity_arguments(p.oid),pg_get_function_arguments(p.oid),coalesce(pg_get_function_result(p.oid),''),p.proretset,l.lanname,p.provolatile::text,p.proisstrict,p.prosecdef,p.proleakproof,p.proparallel::text,p.procost::float8,p.prorows::float8,coalesce(p.proconfig,'{}'::text[]),pg_get_userbyid(p.proowner),pg_get_functiondef(p.oid),obj_description(p.oid,'pg_proc'),coalesce((select e.extname from pg_depend d join pg_extension e on e.oid=d.refobjid where d.classid='pg_proc'::regclass and d.objid=p.oid and d.refclassid='pg_extension'::regclass and d.deptype='e' limit 1),'') from pg_proc p join pg_namespace n on n.oid=p.pronamespace join pg_language l on l.oid=p.prolang where p.prokind in ('f','p') and n.nspname <> 'information_schema' and n.nspname !~ '^pg_' order by n.nspname,p.proname,pg_get_function_identity_arguments(p.oid)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var ns, name, kind, identityArguments, arguments, result, language, volatility, parallel, owner, definition, extension string
		var returnsSet, strict, security, leakproof bool
		var cost, rowsEstimate float64
		var configuration []string
		var comment *string
		if err := rows.Scan(&oid, &ns, &name, &kind, &identityArguments, &arguments, &result, &returnsSet, &language, &volatility, &strict, &security, &leakproof, &parallel, &cost, &rowsEstimate, &configuration, &owner, &definition, &comment, &extension); err != nil {
			return err
		}
		p := i.schemas[ns]
		if p == "" {
			continue
		}
		k := schema.KindFunction
		if kind == "p" {
			k = schema.KindProcedure
		}
		logical := name + "(" + identityArguments + ")"
		specification := map[string]any{"name": name, "identity_arguments": identityArguments, "arguments": arguments, "result": result, "returns_set": returnsSet, "language": language, "volatility": volatility, "strict": strict, "security_definer": security, "leakproof": leakproof, "parallel": parallel, "cost": cost, "rows": rowsEstimate, "configuration": configuration, "owner": owner, "definition": definition, "body_digest": routineDefinitionDigest(definition)}
		if extension != "" {
			specification["extension"] = extension
		}
		id := i.add(k, i.name(ns, logical, p), specification, dep(p, schema.DependencyContains), comment)
		i.recordOID("pg_proc", oid, id)
	}
	return rows.Err()
}

func (i *inspector) inspectRoutineDependencies(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select distinct d.objid::oid,d.refclassid::regclass::text,d.refobjid::oid from pg_depend d join pg_proc p on p.oid=d.objid join pg_namespace n on n.oid=p.pronamespace where d.classid='pg_proc'::regclass and d.refclassid in ('pg_type'::regclass,'pg_class'::regclass,'pg_proc'::regclass) and d.deptype in ('n','a') and n.nspname <> 'information_schema' and n.nspname !~ '^pg_' order by 1,2,3`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var fromOID, toOID uint32
		var catalog string
		if err := rows.Scan(&fromOID, &catalog, &toOID); err != nil {
			return err
		}
		from, to := i.byOID[fromOID], i.byOID[toOID]
		if from == "" || to == "" || from == to {
			continue
		}
		dependencyType := schema.DependencyReferences
		if catalog == "pg_type" {
			dependencyType = schema.DependencyUses
		}
		for index := range i.resources {
			if i.resources[index].ID != from {
				continue
			}
			exists := false
			for _, dependency := range i.resources[index].Dependencies {
				exists = exists || dependency.Target == to && dependency.Type == dependencyType
			}
			if !exists {
				i.resources[index].Dependencies = append(i.resources[index].Dependencies, schema.Dependency{Target: to, Type: dependencyType})
			}
		}
	}
	return rows.Err()
}

func (i *inspector) inspectExpressionDependencies(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select distinct d.classid::regclass::text,d.objid::oid,d.refclassid::regclass::text,d.refobjid::oid from pg_depend d where d.classid in ('pg_constraint'::regclass,'pg_class'::regclass) and (d.refclassid='pg_proc'::regclass or d.classid='pg_constraint'::regclass and d.refclassid='pg_type'::regclass) and d.deptype in ('n','a') order by 1,2,3,4`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var catalog, targetCatalog string
		var fromOID, toOID uint32
		if err := rows.Scan(&catalog, &fromOID, &targetCatalog, &toOID); err != nil {
			return err
		}
		from := i.byCatalog[catalogOID{catalog: catalog, oid: fromOID}]
		to := i.byCatalog[catalogOID{catalog: targetCatalog, oid: toOID}]
		if from == "" || to == "" {
			continue
		}
		for index := range i.resources {
			resource := &i.resources[index]
			if resource.ID != from || catalog == "pg_class" && resource.Kind != schema.KindIndex {
				continue
			}
			dependencyType := schema.DependencyReferences
			if targetCatalog == "pg_type" {
				dependencyType = schema.DependencyUses
			}
			exists := false
			for _, dependency := range resource.Dependencies {
				exists = exists || dependency.Target == to && dependency.Type == dependencyType
			}
			if !exists {
				resource.Dependencies = append(resource.Dependencies, schema.Dependency{Target: to, Type: dependencyType})
			}
		}
	}
	return rows.Err()
}

func (i *inspector) inspectColumnRoutineDependencies(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select distinct ad.adrelid,ad.adnum,d.refobjid::oid from pg_attrdef ad join pg_attribute a on a.attrelid=ad.adrelid and a.attnum=ad.adnum join pg_depend d on d.classid='pg_attrdef'::regclass and d.objid=ad.oid join pg_proc p on p.oid=d.refobjid join pg_namespace n on n.oid=p.pronamespace where d.refclassid='pg_proc'::regclass and n.nspname <> 'information_schema' and n.nspname !~ '^pg_' order by 1,2,3`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var relation, routine uint32
		var position int16
		if err := rows.Scan(&relation, &position, &routine); err != nil {
			return err
		}
		from, to := i.columns[columnCatalogKey{relation: relation, position: position}], i.byOID[routine]
		if from == "" || to == "" {
			continue
		}
		for index := range i.resources {
			if i.resources[index].ID != from {
				continue
			}
			exists := false
			for _, dependency := range i.resources[index].Dependencies {
				exists = exists || dependency.Target == to && dependency.Type == schema.DependencyReferences
			}
			if !exists {
				i.resources[index].Dependencies = append(i.resources[index].Dependencies, schema.Dependency{Target: to, Type: schema.DependencyReferences})
			}
		}
	}
	return rows.Err()
}

func (i *inspector) inspectTriggers(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select g.oid,g.tgrelid,g.tgfoid,n.nspname,c.relname,g.tgname,pg_get_triggerdef(g.oid,true),g.tgenabled::text,obj_description(g.oid,'pg_trigger'),coalesce((select array_agg(distinct a.attname order by a.attname) from pg_depend d join pg_attribute a on a.attrelid=d.refobjid and a.attnum=d.refobjsubid where d.classid='pg_trigger'::regclass and d.objid=g.oid and d.refclassid='pg_class'::regclass and d.refobjid=g.tgrelid and d.refobjsubid > 0),'{}'::text[]) from pg_trigger g join pg_class c on c.oid=g.tgrelid join pg_namespace n on n.oid=c.relnamespace where not g.tgisinternal and n.nspname <> 'information_schema' and n.nspname !~ '^pg_' order by n.nspname,c.relname,g.tgname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid, rel, function uint32
		var ns, table, name, definition, enabled string
		var comment *string
		var columns []string
		if err := rows.Scan(&oid, &rel, &function, &ns, &table, &name, &definition, &enabled, &comment, &columns); err != nil {
			return err
		}
		p := i.byOID[rel]
		if p == "" {
			continue
		}
		deps := dep(p, schema.DependencyContains)
		if target := i.byOID[function]; target != "" {
			deps = append(deps, schema.Dependency{Target: target, Type: schema.DependencyReferences})
		}
		for _, column := range columns {
			if target := i.columnID(rel, column); target != "" {
				deps = append(deps, schema.Dependency{Target: target, Type: schema.DependencyReferences})
			}
		}
		id := i.add(schema.KindTrigger, i.name(ns, name, p), map[string]any{"definition": definition, "enabled": enabled, "columns": columns}, deps, comment)
		i.recordOID("pg_trigger", oid, id)
	}
	return rows.Err()
}

func (i *inspector) inspectPolicies(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select p.oid,p.polrelid,n.nspname,c.relname,p.polname,p.polcmd::text,p.polpermissive,coalesce(array(select case when role_oid=0 then 'public' else pg_get_userbyid(role_oid) end from unnest(p.polroles) role_oid order by 1),array[]::text[]),pg_get_expr(p.polqual,p.polrelid),pg_get_expr(p.polwithcheck,p.polrelid),obj_description(p.oid,'pg_policy') from pg_policy p join pg_class c on c.oid=p.polrelid join pg_namespace n on n.oid=c.relnamespace where n.nspname <> 'information_schema' and n.nspname !~ '^pg_' order by n.nspname,c.relname,p.polname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid, rel uint32
		var ns, table, name, command string
		var permissive bool
		var roles []string
		var using, check, comment *string
		if err := rows.Scan(&oid, &rel, &ns, &table, &name, &command, &permissive, &roles, &using, &check, &comment); err != nil {
			return err
		}
		p := i.byOID[rel]
		if p == "" {
			continue
		}
		spec := map[string]any{"command": command, "permissive": permissive, "roles": roles}
		if using != nil {
			spec["using"] = *using
		}
		if check != nil {
			spec["check"] = *check
		}
		rawSpec, _ := json.Marshal(spec)
		policy := schema.Resource{Kind: schema.KindPolicy, Name: i.name(ns, name, p), Spec: rawSpec, Dependencies: dep(p, schema.DependencyContains)}
		policy.ID = schema.StableID(policy.Kind, policy.Name)
		if parsed, parseErr := parsePolicy(policy, resourceMapFromSlice(i.resources, policy)); parseErr == nil {
			policy.Dependencies = make([]schema.Dependency, 0, len(parsed.Dependencies))
			for _, target := range parsed.Dependencies {
				dependencyType := schema.DependencyReferences
				if target == p {
					dependencyType = schema.DependencyContains
				}
				policy.Dependencies = append(policy.Dependencies, schema.Dependency{Target: target, Type: dependencyType})
			}
		}
		id := i.add(schema.KindPolicy, policy.Name, spec, policy.Dependencies, comment)
		i.recordOID("pg_policy", oid, id)
	}
	return rows.Err()
}

func resourceMapFromSlice(resources []schema.Resource, additional ...schema.Resource) map[string]schema.Resource {
	out := make(map[string]schema.Resource, len(resources)+len(additional))
	for _, resource := range resources {
		out[resource.ID] = resource
	}
	for _, resource := range additional {
		out[resource.ID] = resource
	}
	return out
}

func (i *inspector) attachOwnerDependencies() {
	roles := map[string]string{}
	for _, resource := range i.resources {
		if resource.Kind == schema.KindRole {
			roles[resource.Name.Name] = resource.ID
		}
	}
	for index := range i.resources {
		owner := stringValue(spec(i.resources[index]), "owner")
		if owner == "" || roles[owner] == "" {
			continue
		}
		exists := false
		for _, dependency := range i.resources[index].Dependencies {
			exists = exists || dependency.Target == roles[owner] && dependency.Type == schema.DependencyOwns
		}
		if !exists {
			i.resources[index].Dependencies = append(i.resources[index].Dependencies, schema.Dependency{Target: roles[owner], Type: schema.DependencyOwns})
		}
	}
}

func (i *inspector) inspectRoles(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select oid,rolname,rolsuper,rolinherit,rolcreaterole,rolcreatedb,rolcanlogin,rolreplication,rolbypassrls,rolconnlimit,rolvaliduntil::text,coalesce(rolconfig,'{}'::text[]),obj_description(oid,'pg_authid') from pg_roles order by rolname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var name string
		var super, inherit, createRole, createDB, login, repl, bypass bool
		var limit int32
		var configuration []string
		var until, comment *string
		if err := rows.Scan(&oid, &name, &super, &inherit, &createRole, &createDB, &login, &repl, &bypass, &limit, &until, &configuration, &comment); err != nil {
			return err
		}
		spec := map[string]any{"superuser": super, "inherit": inherit, "create_role": createRole, "create_database": createDB, "login": login, "replication": repl, "bypass_rls": bypass, "connection_limit": limit, "configuration": configuration}
		if until != nil {
			spec["valid_until"] = *until
		}
		id := i.add(schema.KindRole, schema.Name{Name: name}, spec, nil, comment)
		i.recordOID("pg_authid", oid, id)
	}
	return rows.Err()
}

func (i *inspector) inspectGrants(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `
with grants as (
  select n.oid as target_oid,a.grantor,a.grantee,a.privilege_type,a.is_grantable
  from pg_namespace n
  cross join lateral aclexplode(coalesce(n.nspacl,acldefault('n',n.nspowner))) a
  where n.nspname <> 'information_schema' and n.nspname !~ '^pg_'
  union all
  select c.oid,a.grantor,a.grantee,a.privilege_type,a.is_grantable
  from pg_class c join pg_namespace n on n.oid=c.relnamespace
  cross join lateral aclexplode(coalesce(c.relacl,acldefault('r',c.relowner))) a
  where c.relkind in ('r','p','v','m') and n.nspname <> 'information_schema' and n.nspname !~ '^pg_'
  union all
  select c.oid,a.grantor,a.grantee,a.privilege_type,a.is_grantable
  from pg_class c join pg_namespace n on n.oid=c.relnamespace
  cross join lateral aclexplode(coalesce(c.relacl,acldefault('S',c.relowner))) a
  where c.relkind='S' and n.nspname <> 'information_schema' and n.nspname !~ '^pg_'
  union all
  select p.oid,a.grantor,a.grantee,a.privilege_type,a.is_grantable
  from pg_proc p join pg_namespace n on n.oid=p.pronamespace
  cross join lateral aclexplode(coalesce(p.proacl,acldefault('f',p.proowner))) a
  where p.prokind in ('f','p') and n.nspname <> 'information_schema' and n.nspname !~ '^pg_'
)
select target_oid,grantor_role.rolname,coalesce(grantee_role.rolname,'PUBLIC'),privilege_type,is_grantable
from grants g
join pg_roles grantor_role on grantor_role.oid=g.grantor
left join pg_roles grantee_role on grantee_role.oid=nullif(g.grantee,0)
order by target_oid,grantor_role.rolname,coalesce(grantee_role.rolname,'PUBLIC'),privilege_type`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var targetOID uint32
		var grantor, grantee, privilege string
		var grantable bool
		if err := rows.Scan(&targetOID, &grantor, &grantee, &privilege, &grantable); err != nil {
			return err
		}
		p := i.byOID[targetOID]
		if p == "" {
			continue
		}
		parent := resourceByID(i.resources, p)
		name := grantee + ":" + strings.ToLower(privilege) + ":" + grantor
		deps := dep(p, schema.DependencyReferences)
		if rid := findKindName(i.resources, schema.KindRole, grantee); rid != "" {
			deps = append(deps, schema.Dependency{Target: rid, Type: schema.DependencyReferences})
		}
		if rid := findKindName(i.resources, schema.KindRole, grantor); rid != "" && rid != findKindName(i.resources, schema.KindRole, grantee) {
			deps = append(deps, schema.Dependency{Target: rid, Type: schema.DependencyReferences})
		}
		i.add(schema.KindGrant, i.name(parent.Name.Schema, name, p), map[string]any{"grantor": grantor, "grantee": grantee, "privilege": privilege, "grantable": grantable}, deps, nil)
	}
	return rows.Err()
}

func (i *inspector) inspectMemberships(ctx context.Context) error {
	var version int
	versionRows, err := i.conn.Query(ctx, `select current_setting('server_version_num')::integer`)
	if err != nil {
		return err
	}
	if !versionRows.Next() {
		versionRows.Close()
		return errors.New("PostgreSQL server version is unavailable")
	}
	if err = versionRows.Scan(&version); err != nil {
		versionRows.Close()
		return err
	}
	versionRows.Close()
	query := `select m.roleid,m.member,m.grantor,coalesce(m.admin_option,false),parent.rolname,member.rolname,grantor.rolname from pg_auth_members m join pg_roles parent on parent.oid=m.roleid join pg_roles member on member.oid=m.member join pg_roles grantor on grantor.oid=m.grantor order by parent.rolname,member.rolname,grantor.rolname`
	if version >= 160000 {
		query = `select m.roleid,m.member,m.grantor,coalesce(m.admin_option,false),m.inherit_option,m.set_option,parent.rolname,member.rolname,grantor.rolname from pg_auth_members m join pg_roles parent on parent.oid=m.roleid join pg_roles member on member.oid=m.member join pg_roles grantor on grantor.oid=m.grantor order by parent.rolname,member.rolname,grantor.rolname`
	}
	rows, err := i.conn.Query(ctx, query)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var parentOID, memberOID, grantorOID uint32
		var admin, inherit, set bool
		var parent, member, grantor string
		var scanErr error
		if version >= 160000 {
			scanErr = rows.Scan(&parentOID, &memberOID, &grantorOID, &admin, &inherit, &set, &parent, &member, &grantor)
		} else {
			scanErr = rows.Scan(&parentOID, &memberOID, &grantorOID, &admin, &parent, &member, &grantor)
		}
		if scanErr != nil {
			return scanErr
		}
		parentID, memberID := i.byOID[parentOID], i.byOID[memberOID]
		if parentID == "" || memberID == "" {
			continue
		}
		name := member + "->" + parent + "@" + grantor
		specification := map[string]any{"parent": parent, "member": member, "grantor": grantor, "admin": admin}
		if version >= 160000 {
			specification["inherit"], specification["set"] = inherit, set
		}
		dependencies := []schema.Dependency{{Target: parentID, Type: schema.DependencyReferences}, {Target: memberID, Type: schema.DependencyReferences}}
		if grantorID := i.byOID[grantorOID]; !protectedRole(grantor) && grantorID != "" && grantorID != parentID && grantorID != memberID {
			dependencies = append(dependencies, schema.Dependency{Target: grantorID, Type: schema.DependencyReferences})
		}
		i.add(schema.KindMembership, schema.Name{Name: name}, specification, dependencies, nil)
	}
	return rows.Err()
}

func (i *inspector) inspectDefaultPrivileges(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select d.defaclrole,d.defaclnamespace,d.defaclobjtype::text,grantor.rolname,coalesce(grantee.rolname,'PUBLIC'),a.privilege_type,a.is_grantable,coalesce(n.nspname,'') from pg_default_acl d join pg_roles grantor on grantor.oid=d.defaclrole left join pg_namespace n on n.oid=d.defaclnamespace cross join lateral aclexplode(d.defaclacl) a left join pg_roles grantee on grantee.oid=nullif(a.grantee,0) order by grantor.rolname,n.nspname,d.defaclobjtype,coalesce(grantee.rolname,'PUBLIC'),a.privilege_type`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var ownerOID, namespaceOID uint32
		var objType, owner, grantee, privilege, ns string
		var grantable bool
		if err := rows.Scan(&ownerOID, &namespaceOID, &objType, &owner, &grantee, &privilege, &grantable, &ns); err != nil {
			return err
		}
		ownerID := i.byOID[ownerOID]
		name := owner + ":" + ns + ":" + objType + ":" + grantee + ":" + strings.ToLower(privilege)
		spec := map[string]any{"owner": owner, "object_type": objType, "schema": ns, "grantee": grantee, "privilege": privilege, "grantable": grantable}
		deps := []schema.Dependency{}
		if ownerID != "" && !protectedRole(owner) {
			deps = append(deps, schema.Dependency{Target: ownerID, Type: schema.DependencyReferences})
		}
		if ns != "" {
			if schemaID := i.schemas[ns]; schemaID != "" {
				deps = append(deps, schema.Dependency{Target: schemaID, Type: schema.DependencyReferences})
			}
		}
		if rid := findKindName(i.resources, schema.KindRole, grantee); rid != "" {
			deps = append(deps, schema.Dependency{Target: rid, Type: schema.DependencyReferences})
		}
		i.add(schema.KindDefaultPrivilege, schema.Name{Name: name}, spec, deps, nil)
	}
	return rows.Err()
}

func value(v *string) string {
	if v == nil {
		return ""
	}
	return *v
}
func resourceByID(rs []schema.Resource, id string) schema.Resource {
	for _, r := range rs {
		if r.ID == id {
			return r
		}
	}
	return schema.Resource{}
}
func findKindName(rs []schema.Resource, k schema.Kind, name string) string {
	for _, r := range rs {
		if r.Kind == k && r.Name.Name == name {
			return r.ID
		}
	}
	return ""
}

func splitPatterns(s string) []string {
	var out []string
	for _, v := range strings.FieldsFunc(s, func(r rune) bool { return r == ',' || r == ';' }) {
		if v = strings.TrimSpace(v); v != "" {
			out = append(out, v)
		}
	}
	return out
}

func filterDocument(doc schema.Document, schemas, includes, excludes []string) schema.Document {
	rs := doc.Graph.Resources
	byID := make(map[string]schema.Resource, len(rs))
	children := map[string][]string{}
	for _, r := range rs {
		byID[r.ID] = r
		if r.Name.Parent != "" {
			children[r.Name.Parent] = append(children[r.Name.Parent], r.ID)
		}
	}
	schemaSet := map[string]bool{}
	for _, s := range schemas {
		schemaSet[s] = true
	}
	eligible := func(r schema.Resource) bool {
		// AutoSQL's durable execution ledger is deliberately outside the managed
		// user graph. Keeping the namespace reserved prevents bootstrap metadata
		// from appearing as application drift after a successful convergence.
		if r.Kind == schema.KindSchema && r.Name.Name == "autosql_internal" || r.Name.Schema == "autosql_internal" {
			return false
		}
		if len(schemaSet) > 0 && r.Kind != schema.KindRole {
			if r.Kind == schema.KindSchema {
				if !schemaSet[r.Name.Name] {
					return false
				}
			} else if r.Name.Schema != "" && !schemaSet[r.Name.Schema] {
				return false
			}
		}
		return !matchesAny(r, excludes)
	}
	keep := map[string]bool{}
	if len(includes) == 0 {
		for _, r := range rs {
			if eligible(r) {
				keep[r.ID] = true
			}
		}
	} else {
		for _, r := range rs {
			if eligible(r) && matchesAny(r, includes) {
				keep[r.ID] = true
			}
		}
	}
	// A selected container implies its children, except explicitly excluded ones.
	var addChildren func(string)
	addChildren = func(id string) {
		for _, cid := range children[id] {
			if eligible(byID[cid]) && !keep[cid] {
				keep[cid] = true
				addChildren(cid)
			}
		}
	}
	for id := range keep {
		addChildren(id)
	}
	// Retain parents and referenced dependencies only when eligible. Missing edges are dropped below.
	for changed := true; changed; {
		changed = false
		for id := range keep {
			r := byID[id]
			for _, need := range append([]schema.Dependency(nil), r.Dependencies...) {
				if _, ok := byID[need.Target]; ok && eligible(byID[need.Target]) && !keep[need.Target] {
					keep[need.Target] = true
					changed = true
				}
			}
			if r.Name.Parent != "" && !keep[r.Name.Parent] {
				if p, ok := byID[r.Name.Parent]; ok && eligible(p) {
					keep[p.ID] = true
					changed = true
				}
			}
		}
	}
	out := make([]schema.Resource, 0, len(keep))
	for _, r := range rs {
		if !keep[r.ID] {
			continue
		}
		if r.Name.Parent != "" && !keep[r.Name.Parent] {
			continue
		}
		deps := r.Dependencies[:0]
		for _, d := range r.Dependencies {
			if keep[d.Target] {
				deps = append(deps, d)
			}
		}
		r.Dependencies = deps
		out = append(out, r)
	}
	doc.Graph.Resources = out
	return doc
}

func matchesAny(r schema.Resource, patterns []string) bool {
	for _, p := range patterns {
		for _, candidate := range []string{r.Name.Schema + "." + r.Name.Name, string(r.Kind) + ":" + r.Name.Schema + "." + r.Name.Name, string(r.Kind) + ":" + r.Name.Name, r.Name.Name} {
			if ok, _ := path.Match(p, candidate); ok {
				return true
			}
		}
	}
	return false
}
