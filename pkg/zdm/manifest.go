package zdm

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

type catalogReader interface {
	Query(context.Context, string, ...any) (pgx.Rows, error)
	QueryRow(context.Context, string, ...any) pgx.Row
}
type columnContract struct {
	typ           string
	nullable      bool
	def, identity string
}

var metadataColumns = map[string]map[string]columnContract{
	"meta":            {"singleton": {"boolean", false, "true", ""}, "schema_version": {"integer", false, "", ""}, "active_version": {"text", false, "''::text", ""}, "completed_version": {"text", false, "''::text", ""}, "phase": {"text", false, "''::text", ""}, "progress": {"integer", false, "0", ""}, "recovery_state": {"text", false, "'clean'::text", ""}, "updated_at": {"timestamp with time zone", false, "", ""}},
	"operations":      {"operation_id": {"text", false, "", ""}, "version": {"text", false, "", ""}, "phase": {"text", false, "", ""}, "progress": {"integer", false, "", ""}, "state": {"text", false, "", ""}, "recovery_state": {"text", false, "", ""}, "created_at": {"timestamp with time zone", false, "", ""}, "updated_at": {"timestamp with time zone", false, "", ""}},
	"object_mappings": {"operation_id": {"text", false, "", ""}, "logical_id": {"text", false, "", ""}, "physical_schema": {"text", false, "", ""}, "physical_name": {"text", false, "", ""}, "object_kind": {"text", false, "", ""}},
	"baselines":       {"baseline_id": {"text", false, "", ""}, "target_identity": {"text", false, "", ""}, "environment": {"text", false, "", ""}, "fingerprint": {"text", false, "", ""}, "canonical_schema": {"text", false, "", ""}, "operator_identity": {"text", false, "", ""}, "created_at": {"timestamp with time zone", false, "", ""}},
	"audit":           {"sequence": {"bigint", false, "", "a"}, "event_type": {"text", false, "", ""}, "subject_id": {"text", false, "", ""}, "target_identity": {"text", false, "", ""}, "environment": {"text", false, "", ""}, "fingerprint": {"text", false, "", ""}, "operator_identity": {"text", false, "", ""}, "detail": {"jsonb", false, "", ""}, "at": {"timestamp with time zone", false, "", ""}}}
var metadataColumnOrder = map[string][]string{"meta": {"singleton", "schema_version", "active_version", "completed_version", "phase", "progress", "recovery_state", "updated_at"}, "operations": {"operation_id", "version", "phase", "progress", "state", "recovery_state", "created_at", "updated_at"}, "object_mappings": {"operation_id", "logical_id", "physical_schema", "physical_name", "object_kind"}, "baselines": {"baseline_id", "target_identity", "environment", "fingerprint", "canonical_schema", "operator_identity", "created_at"}, "audit": {"sequence", "event_type", "subject_id", "target_identity", "environment", "fingerprint", "operator_identity", "detail", "at"}}

