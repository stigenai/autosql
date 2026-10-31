package dbtest

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"net/url"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

func (f AdminFactory) OpenIsolated(ctx context.Context, caseName string) (Database, error) {
	return f.Create(ctx, caseName)
}

func (f AdminFactory) Create(parent context.Context, caseName string) (*DisposableTarget, error) {
	f, err := f.normalizeReferences()
	if err != nil {
		return nil, &FactoryError{Stage: "configuration", cause: err}
	}
	budgets, err := f.validate()
	if err != nil || strings.TrimSpace(caseName) == "" {
		return nil, &FactoryError{Stage: "configuration", cause: ErrFactoryConfig}
	}
	ctx, cancel := context.WithTimeout(parent, budgets.startup)
	defer cancel()
	authority, err := f.connect(ctx)
	if err != nil {
		return nil, &FactoryError{Stage: "preflight", cause: errors.Join(err, ctx.Err())}
	}
	defer closeAdmin(authority.conn, budgets.cleanup)
	metadata := leaseFactory{Mode: "existing_admin", Name: f.Name, Admin: f.Definition.Admin, AdminIdentity: authority.identity, PostgresMajor: authority.major, CleanupTimeout: budgets.cleanup.String()}
	lease, err := createLease(f.LeaseDirectory, metadata, time.Now().Add(budgets.execution))
	if err != nil {
		return nil, &FactoryError{Stage: "lease", cause: err}
	}
	target, err := f.provision(ctx, authority, lease, budgets)
	if err == nil {
		return target, nil
	}
	closeAdmin(authority.conn, budgets.cleanup) // release the allocation lock before recovery reconnects
	cleanupErr := cleanupAdminAllocation(context.WithoutCancel(parent), lease, f.Resolver, budgets.cleanup)
	lease.close()
	return nil, errors.Join(&FactoryError{Stage: "provision", LeasePath: lease.path, cause: errors.Join(err, ctx.Err())}, cleanupErr)
}

func closeAdmin(conn *pgx.Conn, timeout time.Duration) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	_ = conn.Close(ctx)
}

func lockAdminAllocation(ctx context.Context, conn *pgx.Conn, lease leaseRecord) error {
	var locked bool
	if err := conn.QueryRow(ctx, `SELECT pg_catalog.pg_try_advisory_lock(pg_catalog.hashtextextended($1,0))`, lease.marker()).Scan(&locked); err != nil || !locked {
		return ErrOwnership
	}
	return nil
}

func (f AdminFactory) provision(ctx context.Context, authority adminAuthority, lease *leaseFile, budgets factoryBudgets) (*DisposableTarget, error) {
	if err := lockAdminAllocation(ctx, authority.conn, lease.snapshot()); err != nil {
		return nil, err
	}
	if err := ensureAdminRegistry(ctx, authority); err != nil {
		return nil, err
	}
	password, err := targetPassword()
	if err != nil {
		return nil, err
	}
	if err := f.provisionRole(ctx, authority, lease, password); err != nil {
		return nil, err
	}
	if err := f.provisionDatabase(ctx, authority, lease); err != nil {
		return nil, err
	}
	return f.openTarget(ctx, authority, lease, password, budgets)
}

func targetPassword() (string, error) {
	var raw [32]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", ErrFactoryProvision
	}
	return hex.EncodeToString(raw[:]), nil
}

func planLeaseResource(lease *leaseFile, kind string) (leaseResource, error) {
	next := lease.snapshot()
	resource := leaseResource{Kind: kind, Name: next.resourceName(), Marker: next.marker(), State: "planned"}
	next.Resources = append(next.Resources, resource)
	if err := lease.save(next); err != nil {
		return leaseResource{}, err
	}
	return resource, nil
}

func saveLeaseResource(lease *leaseFile, resource leaseResource) error {
	next := lease.snapshot()
	for i := range next.Resources {
		if next.Resources[i].Kind == resource.Kind {
			next.Resources[i] = resource
			return lease.save(next)
		}
	}
	return ErrLease
}

func (f AdminFactory) checkpoint(stage string, lease *leaseFile) error {
	if f.stage != nil {
		return f.stage(stage, lease.snapshot())
	}
	return nil
}

func (f AdminFactory) provisionRole(ctx context.Context, authority adminAuthority, lease *leaseFile, password string) error {
	resource, err := planLeaseResource(lease, "role")
	if err != nil {
		return err
	}
	if err := f.checkpoint("role_planned", lease); err != nil {
		return err
	}
	if err := createOwnedRole(ctx, authority, lease.record, password, &resource); err != nil {
		return err
	}
	if err := f.checkpoint("role_created", lease); err != nil {
		return err
	}
	if err := saveLeaseResource(lease, resource); err != nil {
		return err
	}
	return f.checkpoint("role_recorded", lease)
}

