package dbtest

import (
	"context"
	"errors"
	"time"

	"autosql/pkg/secret"
)

// CleanupLease verifies one recorded allocation and removes only its owned
// resources. Factory mode and recovery endpoint come from the private lease.
func CleanupLease(ctx context.Context, path string, resolver *secret.Resolver) error {
	return cleanupLeaseMode(ctx, path, resolver, "")
}

func cleanupLeaseMode(ctx context.Context, path string, resolver *secret.Resolver, mode string) error {
	lease, err := openLeaseFile(path)
	if err != nil {
		return &FactoryError{Stage: "cleanup_lease", LeasePath: path, cause: err}
	}
	defer lease.close()
	if mode != "" && lease.record.Factory.Mode != mode {
		return &FactoryError{Stage: "cleanup_mode", LeasePath: path, cause: ErrLease}
	}
	timeout, err := lease.record.Factory.cleanupTimeout()
	if err != nil {
		return err
	}
	return cleanupAllocation(context.WithoutCancel(ctx), lease, resolver, timeout, nil)
}

func cleanupAllocation(ctx context.Context, lease *leaseFile, resolver *secret.Resolver, timeout time.Duration, command dockerCommand) error {
	switch lease.record.Factory.Mode {
	case "existing_admin":
		return cleanupAdminAllocation(ctx, lease, resolver, timeout)
	case "container":
		return cleanupContainerAllocation(ctx, lease, dockerCLI{endpoint: lease.record.Factory.Endpoint, command: command}, timeout)
	default:
		return ErrLease
	}
}

func beginLeaseCleanup(lease *leaseFile) error {
	next := lease.snapshot()
	next.State = "cleaning"
	next.CleanupErrors = nil
	if err := lease.save(next); err != nil {
		return &FactoryError{Stage: "cleanup_record", LeasePath: lease.path, cause: err}
	}
	return nil
}

func finishLeaseCleanup(lease *leaseFile, failures []leaseFailure) error {
	if err := lease.removeCredential(); err != nil {
		failures = append(failures, leaseFailure{Kind: "credentials", Category: "ownership"})
	}
	next := lease.snapshot()
	next.CleanupErrors = failures
	if len(failures) > 0 {
		next.State = "cleanup_failed"
		persistErr := lease.save(next)
		return errors.Join(&FactoryError{Stage: "cleanup", LeasePath: lease.path, cause: cleanupFailuresCause(failures)}, persistErr)
	}
	next.State = "cleaned"
	next.CredentialRef = ""
	if err := lease.save(next); err != nil {
		return &FactoryError{Stage: "cleanup_record", LeasePath: lease.path, cause: err}
	}
	return nil
}