func (s *Store) validateLayout(ctx context.Context, db catalogReader) error {
	var schemaOwner, current string
	var schemaACL bool
	if err := db.QueryRow(ctx, `select r.rolname,current_user,n.nspacl is null from pg_namespace n join pg_roles r on r.oid=n.nspowner where n.nspname=$1`, s.cfg.Schema).Scan(&schemaOwner, &current, &schemaACL); err != nil {
		return corrupt("reserved namespace is missing or unreadable")
	}
	if schemaOwner != current || !schemaACL {
		return corrupt("reserved namespace ownership or access grants differ from trusted manifest")
	}
	rows, err := db.Query(ctx, `select c.relname,c.relkind::text,r.rolname,c.relacl is null,c.relpersistence::text,c.relrowsecurity,c.relforcerowsecurity,c.relispartition,c.relreplident::text,coalesce(am.amname,''),c.reltablespace,c.reloptions is null from pg_class c join pg_namespace n on n.oid=c.relnamespace join pg_roles r on r.oid=c.relowner left join pg_am am on am.oid=c.relam where n.nspname=$1 and c.relkind in ('r','p','v','m','f','S') order by c.relname`, s.cfg.Schema)
	if err != nil {
		return err
	}
	tables := map[string]bool{}
	sequence := false
	for rows.Next() {
		var name, kind, owner, persistence, replident, accessMethod string
		var tablespace uint32
		var acl, rls, force, partition, optionsDefault bool
		if err = rows.Scan(&name, &kind, &owner, &acl, &persistence, &rls, &force, &partition, &replident, &accessMethod, &tablespace, &optionsDefault); err != nil {
			rows.Close()
			return err
		}
		if kind == "S" {
			if name != "audit_sequence_seq" || owner != current || !acl || persistence != "p" || accessMethod != "" || tablespace != 0 || !optionsDefault {
				rows.Close()
				return corrupt("identity sequence relation differs from trusted manifest")
			}
			sequence = true
			continue
		}
		if _, ok := metadataColumns[name]; !ok || kind != "r" || owner != current || !acl || persistence != "p" || rls || force || partition || replident != "d" || accessMethod != "heap" || tablespace != 0 || !optionsDefault {
			rows.Close()
			return corrupt("relation %s security, persistence, relkind, or ownership differs from trusted manifest", name)
		}
		tables[name] = true
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if len(tables) != len(metadataColumns) || !sequence {
		return corrupt("reserved namespace relation set differs from trusted manifest")
	}
	for table, want := range metadataColumns {
		rows, err = db.Query(ctx, `select a.attname,format_type(a.atttypid,a.atttypmod),not a.attnotnull,coalesce(pg_get_expr(d.adbin,d.adrelid),''),a.attidentity::text,coalesce(coll.collname,''),a.attstorage::text,a.attcompression::text,a.attgenerated::text,coalesce(a.attstattarget,-1),a.attoptions is null,a.attfdwoptions is null from pg_attribute a join pg_class c on c.oid=a.attrelid join pg_namespace n on n.oid=c.relnamespace left join pg_attrdef d on d.adrelid=a.attrelid and d.adnum=a.attnum left join pg_collation coll on coll.oid=a.attcollation where n.nspname=$1 and c.relname=$2 and a.attnum>0 and not a.attisdropped order by a.attnum`, s.cfg.Schema, table)
		if err != nil {
			return err
		}
		got := map[string]columnContract{}
		var order []string
		for rows.Next() {
			var name string
			var c columnContract
			var collation, storage, compression, generated string
			var stattarget int
			var optionsDefault, fdwDefault bool
			if err = rows.Scan(&name, &c.typ, &c.nullable, &c.def, &c.identity, &collation, &storage, &compression, &generated, &stattarget, &optionsDefault, &fdwDefault); err != nil {
				rows.Close()
				return err
			}
			got[name] = c
			order = append(order, name)
			wantCollation, wantStorage := "", "p"
			if c.typ == "text" {
				wantCollation = "default"
				wantStorage = "x"
			}
			if c.typ == "jsonb" {
				wantStorage = "x"
			}
			if collation != wantCollation || storage != wantStorage || compression != "" || generated != "" || stattarget != -1 || !optionsDefault || !fdwDefault {
				return corrupt("table %s column %s physical/collation contract differs", table, name)
			}
		}
		rows.Close()
		if len(got) != len(want) || strings.Join(order, "\x00") != strings.Join(metadataColumnOrder[table], "\x00") {
			return corrupt("table %s column set differs from trusted manifest", table)
		}
		var dropped int
		if err = db.QueryRow(ctx, `select count(*) from pg_attribute where attrelid=$1::regclass and attnum>0 and attisdropped`, s.cfg.Schema+"."+table).Scan(&dropped); err != nil {
			return err
		}
		if dropped != 0 {
			return corrupt("table %s contains dropped physical columns", table)
		}
		for name, w := range want {
			if g, ok := got[name]; !ok || g != w {
				return corrupt("table %s column %s contract differs", table, name)
			}
		}
		// PostgreSQL 18 represents NOT NULL constraints in pg_constraint in
		// addition to pg_attribute.attnotnull. Nullability is validated above;
		// exclude the redundant version-specific catalog representation here.
		rows, err = db.Query(ctx, `select conname,contype::text,condeferrable,condeferred,convalidated,conislocal,coninhcount,connoinherit,pg_get_constraintdef(oid,false) from pg_constraint where conrelid=$1::regclass and contype<>'n' order by conname`, s.cfg.Schema+"."+table)
		if err != nil {
			return err
		}
		var constraints []string
		for rows.Next() {
			var name, typ, def string
			var a, b, c, local, noinherit bool
			var inhcount int
			if err = rows.Scan(&name, &typ, &a, &b, &c, &local, &inhcount, &noinherit, &def); err != nil {
				rows.Close()
				return err
			}
			constraints = append(constraints, fmt.Sprintf("%s|%s|%t|%t|%t|%t|%d|%t|%s", name, typ, a, b, c, local, inhcount, noinherit, def))
		}
		rows.Close()
		if strings.Join(constraints, "\n") != strings.Join(expectedConstraints(s.cfg.Schema, table), "\n") {
			return corrupt("table %s constraints differ from exact trusted definitions", table)
		}
		rows, err = db.Query(ctx, `select ci.relname,i.indisunique,i.indisprimary,i.indisvalid,i.indisready,i.indislive,i.indisclustered,i.indisreplident,pg_get_indexdef(i.indexrelid,0,false) from pg_index i join pg_class ci on ci.oid=i.indexrelid where i.indrelid=$1::regclass order by ci.relname`, s.cfg.Schema+"."+table)
		if err != nil {
			return err
		}
		var indexes []string
		for rows.Next() {
			var name, def string
			var a, b, c, d, e, f, g bool
			if err = rows.Scan(&name, &a, &b, &c, &d, &e, &f, &g, &def); err != nil {
				rows.Close()
				return err
			}
			indexes = append(indexes, fmt.Sprintf("%s|%t|%t|%t|%t|%t|%t|%t|%s", name, a, b, c, d, e, f, g, def))
		}
		rows.Close()
		if strings.Join(indexes, "\n") != strings.Join(expectedIndexes(s.cfg.Schema, table), "\n") {
			return corrupt("table %s indexes differ from exact trusted definitions", table)
		}
		var rules, triggers, policies int
		if err = db.QueryRow(ctx, `select (select count(*) from pg_rewrite where ev_class=$1::regclass),(select count(*) from pg_trigger where tgrelid=$1::regclass and not tgisinternal),(select count(*) from pg_policy where polrelid=$1::regclass)`, s.cfg.Schema+"."+table).Scan(&rules, &triggers, &policies); err != nil {
			return err
		}
		if rules != 0 || triggers != 0 || policies != 0 {
			return corrupt("table %s has unexpected rules, policies, or non-internal triggers", table)
		}
	}
	return s.validateSequence(ctx, db, current)
}
func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s; do not edit the reserved namespace—restore audited metadata or run a supported explicit upgrade", ErrCorrupt, fmt.Sprintf(format, args...))
}
func expectedConstraints(ns, t string) []string {
	p := "false|false|true|true|0|true|"
	c := "false|false|true|true|0|false|"
	switch t {
	case "meta":
		return []string{"meta_pkey|p|" + p + "PRIMARY KEY (singleton)", "meta_progress_check|c|" + c + "CHECK (((progress >= 0) AND (progress <= 100)))", "meta_singleton_check|c|" + c + "CHECK (singleton)"}
	case "operations":
		return []string{"operations_pkey|p|" + p + "PRIMARY KEY (operation_id)", "operations_progress_check|c|" + c + "CHECK (((progress >= 0) AND (progress <= 100)))"}
	case "object_mappings":
		return []string{"object_mappings_operation_id_fkey|f|" + p + "FOREIGN KEY (operation_id) REFERENCES " + ns + ".operations(operation_id)", "object_mappings_pkey|p|" + p + "PRIMARY KEY (operation_id, logical_id)"}
	case "baselines":
		return []string{"baselines_pkey|p|" + p + "PRIMARY KEY (baseline_id)", "baselines_target_identity_environment_key|u|" + p + "UNIQUE (target_identity, environment)"}
	case "audit":
		return []string{"audit_pkey|p|" + p + "PRIMARY KEY (sequence)"}
	}
	return nil
}
func expectedIndexes(ns, t string) []string {
	one := func(n string, p bool, cols string) string {
		return fmt.Sprintf("%s|true|%t|true|true|true|false|false|CREATE UNIQUE INDEX %s ON %s.%s USING btree (%s)", n, p, n, ns, t, cols)
	}
	switch t {
	case "meta":
		return []string{one("meta_pkey", true, "singleton")}
	case "operations":
		return []string{one("operations_pkey", true, "operation_id")}
	case "object_mappings":
		return []string{one("object_mappings_pkey", true, "operation_id, logical_id")}
	case "baselines":
		return []string{one("baselines_pkey", true, "baseline_id"), one("baselines_target_identity_environment_key", false, "target_identity, environment")}
	case "audit":
		return []string{one("audit_pkey", true, "sequence")}
	}
	return nil
}
func (s *Store) validateSequence(ctx context.Context, db catalogReader, current string) error {
	var typ string
	var start, min, max, inc, cache int64
	var cycle bool
	if err := db.QueryRow(ctx, `select format_type(seqtypid,null),seqstart,seqmin,seqmax,seqincrement,seqcache,seqcycle from pg_sequence where seqrelid=$1::regclass`, s.cfg.Schema+".audit_sequence_seq").Scan(&typ, &start, &min, &max, &inc, &cache, &cycle); err != nil || typ != "bigint" || start != 1 || min != 1 || max != 9223372036854775807 || inc != 1 || cache != 1 || cycle {
		return corrupt("audit identity sequence parameters differ from trusted manifest")
	}
	var owner string
	var bindings int
	if err := db.QueryRow(ctx, `select r.rolname,(select count(*) from pg_depend d join pg_attribute a on a.attrelid=d.refobjid and a.attnum=d.refobjsubid where d.objid=c.oid and d.deptype='i' and d.refobjid=$2::regclass and a.attname='sequence') from pg_class c join pg_roles r on r.oid=c.relowner where c.oid=$1::regclass`, s.cfg.Schema+".audit_sequence_seq", s.cfg.Schema+".audit").Scan(&owner, &bindings); err != nil || owner != current || bindings != 1 {
		return corrupt("audit identity sequence ownership/binding differs from trusted manifest")
	}
	return nil
}
