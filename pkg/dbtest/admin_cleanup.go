package dbtest

import (
	"context"
	"errors"
	"time"

	"autosql/pkg/secret"

	"github.com/jackc/pgx/v5"
)

// CleanupAdminLease removes only the exact resources recorded by one durable
// lease after validating the current server identity and live ownership. It
// never accepts a bare database name or a cleanup-all selector.
func CleanupAdminLease(ctx context.Context, path string, resolver *secret.Resolver) error {
	return cleanupLeaseMode(ctx, path, resolver, "existing_admin")
}

func cleanupAdminAllocation(ctx context.Context, lease *leaseFile, resolver *secret.Resolver, timeout time.Duration) error {
	if lease.record.State == "cleaned" {
		return verifyCleanedAdminAllocation(ctx, lease, resolver, timeout)
	}
	if err := beginLeaseCleanup(lease); err != nil {
		return err
	}
	var failures []leaseFailure
	if len(lease.record.Resources) > 0 {
		failures = cleanupAdminResources(ctx, lease, resolver, timeout)
	}
	return finishLeaseCleanup(lease, failures)
}

func cleanupAdminResources(ctx context.Context, lease *leaseFile, resolver *secret.Resolver, timeout time.Duration) []leaseFailure {
	connectCtx, cancel := freshSetupContext(ctx, timeout)
	authority, err := reconnectLeaseAdmin(connectCtx, lease.record, resolver)
	cancel()
	if err != nil {
		return failedLeaseResources(lease.record, "connection")
	}
	defer closeAdmin(authority.conn, timeout)
	lockCtx, cancel := freshSetupContext(ctx, timeout)
	err = lockAdminAllocation(lockCtx, authority.conn, lease.record)
	cancel()
	if err != nil {
		return failedLeaseResources(lease.record, "ownership")
	}
	var failures []leaseFailure
	for i := len(lease.record.Resources) - 1; i >= 0; i-- {
		resource := lease.snapshot().Resources[i]
		actionCtx, cancel := freshSetupContext(ctx, timeout)
		err = cleanupAdminResource(actionCtx, authority, lease, resource)
		if err != nil {
			err = errors.Join(err, actionCtx.Err())
		}
		cancel()
		if err != nil {
			failures = append(failures, leaseFailure{Kind: resource.Kind, Category: cleanupCategory(err)})
		}
	}
	return failures
}

func failedLeaseResources(record leaseRecord, category string) []leaseFailure {
	var failures []leaseFailure
	for _, resource := range record.Resources {
		failures = append(failures, leaseFailure{Kind: resource.Kind, Category: category})
	}
	return failures
}

func cleanupCategory(err error) string {
	if errors.Is(err, context.DeadlineExceeded) {
		return "timeout"
	}
	if errors.Is(err, ErrLease) {
		return "persist"
	}
	if errors.Is(err, ErrOwnership) {
		return "ownership"
	}
	return "remove"
}

func reconnectLeaseAdmin(ctx context.Context, lease leaseRecord, resolver *secret.Resolver) (adminAuthority, error) {
	if resolver == nil {
		resolver = secret.NewResolver()
	}
	raw, err := resolver.Resolve(ctx, lease.Factory.Admin)
	if err != nil {
		return adminAuthority{}, ErrFactoryConnection
	}
	u, endpoint, err := parseFactoryURL(raw, true)
	if err != nil {
		return adminAuthority{}, err
	}
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		return adminAuthority{}, ErrFactoryConnection
	}
	authority := adminAuthority{conn: conn, url: u}
	err = authority.inspect(ctx, endpoint.database, lease.Factory.PostgresMajor)
	if err != nil || authority.identity != lease.Factory.AdminIdentity {
		closeAdmin(conn, 10*time.Second)
		return adminAuthority{}, ErrOwnership
	}
	return authority, nil
}

func cleanupAdminResource(ctx context.Context, authority adminAuthority, lease *leaseFile, resource leaseResource) error {
	registry, exists, err := verifyOwnedResource(ctx, authority, lease.record, resource)
	if err != nil {
		return err
	}
	if exists {
		query := "DROP ROLE " + pgx.Identifier{resource.Name}.Sanitize()
		if resource.Kind == "database" {
			query = "DROP DATABASE " + pgx.Identifier{resource.Name}.Sanitize() + " WITH (FORCE)"
		}
		if _, err := authority.conn.Exec(ctx, query); err != nil {
			return errors.Join(ErrFactoryProvision, ctx.Err())
		}
	}
	if registry.Kind != "" {
		if err := recordResourceReleased(ctx, authority, lease.record, registry); err != nil {
			return err
		}
	}
	resource.State = "released"
	return saveLeaseResource(lease, resource)
}

func recordResourceReleased(ctx context.Context, authority adminAuthority, lease leaseRecord, r registryResource) error {
	tx, err := authority.conn.Begin(ctx)
	if err != nil {
		return ErrFactoryProvision
	}
	defer rollbackFactoryTransaction(tx)
	if err := updateRegistryResource(ctx, tx, lease, r.Kind, r.OID, "released"); err != nil {
		return err
	}
	if err := tx.Commit(ctx); err != nil {
		return ErrFactoryProvision
	}
	return nil
}

func verifyCleanedAdminAllocation(ctx context.Context, lease *leaseFile, resolver *secret.Resolver, timeout time.Duration) error {
	if len(lease.record.Resources) == 0 {
		return nil
	}
	ctx, cancel := freshSetupContext(ctx, timeout)
	defer cancel()
	authority, err := reconnectLeaseAdmin(ctx, lease.record, resolver)
	if err != nil {
		return &FactoryError{Stage: "cleanup_verify", LeasePath: lease.path, cause: err}
	}
	defer closeAdmin(authority.conn, timeout)
	for _, resource := range lease.record.Resources {
		_, exists, err := verifyOwnedResource(ctx, authority, lease.record, resource)
		if err != nil || exists {
			return &FactoryError{Stage: "cleanup_verify", LeasePath: lease.path, cause: ErrOwnership}
		}
	}
	return nil
}

func cleanupFailuresCause(failures []leaseFailure) error {
	for _, failure := range failures {
		if failure.Category == "ownership" {
			return ErrOwnership
		}
	}
	return ErrFactoryProvision
}
