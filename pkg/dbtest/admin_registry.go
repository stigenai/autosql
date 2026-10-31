package dbtest

import (
	"context"
	"errors"

	"github.com/jackc/pgx/v5"
)

const registrySchema = "autosql_development_leases"
const registryMarker = "autosql.development-registry/v1"

var errRegistryMissing = errors.New("development creation record missing")

const createRegistrySQL = `
CREATE SCHEMA autosql_development_leases;
REVOKE ALL ON SCHEMA autosql_development_leases FROM PUBLIC;
COMMENT ON SCHEMA autosql_development_leases IS 'autosql.development-registry/v1';
CREATE TABLE autosql_development_leases.resources (
 run_id text NOT NULL,
 case_id text NOT NULL,
 kind text NOT NULL CHECK (kind IN ('role','database')),
 name text NOT NULL,
 marker text NOT NULL,
 admin_oid oid NOT NULL,
 resource_oid oid,
 state text NOT NULL CHECK (state IN ('planned','acquired','released')),
 PRIMARY KEY(run_id,case_id,kind),
 UNIQUE(kind,name)
);
REVOKE ALL ON TABLE autosql_development_leases.resources FROM PUBLIC;
COMMENT ON TABLE autosql_development_leases.resources IS 'autosql.development-registry/v1';`

// Registry metadata belongs to the provisioner on the approved development
// administration database. Fixtures get no access to its schema or tables.
// The table retains creation records after cleanup; no database contents are
// cached or reused between cases.
func ensureAdminRegistry(ctx context.Context, authority adminAuthority) error {
	tx, err := authority.conn.Begin(ctx)
	if err != nil {
		return ErrFactoryProvision
	}
	defer rollbackFactoryTransaction(tx)
	if _, err := tx.Exec(ctx, `SELECT pg_catalog.pg_advisory_xact_lock(pg_catalog.hashtextextended($1,0))`, registryMarker); err != nil {
		return ErrFactoryProvision
	}
	var exists bool
	if err := tx.QueryRow(ctx, `SELECT pg_catalog.to_regnamespace($1) IS NOT NULL`, registrySchema).Scan(&exists); err != nil {
		return ErrFactoryProvision
	}
	if !exists {
		if _, err := tx.Exec(ctx, createRegistrySQL); err != nil {
			return ErrFactoryProvision
		}
	}
	if err := verifyAdminRegistry(ctx, tx, authority.owner); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return ErrFactoryProvision
	}
	return nil
}

type registryReader interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}

func verifyAdminRegistry(ctx context.Context, q registryReader, owner uint32) error {
	var valid bool
	err := q.QueryRow(ctx, `SELECT
 n.nspowner=$1 AND c.relowner=$1 AND c.relkind='r'
 AND NOT c.relrowsecurity AND NOT c.relhasrules AND NOT c.relhastriggers
 AND pg_catalog.obj_description(n.oid,'pg_namespace')=$2
 AND pg_catalog.obj_description(c.oid,'pg_class')=$2
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.aclexplode(COALESCE(n.nspacl,pg_catalog.acldefault('n',n.nspowner))) a WHERE a.grantee<>$1)
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.aclexplode(COALESCE(c.relacl,pg_catalog.acldefault('r',c.relowner))) a WHERE a.grantee<>$1)
 FROM pg_catalog.pg_namespace n JOIN pg_catalog.pg_class c ON c.relnamespace=n.oid
 WHERE n.nspname=$3 AND c.relname='resources'`, owner, registryMarker, registrySchema).Scan(&valid)
	if err != nil || !valid {
		return ErrOwnership
	}
	return nil
}

type registryResource struct {
	Kind, Name, Marker, State string
	AdminOID, OID             uint32
}

func readRegistryResource(ctx context.Context, q registryReader, lease leaseRecord, kind string) (registryResource, error) {
	var r registryResource
	err := q.QueryRow(ctx, `SELECT kind,name,marker,state,admin_oid,COALESCE(resource_oid,0::oid)
 FROM autosql_development_leases.resources WHERE run_id=$1 AND case_id=$2 AND kind=$3`, lease.RunID, lease.CaseID, kind).Scan(&r.Kind, &r.Name, &r.Marker, &r.State, &r.AdminOID, &r.OID)
	if errors.Is(err, pgx.ErrNoRows) {
		return registryResource{}, errRegistryMissing
	}
	if err != nil {
		return registryResource{}, ErrOwnership
	}
	if r.Kind != kind || r.Name != lease.resourceName() || r.Marker != lease.marker() {
		return registryResource{}, ErrOwnership
	}
	return r, nil
}

