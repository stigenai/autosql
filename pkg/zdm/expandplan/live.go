package expandplan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"github.com/jackc/pgx/v5"
)

type InspectRequest struct {
	URL, MetadataSchema, Target, Environment string
	Schemas                                  []string
	BeforeFinalInspection                    func() error
}

// InspectLive obtains a stable catalog snapshot in a read-only repeatable-read
// transaction. It never creates metadata or application objects.
func InspectLive(ctx context.Context, r InspectRequest) (Snapshot, error) {
	if r.URL == "" || r.MetadataSchema == "" || r.Target == "" || r.Environment == "" || len(r.Schemas) == 0 {
		return Snapshot{}, refuse("URL, metadata schema, target, environment, and application schemas are required")
	}
	c, err := pgx.Connect(ctx, r.URL)
	if err != nil {
		return Snapshot{}, fmt.Errorf("connect target: %w", err)
	}
	defer c.Close(context.WithoutCancel(ctx))
	tx, err := c.BeginTx(ctx, pgx.TxOptions{IsoLevel: pgx.RepeatableRead, AccessMode: pgx.ReadOnly})
	if err != nil {
		return Snapshot{}, err
	}
	defer tx.Rollback(context.WithoutCancel(ctx))
	if _, err = tx.Exec(ctx, `set local search_path=pg_catalog`); err != nil {
		return Snapshot{}, err
	}
	var major int
	if err = tx.QueryRow(ctx, `select current_setting('server_version_num')::int/10000`).Scan(&major); err != nil {
		return Snapshot{}, err
	}
	doc, err := postgres.InspectTx(ctx, tx, postgres.Options{Schemas: r.Schemas})
	if err != nil {
		return Snapshot{}, fmt.Errorf("inspect application schema: %w", err)
	}
	fp, err := schema.SemanticFingerprint(doc)
	if err != nil {
		return Snapshot{}, err
	}
	canonical, err := doc.MarshalCanonical()
	if err != nil {
		return Snapshot{}, err
	}
	s := Snapshot{Fingerprint: fp, Target: r.Target, Environment: r.Environment, PostgresMajor: major, Tables: map[string]Table{}, Indexes: map[string]Index{}, Mappings: map[string]Mapping{}, Schemas: append([]string(nil), r.Schemas...)}
	rows, err := tx.Query(ctx, `select n.nspname,c.relname,r.rolname,c.relkind='p' from pg_catalog.pg_class c join pg_catalog.pg_namespace n on n.oid=c.relnamespace join pg_catalog.pg_roles r on r.oid=c.relowner where n.nspname=any($1) and c.relkind in ('r','p') order by 1,2`, r.Schemas)
	if err != nil {
		return Snapshot{}, err
	}
	for rows.Next() {
		var t Table
		if err = rows.Scan(&t.Schema, &t.Name, &t.Owner, &t.Partitioned); err != nil {
			rows.Close()
			return Snapshot{}, err
		}
		if _, dup := s.Tables[t.Name]; dup {
			rows.Close()
			return Snapshot{}, refuse("ambiguous unqualified table %s", t.Name)
		}
		t.Columns = map[string]Column{}
		s.Tables[t.Name] = t
	}
	if err = rows.Err(); err != nil {
		return Snapshot{}, err
	}
	rows.Close()
	rows, err = tx.Query(ctx, `select n.nspname,c.relname,a.attname,pg_catalog.format_type(a.atttypid,a.atttypmod),not a.attnotnull from pg_catalog.pg_attribute a join pg_catalog.pg_class c on c.oid=a.attrelid join pg_catalog.pg_namespace n on n.oid=c.relnamespace where n.nspname=any($1) and c.relkind in ('r','p') and a.attnum>0 and not a.attisdropped order by 1,2,a.attnum`, r.Schemas)
	if err != nil {
		return Snapshot{}, err
	}
	for rows.Next() {
		var ns, tn string
		var col Column
		if err = rows.Scan(&ns, &tn, &col.Name, &col.Type, &col.Nullable); err != nil {
			rows.Close()
			return Snapshot{}, err
		}
		t, ok := s.Tables[tn]
		if !ok || t.Schema != ns {
			rows.Close()
			return Snapshot{}, refuse("ambiguous column catalog")
		}
		t.Columns[col.Name] = col
		s.Tables[tn] = t
	}
	if err = rows.Err(); err != nil {
		return Snapshot{}, err
	}
	rows.Close()
	rows, err = tx.Query(ctx, `select ni.nspname,ci.relname,ct.relname,r.rolname,coalesce(con.oid,0)<>0,ct.relkind='p' from pg_catalog.pg_index i join pg_catalog.pg_class ci on ci.oid=i.indexrelid join pg_catalog.pg_namespace ni on ni.oid=ci.relnamespace join pg_catalog.pg_class ct on ct.oid=i.indrelid join pg_catalog.pg_roles r on r.oid=ci.relowner left join pg_catalog.pg_constraint con on con.conindid=i.indexrelid where ni.nspname=any($1) order by 1,2`, r.Schemas)
	if err != nil {
		return Snapshot{}, err
	}
	for rows.Next() {
		var x Index
		if err = rows.Scan(&x.Schema, &x.Name, &x.Table, &x.Owner, &x.ConstraintBacked, &x.Partitioned); err != nil {
			rows.Close()
			return Snapshot{}, err
		}
		if _, dup := s.Indexes[x.Name]; dup {
			rows.Close()
			return Snapshot{}, refuse("ambiguous unqualified index %s", x.Name)
		}
		s.Indexes[x.Name] = x
	}
	if err = rows.Err(); err != nil {
		return Snapshot{}, err
	}
	rows.Close()
	q := pgx.Identifier{r.MetadataSchema}.Sanitize()
	var storedFP string
	var stored []byte
	if err = tx.QueryRow(ctx, `select fingerprint,canonical_schema from `+q+`.baselines where target_identity=$1 and environment=$2`, r.Target, r.Environment).Scan(&storedFP, &stored); err != nil {
		return Snapshot{}, refuse("trusted baseline unavailable: %v", err)
	}
	var storedDoc schema.Document
	if json.Unmarshal(stored, &storedDoc) != nil {
		return Snapshot{}, refuse("baseline canonical schema is corrupt")
	}
	checkFP, e := schema.SemanticFingerprint(storedDoc)
	if e != nil || checkFP != storedFP || storedFP != fp || !bytes.Equal(stored, canonical) {
		return Snapshot{}, refuse("live schema drifted from trusted baseline")
	}
	rows, err = tx.Query(ctx, `select operation_id,logical_id,physical_schema,physical_name,object_kind from `+q+`.object_mappings order by operation_id,logical_id`)
	if err != nil {
		return Snapshot{}, refuse("read physical mappings: %v", err)
	}
	for rows.Next() {
		var m Mapping
		if err = rows.Scan(&m.OperationID, &m.LogicalID, &m.Schema, &m.Name, &m.Kind); err != nil {
			rows.Close()
			return Snapshot{}, err
		}
		s.Mappings[m.OperationID+"/"+m.LogicalID] = m
	}
	if err = rows.Err(); err != nil {
		return Snapshot{}, err
	}
	rows.Close()
	if r.BeforeFinalInspection != nil {
		if err = r.BeforeFinalInspection(); err != nil {
			return Snapshot{}, err
		}
	}
	final, err := postgres.InspectTx(ctx, tx, postgres.Options{Schemas: r.Schemas})
	if err != nil {
		return Snapshot{}, err
	}
	finalFP, err := schema.SemanticFingerprint(final)
	if err != nil || finalFP != fp {
		return Snapshot{}, refuse("schema changed during planning")
	}
	if err = tx.Commit(ctx); err != nil {
		return Snapshot{}, err
	}
	return s, nil
}