func createOwnedRole(ctx context.Context, authority adminAuthority, lease leaseRecord, password string, resource *leaseResource) error {
	tx, err := authority.conn.Begin(ctx)
	if err != nil {
		return ErrFactoryProvision
	}
	defer rollbackFactoryTransaction(tx)
	// Names and passwords are generated locally from hex; identifiers still use
	// the driver's quoting. No fixture-controlled SQL enters provisioning.
	query := "CREATE ROLE " + pgx.Identifier{resource.Name}.Sanitize() + " LOGIN NOSUPERUSER NOCREATEDB NOCREATEROLE NOINHERIT NOREPLICATION NOBYPASSRLS CONNECTION LIMIT 16 PASSWORD '" + password + "' VALID UNTIL '" + lease.Deadline.UTC().Format(time.RFC3339Nano) + "'"
	if _, err := tx.Exec(ctx, query); err != nil {
		return ErrFactoryProvision
	}
	if _, err := tx.Exec(ctx, "COMMENT ON ROLE "+pgx.Identifier{resource.Name}.Sanitize()+" IS '"+resource.Marker+"'"); err != nil {
		return ErrFactoryProvision
	}
	resource.OID, _, _, err = readLiveResource(ctx, tx, "role", resource.Name)
	if err != nil || resource.OID == 0 {
		return ErrFactoryProvision
	}
	resource.State = "acquired"
	if err := registerResource(ctx, tx, lease, *resource, authority.owner); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return ErrFactoryProvision
	}
	return nil
}

func (f AdminFactory) provisionDatabase(ctx context.Context, authority adminAuthority, lease *leaseFile) error {
	resource, err := planLeaseResource(lease, "database")
	if err != nil {
		return err
	}
	if err := registerDatabasePlan(ctx, authority, lease.record, resource); err != nil {
		return err
	}
	if err := f.checkpoint("database_planned", lease); err != nil {
		return err
	}
	if _, err := authority.conn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{resource.Name}.Sanitize()+" TEMPLATE template0 ALLOW_CONNECTIONS false"); err != nil {
		return ErrFactoryProvision
	}
	// CREATE DATABASE cannot run inside a transaction. Once acknowledged, a
	// fresh bounded context records its marker/OID even if startup was canceled.
	markCtx, cancel := freshSetupContext(ctx, f.cleanupBudget())
	err = markCreatedDatabase(markCtx, authority, lease.record, &resource)
	cancel()
	if err != nil {
		return err
	}
	if err := f.checkpoint("database_created", lease); err != nil {
		return err
	}
	if err := saveLeaseResource(lease, resource); err != nil {
		return err
	}
	return f.checkpoint("database_recorded", lease)
}

func (f AdminFactory) cleanupBudget() time.Duration {
	budgets, err := parseFactoryBudgets(f.Definition)
	if err != nil {
		return 10 * time.Second
	}
	return budgets.cleanup
}

func registerDatabasePlan(ctx context.Context, authority adminAuthority, lease leaseRecord, resource leaseResource) error {
	tx, err := authority.conn.Begin(ctx)
	if err != nil {
		return ErrFactoryProvision
	}
	defer rollbackFactoryTransaction(tx)
	if err := registerResource(ctx, tx, lease, resource, authority.owner); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return ErrFactoryProvision
	}
	return nil
}

func markCreatedDatabase(ctx context.Context, authority adminAuthority, lease leaseRecord, resource *leaseResource) error {
	tx, err := authority.conn.Begin(ctx)
	if err != nil {
		return ErrFactoryProvision
	}
	defer rollbackFactoryTransaction(tx)
	name := pgx.Identifier{resource.Name}.Sanitize()
	if _, err := tx.Exec(ctx, "COMMENT ON DATABASE "+name+" IS '"+resource.Marker+"'; REVOKE ALL ON DATABASE "+name+" FROM PUBLIC; GRANT CONNECT,CREATE,TEMPORARY ON DATABASE "+name+" TO "+name+"; ALTER DATABASE "+name+" ALLOW_CONNECTIONS true"); err != nil {
		return ErrFactoryProvision
	}
	var owner uint32
	resource.OID, owner, _, err = readLiveResource(ctx, tx, "database", resource.Name)
	if err != nil || resource.OID == 0 || owner != authority.owner {
		return ErrOwnership
	}
	resource.State = "acquired"
	if err := updateRegistryResource(ctx, tx, lease, "database", resource.OID, "acquired"); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return ErrFactoryProvision
	}
	return nil
}

func restrictedTargetURL(admin *url.URL, name, password string) *url.URL {
	target := *admin
	target.User = url.UserPassword(name, password)
	target.Path = "/" + name
	target.RawPath = ""
	query := target.Query()
	query.Del("sslcert")
	query.Del("sslkey")
	query.Set("application_name", "autosql-disposable-test")
	target.RawQuery = query.Encode()
	return &target
}

func (f AdminFactory) openTarget(ctx context.Context, authority adminAuthority, lease *leaseFile, password string, budgets factoryBudgets) (*DisposableTarget, error) {
	u := restrictedTargetURL(authority.url, lease.record.resourceName(), password)
	if err := prepareTargetDatabase(ctx, authority, u, lease.record); err != nil {
		return nil, err
	}
	if err := lease.writeCredential(u.String()); err != nil {
		return nil, err
	}
	conn, err := connectRestrictedTarget(ctx, u, authority, lease.record)
	if err != nil {
		return nil, err
	}
	next := lease.snapshot()
	next.State = "ready"
	if err := lease.save(next); err != nil {
		closeAdmin(conn, budgets.cleanup)
		return nil, err
	}
	if err := f.checkpoint("ready", lease); err != nil {
		closeAdmin(conn, budgets.cleanup)
		return nil, err
	}
	return &DisposableTarget{conn: conn, lease: lease, resolver: f.Resolver, cleanupTimeout: budgets.cleanup}, nil
}

// Compile-time compatibility with existing integer-count harness callers.
var _ Factory = AdminFactory{}

func rollbackFactoryTransaction(tx pgx.Tx) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = tx.Rollback(ctx)
}