func registerResource(ctx context.Context, tx pgx.Tx, lease leaseRecord, resource leaseResource, owner uint32) error {
	_, err := tx.Exec(ctx, `INSERT INTO autosql_development_leases.resources
 (run_id,case_id,kind,name,marker,admin_oid,resource_oid,state)
 VALUES($1,$2,$3,$4,$5,$6,NULLIF($7::oid,0::oid),$8)`, lease.RunID, lease.CaseID, resource.Kind, resource.Name, resource.Marker, owner, resource.OID, resource.State)
	if err != nil {
		return ErrFactoryProvision
	}
	return nil
}

func updateRegistryResource(ctx context.Context, tx pgx.Tx, lease leaseRecord, kind string, oid uint32, state string) error {
	result, err := tx.Exec(ctx, `UPDATE autosql_development_leases.resources SET resource_oid=$4,state=$5
 WHERE run_id=$1 AND case_id=$2 AND kind=$3`, lease.RunID, lease.CaseID, kind, oid, state)
	if err != nil || result.RowsAffected() != 1 {
		return ErrOwnership
	}
	return nil
}

func readLiveResource(ctx context.Context, q registryReader, kind, name string) (oid, owner uint32, marker string, err error) {
	query := `SELECT oid,0::oid,COALESCE(pg_catalog.shobj_description(oid,'pg_authid'),'') FROM pg_catalog.pg_roles WHERE rolname=$1`
	if kind == "database" {
		query = `SELECT oid,datdba,COALESCE(pg_catalog.shobj_description(oid,'pg_database'),'') FROM pg_catalog.pg_database WHERE datname=$1`
	}
	err = q.QueryRow(ctx, query, name).Scan(&oid, &owner, &marker)
	if errors.Is(err, pgx.ErrNoRows) {
		return 0, 0, "", nil
	}
	if err != nil {
		return 0, 0, "", ErrOwnership
	}
	return oid, owner, marker, nil
}

func verifyOwnedResource(ctx context.Context, authority adminAuthority, lease leaseRecord, resource leaseResource) (registryResource, bool, error) {
	if err := verifyAdminRegistry(ctx, authority.conn, authority.owner); err != nil {
		return registryResource{}, false, err
	}
	r, err := readRegistryResource(ctx, authority.conn, lease, resource.Kind)
	oid, owner, marker, liveErr := readLiveResource(ctx, authority.conn, resource.Kind, resource.Name)
	if registryAndObjectAbsent(err, liveErr, oid) {
		return registryResource{}, false, nil
	}
	if err != nil || liveErr != nil || r.AdminOID != authority.owner {
		return registryResource{}, false, ErrOwnership
	}
	if resource.OID != 0 && resource.OID != r.OID {
		return registryResource{}, false, ErrOwnership
	}
	if oid == 0 {
		return r, false, nil
	} // absence permits recording cleanup, never deletion
	if err := verifyLiveOwnership(ctx, authority, r, resource, oid, owner, marker); err != nil {
		return registryResource{}, false, err
	}
	return r, true, nil
}

func registryAndObjectAbsent(registryErr, liveErr error, oid uint32) bool {
	return errors.Is(registryErr, errRegistryMissing) && liveErr == nil && oid == 0
}

func verifyLiveOwnership(ctx context.Context, authority adminAuthority, r registryResource, resource leaseResource, oid, owner uint32, marker string) error {
	if r.State == "released" || r.OID == 0 || oid != r.OID || marker != resource.Marker {
		return ErrOwnership
	}
	if resource.Kind == "database" {
		if owner != authority.owner {
			return ErrOwnership
		}
		return nil
	}
	var restricted bool
	err := authority.conn.QueryRow(ctx, `SELECT NOT(rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls OR rolinherit)
 AND NOT EXISTS(SELECT 1 FROM pg_catalog.pg_auth_members m WHERE m.member=$1 OR (m.roleid=$1 AND m.member<>$2))
 FROM pg_catalog.pg_roles WHERE oid=$1`, oid, authority.owner).Scan(&restricted)
	if err != nil || !restricted {
		return ErrOwnership
	}
	return nil
}
