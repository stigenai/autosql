package expandplan

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"github.com/jackc/pgx/v5"
)

type InspectRequest struct {
	URL, MetadataSchema, Target, Environment, ArtifactDigest string
	Schemas                                                  []string
	BeforeFinalInspection                                    func() error
}

// InspectLive obtains a stable catalog snapshot in a read-only repeatable-read
// transaction. It never creates metadata or application objects.
func InspectLive(ctx context.Context, r InspectRequest) (Snapshot, error) {
	if r.URL == "" || r.MetadataSchema == "" || r.Target == "" || r.Environment == "" || r.ArtifactDigest == "" || len(r.Schemas) == 0 {
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
	if _, err = tx.Exec(ctx, `select pg_catalog.pg_advisory_xact_lock_shared(pg_catalog.hashtextextended($1,0::bigint))`, "autosql.zdm.expand-plan/v1/"+r.Target+"/"+r.Environment); err != nil {
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
	s := Snapshot{Fingerprint: fp, Target: r.Target, Environment: r.Environment, ArtifactDigest: r.ArtifactDigest, PostgresMajor: major, Tables: map[string]Table{}, Indexes: map[string]Index{}, Mappings: map[string]Mapping{}, Schemas: append([]string(nil), r.Schemas...), UniqueEvidence: map[string]UniqueEvidence{}, Constraints: map[string]bool{}, SchemaCreate: map[string]bool{}, ExistingObjects: map[string]string{}}
	for _, ns := range r.Schemas {
		var ok bool
		if err = tx.QueryRow(ctx, `select pg_catalog.has_schema_privilege(current_user,$1,'CREATE')`, ns).Scan(&ok); err != nil {
			return Snapshot{}, err
		}
		s.SchemaCreate[ns] = ok
	}
	rows, err := tx.Query(ctx, `select n.nspname,c.relname,r.rolname,c.relkind='p',pg_catalog.pg_has_role(current_user,c.relowner,'USAGE') from pg_catalog.pg_class c join pg_catalog.pg_namespace n on n.oid=c.relnamespace join pg_catalog.pg_roles r on r.oid=c.relowner where n.nspname=any($1) and c.relkind in ('r','p') order by 1,2`, r.Schemas)
	if err != nil {
		return Snapshot{}, err
	}
	for rows.Next() {
		var t Table
		if err = rows.Scan(&t.Schema, &t.Name, &t.Owner, &t.Partitioned, &t.CanAlter); err != nil {
			rows.Close()
			return Snapshot{}, err
		}
		if _, dup := s.Tables[t.Name]; dup {
			rows.Close()
			return Snapshot{}, refuse("ambiguous unqualified table %s", t.Name)
		}
		t.Columns = map[string]Column{}
		s.Tables[t.Name] = t
		s.ExistingObjects[t.Schema+"."+t.Name] = "table"
	}
	if err = rows.Err(); err != nil {
		return Snapshot{}, err
	}
	rows.Close()
	for _, t := range s.Tables {
		if _, err = tx.Exec(ctx, "LOCK TABLE "+qi(t.Schema, t.Name)+" IN ACCESS SHARE MODE"); err != nil {
			return Snapshot{}, refuse("lock application relation: %v", err)
		}
	}
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
		s.ExistingObjects[ns+"."+col.Name] = "column"
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
		s.ExistingObjects[x.Schema+"."+x.Name] = "index"
	}
	if err = rows.Err(); err != nil {
		return Snapshot{}, err
	}
	rows.Close()
	rows, err = tx.Query(ctx, `select ic.relname,tc.relname,con.oid<>0,i.indisvalid,i.indisready,i.indpred is not null,i.indexprs is not null,array(select a.attname from unnest(i.indkey::smallint[]) with ordinality k(attnum,ord) join pg_catalog.pg_attribute a on a.attrelid=i.indrelid and a.attnum=k.attnum order by k.ord) from pg_catalog.pg_index i join pg_catalog.pg_class ic on ic.oid=i.indexrelid join pg_catalog.pg_namespace n on n.oid=ic.relnamespace join pg_catalog.pg_class tc on tc.oid=i.indrelid left join pg_catalog.pg_constraint con on con.conindid=i.indexrelid where n.nspname=any($1) and i.indisunique order by ic.relname`, r.Schemas)
	if err != nil {
		return Snapshot{}, err
	}
	for rows.Next() {
		var e UniqueEvidence
		if err = rows.Scan(&e.Name, &e.Table, &e.Constraint, &e.Valid, &e.Ready, &e.Partial, &e.Expression, &e.Columns); err != nil {
			rows.Close()
			return Snapshot{}, err
		}
		s.UniqueEvidence[e.Name] = e
	}
	if err = rows.Err(); err != nil {
		return Snapshot{}, err
	}
	rows.Close()
	rows, err = tx.Query(ctx, `select n.nspname,con.conname from pg_catalog.pg_constraint con join pg_catalog.pg_class c on c.oid=con.conrelid join pg_catalog.pg_namespace n on n.oid=c.relnamespace where n.nspname=any($1)`, r.Schemas)
	if err != nil {
		return Snapshot{}, err
	}
	for rows.Next() {
		var ns, name string
		if err = rows.Scan(&ns, &name); err != nil {
			rows.Close()
			return Snapshot{}, err
		}
		s.Constraints[ns+"."+name] = true
		s.ExistingObjects[ns+"."+name] = "constraint"
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
		parts := strings.SplitN(m.LogicalID, "/", 2)
		if len(parts) != 2 {
			return Snapshot{}, refuse("unscoped legacy physical mapping requires audited migration")
		}
		m.Scope = parts[0]
		m.LogicalID = parts[1]
		s.ExistingObjects[m.Schema+"."+m.Name] = "mapped_" + m.Kind
		if m.Scope == mappingScope(r.Target, r.Environment, r.ArtifactDigest) {
			s.Mappings[m.Scope+"/"+m.OperationID+"/"+m.LogicalID] = m
		}
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
	fresh, err := postgres.InspectURL(ctx, r.URL, postgres.Options{Schemas: r.Schemas})
	if err != nil {
		return Snapshot{}, err
	}
	freshFP, err := schema.SemanticFingerprint(fresh)
	if err != nil || freshFP != fp {
		return Snapshot{}, refuse("schema changed at planning fence")
	}
	return s, nil
}
