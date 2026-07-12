package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path"
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
	schemas   map[string]string
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
	var last error
	for attempt := 0; attempt < 5; attempt++ {
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
		if !transientCatalogOID(inspectErr) || attempt == 4 {
			return schema.Document{}, inspectErr
		}
		timer := time.NewTimer(time.Duration(attempt+1) * 5 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return schema.Document{}, ctx.Err()
		case <-timer.C:
		}
	}
	return schema.Document{}, last
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
	i := &inspector{conn: conn, byOID: map[uint32]string{}, schemas: map[string]string{}}
	steps := []struct {
		resource, privilege string
		fn                  func(context.Context) error
	}{
		{"schemas", "USAGE on the selected schemas", i.inspectSchemas},
		{"extensions", "USAGE on the extension schemas", i.inspectExtensions},
		{"types", "USAGE on the selected schemas and types", i.inspectTypes},
		{"sequences", "USAGE on the selected schemas", i.inspectSequences},
		{"relations", "USAGE on schemas and SELECT on catalog metadata", i.inspectRelations},
		{"relation dependencies", "USAGE on schemas and SELECT on catalog dependency metadata", i.inspectRelationDependencies},
		{"columns", "USAGE on schemas and SELECT on catalog metadata", i.inspectColumns},
		{"constraints", "USAGE on schemas and SELECT on catalog metadata", i.inspectConstraints},
		{"indexes", "USAGE on schemas and SELECT on catalog metadata", i.inspectIndexes},
		{"routines", "USAGE on schemas and routines", i.inspectRoutines},
		{"triggers", "USAGE on schemas and tables", i.inspectTriggers},
	}
	if enabled(req.Options, "policies", true) {
		steps = append(steps, struct {
			resource, privilege string
			fn                  func(context.Context) error
		}{"row security policies", "USAGE on schemas and ownership or privileges on policy tables", i.inspectPolicies})
	}
	if enabled(req.Options, "roles", false) {
		steps = append(steps, struct {
			resource, privilege string
			fn                  func(context.Context) error
		}{"roles", "pg_read_all_settings or CREATEROLE", i.inspectRoles})
	}
	if enabled(req.Options, "grants", false) {
		steps = append(steps, struct {
			resource, privilege string
			fn                  func(context.Context) error
		}{"grants", "USAGE on schemas and visibility of information_schema grants", i.inspectGrants})
	}
	for _, step := range steps {
		if err := step.fn(ctx); err != nil {
			return schema.Document{}, classify(step.resource, step.privilege, req.URL, err)
		}
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
	rows, err := i.conn.Query(ctx, `select distinct rw.ev_class::oid,d.refobjid::oid from pg_rewrite rw join pg_depend d on d.classid='pg_rewrite'::regclass and d.objid=rw.oid join pg_class v on v.oid=rw.ev_class where v.relkind in ('v','m') and d.refclassid='pg_class'::regclass and d.deptype='n' and d.refobjid<>rw.ev_class order by 1,2`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var from, to uint32
		if err := rows.Scan(&from, &to); err != nil {
			return err
		}
		fromID, toID := i.byOID[from], i.byOID[to]
		if fromID == "" || toID == "" {
			continue
		}
		for idx := range i.resources {
			if i.resources[idx].ID != fromID {
				continue
			}
			exists := false
			for _, dep := range i.resources[idx].Dependencies {
				exists = exists || dep.Target == toID && dep.Type == schema.DependencyReferences
			}
			if !exists {
				i.resources[idx].Dependencies = append(i.resources[idx].Dependencies, schema.Dependency{Target: toID, Type: schema.DependencyReferences})
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
	rows, err := i.conn.Query(ctx, `select oid, nspname, obj_description(oid, 'pg_namespace') from pg_namespace where nspname <> 'information_schema' and nspname !~ '^pg_' order by nspname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var name string
		var comment *string
		if err := rows.Scan(&oid, &name, &comment); err != nil {
			return err
		}
		id := i.add(schema.KindSchema, i.name("", name, ""), map[string]any{}, nil, comment)
		i.schemas[name], i.byOID[oid] = id, id
	}
	return rows.Err()
}

func (i *inspector) inspectExtensions(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select e.oid, e.extname, e.extversion, n.nspname, obj_description(e.oid, 'pg_extension') from pg_extension e join pg_namespace n on n.oid=e.extnamespace order by e.extname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var name, version, ns string
		var comment *string
		if err := rows.Scan(&oid, &name, &version, &ns, &comment); err != nil {
			return err
		}
		parent := i.schemas[ns]
		if parent == "" {
			continue
		}
		id := i.add(schema.KindExtension, i.name(ns, name, parent), map[string]any{"version": version}, dep(parent, schema.DependencyContains), comment)
		i.byOID[oid] = id
	}
	return rows.Err()
}

func (i *inspector) inspectTypes(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `
select t.oid,n.nspname,t.typname,t.typtype::text,format_type(t.typbasetype,t.typtypmod),t.typnotnull,
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
		base           string
		notnull        bool
		def, comment   *string
	}
	var found []typeRow
	for rows.Next() {
		var r typeRow
		if err := rows.Scan(&r.oid, &r.ns, &r.name, &r.kind, &r.base, &r.notnull, &r.def, &r.comment); err != nil {
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
		i.byOID[r.oid] = id
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
	rows, e := i.conn.Query(ctx, `select a.attname,format_type(a.atttypid,a.atttypmod),a.attnotnull from pg_attribute a join pg_type t on t.typrelid=a.attrelid where t.oid=$1 and a.attnum>0 and not a.attisdropped order by a.attnum`, oid)
	if e != nil {
		return nil, e
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var n, t string
		var nn bool
		if e = rows.Scan(&n, &t, &nn); e != nil {
			return nil, e
		}
		out = append(out, map[string]any{"name": n, "type": t, "not_null": nn})
	}
	return out, rows.Err()
}

func (i *inspector) inspectSequences(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select c.oid,n.nspname,c.relname,s.seqstart,s.seqincrement,s.seqmin,s.seqmax,s.seqcache,s.seqcycle,obj_description(c.oid,'pg_class') from pg_class c join pg_namespace n on n.oid=c.relnamespace join pg_sequence s on s.seqrelid=c.oid where c.relkind='S' and n.nspname <> 'information_schema' and n.nspname !~ '^pg_' order by n.nspname,c.relname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var ns, name string
		var start, inc, min, max, cache int64
		var cycle bool
		var comment *string
		if err := rows.Scan(&oid, &ns, &name, &start, &inc, &min, &max, &cache, &cycle, &comment); err != nil {
			return err
		}
		p := i.schemas[ns]
		if p == "" {
			continue
		}
		id := i.add(schema.KindSequence, i.name(ns, name, p), map[string]any{"start": start, "increment": inc, "min": min, "max": max, "cache": cache, "cycle": cycle}, dep(p, schema.DependencyContains), comment)
		i.byOID[oid] = id
	}
	return rows.Err()
}

func (i *inspector) inspectRelations(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select c.oid,n.nspname,c.relname,c.relkind::text,c.relpersistence::text,c.relrowsecurity,c.relforcerowsecurity,pg_get_viewdef(c.oid,true),obj_description(c.oid,'pg_class') from pg_class c join pg_namespace n on n.oid=c.relnamespace where c.relkind in ('r','p','v','m') and n.nspname <> 'information_schema' and n.nspname !~ '^pg_' order by n.nspname,c.relname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var ns, name, relkind, persistence string
		var rls, force bool
		var definition, comment *string
		if err := rows.Scan(&oid, &ns, &name, &relkind, &persistence, &rls, &force, &definition, &comment); err != nil {
			return err
		}
		p := i.schemas[ns]
		if p == "" {
			continue
		}
		kind := schema.KindTable
		spec := map[string]any{}
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
		i.byOID[oid] = id
	}
	return rows.Err()
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
		spec := map[string]any{"position": ordinals[rel], "type": typ, "not_null": nn}
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
		i.add(schema.KindColumn, i.name(ns, name, p), spec, deps, comment)
	}
	return rows.Err()
}

func (i *inspector) inspectConstraints(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select x.oid,x.conrelid,x.confrelid,n.nspname,c.relname,x.conname,x.contype::text,pg_get_constraintdef(x.oid,true),x.condeferrable,x.condeferred,x.convalidated,obj_description(x.oid,'pg_constraint') from pg_constraint x join pg_class c on c.oid=x.conrelid join pg_namespace n on n.oid=c.relnamespace where x.contype in ('p','u','c','f') and n.nspname <> 'information_schema' and n.nspname !~ '^pg_' order by n.nspname,c.relname,x.conname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid, rel, foreign uint32
		var ns, table, name, typ, definition string
		var deferrable, deferred, validated bool
		var comment *string
		if err := rows.Scan(&oid, &rel, &foreign, &ns, &table, &name, &typ, &definition, &deferrable, &deferred, &validated, &comment); err != nil {
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
		if target := i.byOID[foreign]; kind == schema.KindForeignKey && target != "" {
			deps = append(deps, schema.Dependency{Target: target, Type: schema.DependencyReferences})
		}
		id := i.add(kind, i.name(ns, name, p), map[string]any{"definition": definition, "deferrable": deferrable, "initially_deferred": deferred, "validated": validated}, deps, comment)
		i.byOID[oid] = id
	}
	return rows.Err()
}

func (i *inspector) inspectIndexes(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select x.indexrelid,x.indrelid,n.nspname,t.relname,c.relname,am.amname,x.indisunique,x.indisvalid,x.indisready,pg_get_indexdef(x.indexrelid),obj_description(x.indexrelid,'pg_class') from pg_index x join pg_class c on c.oid=x.indexrelid join pg_class t on t.oid=x.indrelid join pg_namespace n on n.oid=t.relnamespace join pg_am am on am.oid=c.relam left join pg_constraint con on con.conindid=x.indexrelid where con.oid is null and n.nspname <> 'information_schema' and n.nspname !~ '^pg_' order by n.nspname,t.relname,c.relname`)
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
		if err := rows.Scan(&oid, &rel, &ns, &table, &name, &method, &unique, &valid, &ready, &definition, &comment); err != nil {
			return err
		}
		if definition == nil {
			return &catalogDisappearanceError{resource: "index definition", oid: oid}
		}
		p := i.byOID[rel]
		if p == "" {
			continue
		}
		id := i.add(schema.KindIndex, i.name(ns, name, p), map[string]any{"method": method, "unique": unique, "valid": valid, "ready": ready, "definition": *definition}, dep(p, schema.DependencyContains), comment)
		i.byOID[oid] = id
	}
	return rows.Err()
}

func (i *inspector) inspectRoutines(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select p.oid,n.nspname,p.proname,p.prokind::text,pg_get_function_identity_arguments(p.oid),coalesce(pg_get_function_result(p.oid),''),l.lanname,p.provolatile::text,p.prosecdef,p.proleakproof,p.proparallel::text,pg_get_functiondef(p.oid),obj_description(p.oid,'pg_proc') from pg_proc p join pg_namespace n on n.oid=p.pronamespace join pg_language l on l.oid=p.prolang where p.prokind in ('f','p') and n.nspname <> 'information_schema' and n.nspname !~ '^pg_' order by n.nspname,p.proname,pg_get_function_identity_arguments(p.oid)`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var ns, name, kind, args, result, language, volatility, parallel, definition string
		var security, leakproof bool
		var comment *string
		if err := rows.Scan(&oid, &ns, &name, &kind, &args, &result, &language, &volatility, &security, &leakproof, &parallel, &definition, &comment); err != nil {
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
		logical := name + "(" + args + ")"
		id := i.add(k, i.name(ns, logical, p), map[string]any{"name": name, "identity_arguments": args, "result": result, "language": language, "volatility": volatility, "security_definer": security, "leakproof": leakproof, "parallel": parallel, "definition": definition}, dep(p, schema.DependencyContains), comment)
		i.byOID[oid] = id
	}
	return rows.Err()
}

func (i *inspector) inspectTriggers(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select g.oid,g.tgrelid,n.nspname,c.relname,g.tgname,pg_get_triggerdef(g.oid,true),g.tgenabled::text,obj_description(g.oid,'pg_trigger') from pg_trigger g join pg_class c on c.oid=g.tgrelid join pg_namespace n on n.oid=c.relnamespace where not g.tgisinternal and n.nspname <> 'information_schema' and n.nspname !~ '^pg_' order by n.nspname,c.relname,g.tgname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid, rel uint32
		var ns, table, name, definition, enabled string
		var comment *string
		if err := rows.Scan(&oid, &rel, &ns, &table, &name, &definition, &enabled, &comment); err != nil {
			return err
		}
		p := i.byOID[rel]
		if p == "" {
			continue
		}
		id := i.add(schema.KindTrigger, i.name(ns, name, p), map[string]any{"definition": definition, "enabled": enabled}, dep(p, schema.DependencyContains), comment)
		i.byOID[oid] = id
	}
	return rows.Err()
}

func (i *inspector) inspectPolicies(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select p.oid,p.polrelid,n.nspname,c.relname,p.polname,p.polcmd::text,p.polpermissive,coalesce(array(select r.rolname from pg_roles r where r.oid=any(p.polroles) order by r.rolname),array[]::name[]),pg_get_expr(p.polqual,p.polrelid),pg_get_expr(p.polwithcheck,p.polrelid),obj_description(p.oid,'pg_policy') from pg_policy p join pg_class c on c.oid=p.polrelid join pg_namespace n on n.oid=c.relnamespace where n.nspname <> 'information_schema' and n.nspname !~ '^pg_' order by n.nspname,c.relname,p.polname`)
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
		id := i.add(schema.KindPolicy, i.name(ns, name, p), spec, dep(p, schema.DependencyContains), comment)
		i.byOID[oid] = id
	}
	return rows.Err()
}

func (i *inspector) inspectRoles(ctx context.Context) error {
	rows, err := i.conn.Query(ctx, `select oid,rolname,rolsuper,rolinherit,rolcreaterole,rolcreatedb,rolcanlogin,rolreplication,rolbypassrls,rolconnlimit,rolvaliduntil::text,obj_description(oid,'pg_authid') from pg_roles order by rolname`)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var oid uint32
		var name string
		var super, inherit, createRole, createDB, login, repl, bypass bool
		var limit int32
		var until, comment *string
		if err := rows.Scan(&oid, &name, &super, &inherit, &createRole, &createDB, &login, &repl, &bypass, &limit, &until, &comment); err != nil {
			return err
		}
		spec := map[string]any{"superuser": super, "inherit": inherit, "create_role": createRole, "create_database": createDB, "login": login, "replication": repl, "bypass_rls": bypass, "connection_limit": limit}
		if until != nil {
			spec["valid_until"] = *until
		}
		id := i.add(schema.KindRole, schema.Name{Name: name}, spec, nil, comment)
		i.byOID[oid] = id
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
