package dbtest

import (
	"context"
	"errors"
	"net/url"
	"path/filepath"
	"sync"
	"time"

	"autosql/pkg/secret"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// DisposableTarget is returned only after live ownership and the restricted
// execution identity have been checked. It never exposes an administrator URL.
type DisposableTarget struct {
	mu               sync.Mutex
	conn             *pgx.Conn
	lease            *leaseFile
	resolver         *secret.Resolver
	cleanupTimeout   time.Duration
	containerCommand dockerCommand
	closed           bool
}

func (d *DisposableTarget) LeasePath() string { return d.lease.path }
func (d *DisposableTarget) CredentialReference() secret.Reference {
	return secret.Reference((&url.URL{Scheme: "file", Path: filepath.Join(filepath.Dir(d.lease.path), credentialFilename)}).String())
}
func (d *DisposableTarget) PostgresMajor() int {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.lease.record.Factory.PostgresMajor
}

func (d *DisposableTarget) Exec(ctx context.Context, query string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.beginUse(); err != nil {
		return err
	}
	ctx, cancel := context.WithDeadline(ctx, d.lease.record.Deadline)
	defer cancel()
	_, err := d.conn.Exec(ctx, query)
	return boundedDatabaseError(err)
}

func (d *DisposableTarget) QueryCount(ctx context.Context, query string, args ...any) (int64, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if err := d.beginUse(); err != nil {
		return 0, err
	}
	ctx, cancel := context.WithDeadline(ctx, d.lease.record.Deadline)
	defer cancel()
	var result int64
	err := d.conn.QueryRow(ctx, query, args...).Scan(&result)
	return result, boundedDatabaseError(err)
}

func (d *DisposableTarget) beginUse() error {
	if d.closed || !d.lease.record.Deadline.After(time.Now()) {
		return ErrLease
	}
	if d.lease.record.State == "ready" {
		next := d.lease.snapshot()
		next.State = "running"
		return d.lease.save(next)
	}
	if d.lease.record.State != "running" {
		return ErrLease
	}
	return nil
}

func (d *DisposableTarget) Close(ctx context.Context) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil
	}
	d.closed = true
	closeAdmin(d.conn, d.cleanupTimeout)
	err := cleanupAllocation(context.WithoutCancel(ctx), d.lease, d.resolver, d.cleanupTimeout, d.containerCommand)
	d.lease.close()
	return err
}

// Detach closes this process's fixture connection and releases its local lease
// lock, retaining resources and the private credential file for explicit
// interactive use. CleanupLease is then required to remove them.
func (d *DisposableTarget) Detach() error {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return ErrLease
	}
	d.closed = true
	closeAdmin(d.conn, d.cleanupTimeout)
	d.lease.close()
	return nil
}

type DatabaseError struct{ State string }

func (e *DatabaseError) Error() string {
	return "disposable database operation failed (SQLSTATE " + e.State + ")"
}
func (e *DatabaseError) SQLState() string { return e.State }

func boundedDatabaseError(err error) error {
	if err == nil {
		return nil
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	var postgres *pgconn.PgError
	if errors.As(err, &postgres) {
		return &DatabaseError{State: postgres.Code}
	}
	return ErrFactoryConnection
}

func prepareTargetDatabase(ctx context.Context, authority adminAuthority, target *url.URL, lease leaseRecord) error {
	u := *authority.url
	u.Path = target.Path
	u.RawPath = ""
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		return ErrFactoryConnection
	}
	defer closeAdmin(conn, 10*time.Second)
	if err := verifyTargetConnection(ctx, conn, authority, lease, false); err != nil {
		return err
	}
	var allowed bool
	if err := conn.QueryRow(ctx, `SELECT pg_catalog.has_schema_privilege($1,'public','USAGE') AND pg_catalog.has_schema_privilege($1,'public','CREATE')`, lease.resourceName()).Scan(&allowed); err != nil {
		return ErrFactoryProvision
	}
	if !allowed {
		if _, err := conn.Exec(ctx, "GRANT USAGE,CREATE ON SCHEMA public TO "+pgx.Identifier{lease.resourceName()}.Sanitize()); err != nil {
			return ErrFactoryProvision
		}
	}
	return nil
}

func connectRestrictedTarget(ctx context.Context, u *url.URL, authority adminAuthority, lease leaseRecord) (*pgx.Conn, error) {
	cfg, err := pgx.ParseConfig(u.String())
	if err != nil {
		return nil, ErrFactoryConfig
	}
	// Never reuse an administrator client certificate inherited through the
	// environment. Restricted fixture authentication uses its generated secret.
	if cfg.TLSConfig != nil {
		cfg.TLSConfig.Certificates = nil
		cfg.TLSConfig.GetClientCertificate = nil
	}
	for _, fallback := range cfg.Fallbacks {
		if fallback.TLSConfig != nil {
			fallback.TLSConfig.Certificates = nil
			fallback.TLSConfig.GetClientCertificate = nil
		}
	}
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		return nil, ErrFactoryConnection
	}
	if err := verifyTargetConnection(ctx, conn, authority, lease, true); err != nil {
		closeAdmin(conn, 10*time.Second)
		return nil, err
	}
	return conn, nil
}

func verifyTargetConnection(ctx context.Context, conn *pgx.Conn, authority adminAuthority, lease leaseRecord, restricted bool) error {
	if conn.PgConn().Conn().RemoteAddr().String() != authority.conn.PgConn().Conn().RemoteAddr().String() {
		return ErrOwnership
	}
	var database, name string
	var databaseOID, roleOID uint32
	var unprivileged bool
	err := conn.QueryRow(ctx, `SELECT current_database(),current_user,d.oid,r.oid,
 NOT(rolsuper OR rolcreatedb OR rolcreaterole OR rolreplication OR rolbypassrls OR rolinherit)
 FROM pg_catalog.pg_database d,pg_catalog.pg_roles r
 WHERE d.datname=current_database() AND r.rolname=current_user`).Scan(&database, &name, &databaseOID, &roleOID, &unprivileged)
	if err != nil || database != lease.resourceName() || databaseOID != resourceOID(lease, "database") {
		return ErrOwnership
	}
	return verifyTargetRole(name, roleOID, unprivileged, authority, lease, restricted)
}

func verifyTargetRole(name string, roleOID uint32, unprivileged bool, authority adminAuthority, lease leaseRecord, restricted bool) error {
	if restricted && (name != lease.resourceName() || roleOID != resourceOID(lease, "role") || !unprivileged) {
		return ErrOwnership
	}
	if !restricted && roleOID != authority.owner {
		return ErrOwnership
	}
	return nil
}

func resourceOID(lease leaseRecord, kind string) uint32 {
	for _, resource := range lease.Resources {
		if resource.Kind == kind {
			return resource.OID
		}
	}
	return 0
}

var _ Database = (*DisposableTarget)(nil)
