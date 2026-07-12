package zdm

import (
	"context"
	"fmt"
	"sort"
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
	"meta": {
		"singleton": {"boolean", false, "true", ""}, "schema_version": {"integer", false, "", ""}, "active_version": {"text", false, "''::text", ""},
		"completed_version": {"text", false, "''::text", ""}, "phase": {"text", false, "''::text", ""}, "progress": {"integer", false, "0", ""},
		"recovery_state": {"text", false, "'clean'::text", ""}, "updated_at": {"timestamp with time zone", false, "", ""},
	},
	"operations": {
		"operation_id": {"text", false, "", ""}, "version": {"text", false, "", ""}, "phase": {"text", false, "", ""}, "progress": {"integer", false, "", ""},
		"state": {"text", false, "", ""}, "recovery_state": {"text", false, "", ""}, "created_at": {"timestamp with time zone", false, "", ""}, "updated_at": {"timestamp with time zone", false, "", ""},
	},
	"object_mappings": {
		"operation_id": {"text", false, "", ""}, "logical_id": {"text", false, "", ""}, "physical_schema": {"text", false, "", ""}, "physical_name": {"text", false, "", ""}, "object_kind": {"text", false, "", ""},
	},
	"baselines": {
		"baseline_id": {"text", false, "", ""}, "target_identity": {"text", false, "", ""}, "environment": {"text", false, "", ""}, "fingerprint": {"text", false, "", ""},
		"canonical_schema": {"text", false, "", ""}, "operator_identity": {"text", false, "", ""}, "created_at": {"timestamp with time zone", false, "", ""},
	},
	"audit": {
		"sequence": {"bigint", false, "", "a"}, "event_type": {"text", false, "", ""}, "subject_id": {"text", false, "", ""}, "target_identity": {"text", false, "", ""},
		"environment": {"text", false, "", ""}, "fingerprint": {"text", false, "", ""}, "operator_identity": {"text", false, "", ""}, "detail": {"jsonb", false, "", ""}, "at": {"timestamp with time zone", false, "", ""},
	},
}

var constraintCounts = map[string]map[string]int{
	"meta": {"p": 1, "c": 2}, "operations": {"p": 1, "c": 1}, "object_mappings": {"p": 1, "f": 1}, "baselines": {"p": 1, "u": 1}, "audit": {"p": 1},
}
var indexCounts = map[string]int{"meta": 1, "operations": 1, "object_mappings": 1, "baselines": 2, "audit": 1}

// validateLayout treats the metadata catalog as a signed-format contract, not
// merely a set of conveniently named columns. Any material deviation is unsafe.
func (s *Store) validateLayout(ctx context.Context, db catalogReader) error {
	var schemaOwner, current string
	var schemaACLDefault bool
	if err := db.QueryRow(ctx, `select r.rolname,current_user,n.nspacl is null from pg_namespace n join pg_roles r on r.oid=n.nspowner where n.nspname=$1`, s.cfg.Schema).Scan(&schemaOwner, &current, &schemaACLDefault); err != nil {
		return corrupt("reserved namespace is missing or unreadable")
	}
	if schemaOwner != current {
		return corrupt("reserved namespace owner is %q, expected executing role %q; use the owning role or restore a trusted namespace", schemaOwner, current)
	}
	if !schemaACLDefault {
		return corrupt("reserved namespace has non-default access grants")
	}
	rows, err := db.Query(ctx, `select c.relname,c.relkind::text,r.rolname,c.relacl is null from pg_class c join pg_namespace n on n.oid=c.relnamespace join pg_roles r on r.oid=c.relowner where n.nspname=$1 and c.relkind in ('r','p','v','m','f','S') order by c.relname`, s.cfg.Schema)
	if err != nil {
		return err
	}
	gotTables := map[string]bool{}
	sequenceOK := false
	for rows.Next() {
		var name, kind, owner string
		var aclDefault bool
		if err = rows.Scan(&name, &kind, &owner, &aclDefault); err != nil {
			rows.Close()
			return err
		}
		if kind == "S" {
			if name != "audit_sequence_seq" || owner != current || !aclDefault {
				return corrupt("identity sequence differs from trusted manifest")
			}
			sequenceOK = true
			continue
		}
		if _, ok := metadataColumns[name]; !ok {
			return corrupt("unexpected relation %s exists in reserved namespace", name)
		}
		if kind != "r" {
			return corrupt("relation %s has relkind %s, expected ordinary table", name, kind)
		}
		if owner != current {
			return corrupt("relation %s owner is %q, expected %q", name, owner, current)
		}
		if !aclDefault {
			return corrupt("relation %s has non-default access grants", name)
		}
		gotTables[name] = true
	}
	rows.Close()
	if err = rows.Err(); err != nil {
		return err
	}
	if len(gotTables) != len(metadataColumns) || !sequenceOK {
		return corrupt("reserved namespace relation set differs from trusted manifest")
	}
	var identitySequence *string
	if err = db.QueryRow(ctx, `select pg_get_serial_sequence($1,'sequence')`, s.cfg.Schema+".audit").Scan(&identitySequence); err != nil || identitySequence == nil || *identitySequence != s.cfg.Schema+".audit_sequence_seq" {
		return corrupt("audit identity sequence binding differs from trusted manifest")
	}
	for table, want := range metadataColumns {
		rows, err = db.Query(ctx, `select a.attname,format_type(a.atttypid,a.atttypmod),not a.attnotnull,coalesce(pg_get_expr(d.adbin,d.adrelid),''),a.attidentity::text from pg_attribute a join pg_class c on c.oid=a.attrelid join pg_namespace n on n.oid=c.relnamespace left join pg_attrdef d on d.adrelid=a.attrelid and d.adnum=a.attnum where n.nspname=$1 and c.relname=$2 and a.attnum>0 and not a.attisdropped order by a.attnum`, s.cfg.Schema, table)
		if err != nil {
			return err
		}
		got := map[string]columnContract{}
		for rows.Next() {
			var name string
			var c columnContract
			if err = rows.Scan(&name, &c.typ, &c.nullable, &c.def, &c.identity); err != nil {
				rows.Close()
				return err
			}
			got[name] = c
		}
		rows.Close()
		if len(got) != len(want) {
			return corrupt("table %s column set differs from trusted manifest", table)
		}
		for name, w := range want {
			g, ok := got[name]
			if !ok || g != w {
				return corrupt("table %s column %s contract differs (type/nullability/default/identity)", table, name)
			}
		}
		counts := map[string]int{}
		rows, err = db.Query(ctx, `select contype::text,pg_get_constraintdef(oid,true) from pg_constraint where conrelid=$1::regclass order by oid`, s.cfg.Schema+"."+table)
		if err != nil {
			return err
		}
		var defs []string
		for rows.Next() {
			var typ, def string
			if err = rows.Scan(&typ, &def); err != nil {
				rows.Close()
				return err
			}
			counts[typ]++
			defs = append(defs, strings.ToLower(def))
		}
		rows.Close()
		if !sameCounts(counts, constraintCounts[table]) {
			return corrupt("table %s constraints differ from trusted manifest", table)
		}
		if !validConstraintSemantics(table, defs) {
			return corrupt("table %s constraint semantics differ from trusted manifest", table)
		}
		var indexes int
		err = db.QueryRow(ctx, `select count(*) from pg_index i where i.indrelid=$1::regclass and i.indisvalid and i.indpred is null and i.indexprs is null`, s.cfg.Schema+"."+table).Scan(&indexes)
		if err != nil {
			return err
		}
		if indexes != indexCounts[table] {
			return corrupt("table %s index set differs from trusted manifest", table)
		}
	}
	return nil
}

func corrupt(format string, args ...any) error {
	return fmt.Errorf("%w: %s; do not edit the reserved namespace—restore audited metadata or run a supported explicit upgrade", ErrCorrupt, fmt.Sprintf(format, args...))
}
func sameCounts(a, b map[string]int) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range b {
		if a[k] != v {
			return false
		}
	}
	return true
}
func validConstraintSemantics(table string, defs []string) bool {
	sort.Strings(defs)
	joined := strings.Join(defs, "|")
	switch table {
	case "meta":
		return strings.Contains(joined, "primary key (singleton)") && strings.Contains(joined, "check (singleton)") && strings.Contains(joined, "progress >= 0") && strings.Contains(joined, "progress <= 100")
	case "operations":
		return strings.Contains(joined, "primary key (operation_id)") && strings.Contains(joined, "progress >= 0") && strings.Contains(joined, "progress <= 100")
	case "object_mappings":
		return strings.Contains(joined, "primary key (operation_id, logical_id)") && strings.Contains(joined, "foreign key (operation_id)") && strings.Contains(joined, "references ") && strings.Contains(joined, "operations(operation_id)")
	case "baselines":
		return strings.Contains(joined, "primary key (baseline_id)") && strings.Contains(joined, "unique (target_identity, environment)")
	case "audit":
		return strings.Contains(joined, "primary key (sequence)")
	}
	return false
}
