package dbtest

import (
	"context"
	"errors"
	"time"
)

func cleanupContainerAllocation(ctx context.Context, lease *leaseFile, d dockerCLI, timeout time.Duration) error {
	if lease.record.State == "cleaned" {
		return verifyCleanedContainer(ctx, lease, d, timeout)
	}
	if err := beginLeaseCleanup(lease); err != nil {
		return err
	}
	var failures []leaseFailure
	if resource, found := leasedContainer(lease.record); found {
		actionCtx, cancel := freshSetupContext(ctx, timeout)
		err := removeOwnedContainer(actionCtx, d, lease.record, resource)
		if err != nil {
			err = errors.Join(err, actionCtx.Err())
		}
		cancel()
		if err != nil {
			failures = append(failures, leaseFailure{Kind: "container", Category: containerCleanupCategory(err)})
		}
	}
	if len(failures) == 0 {
		if err := releaseContainerResources(lease); err != nil {
			return &FactoryError{Stage: "cleanup_record", LeasePath: lease.path, cause: err}
		}
	}
	return finishLeaseCleanup(lease, failures)
}

func leasedContainer(record leaseRecord) (leaseResource, bool) {
	for _, resource := range record.Resources {
		if resource.Kind == "container" {
			return resource, true
		}
	}
	return leaseResource{}, false
}

func containerCleanupCategory(err error) string {
	if errors.Is(err, ErrFactoryConnection) {
		return "connection"
	}
	return cleanupCategory(err)
}

func verifyContainerDaemon(ctx context.Context, d dockerCLI, lease leaseRecord) error {
	actual, err := d.daemonID(ctx)
	if err != nil {
		return err
	}
	if actual != lease.Factory.DaemonID {
		return ErrOwnership
	}
	return nil
}

func removeOwnedContainer(ctx context.Context, d dockerCLI, lease leaseRecord, resource leaseResource) error {
	if err := verifyContainerDaemon(ctx, d, lease); err != nil {
		return err
	}
	container, exists, err := d.ownedContainer(ctx, lease, resource)
	if err != nil || !exists {
		return err
	}
	if resource.State == "released" {
		return ErrOwnership
	}
	// Removing the verified container removes its entire fresh database scope.
	// No host mounts or volumes are permitted, and no --volumes deletion is used.
	if _, err := d.run(ctx, nil, "container", "rm", "--force", container.ID); err != nil {
		return err
	}
	_, exists, err = d.ownedContainer(ctx, lease, resource)
	if err != nil || exists {
		return errors.Join(ErrOwnership, err)
	}
	return nil
}

func releaseContainerResources(lease *leaseFile) error {
	next := lease.snapshot()
	for i := range next.Resources {
		next.Resources[i].State = "released"
	}
	return lease.save(next)
}

func verifyCleanedContainer(ctx context.Context, lease *leaseFile, d dockerCLI, timeout time.Duration) error {
	resource, found := leasedContainer(lease.record)
	if !found {
		return nil
	}
	ctx, cancel := freshSetupContext(ctx, timeout)
	defer cancel()
	if err := verifyContainerDaemon(ctx, d, lease.record); err != nil {
		return &FactoryError{Stage: "cleanup_verify", LeasePath: lease.path, cause: err}
	}
	_, exists, err := d.ownedContainer(ctx, lease.record, resource)
	if err != nil || exists {
		return &FactoryError{Stage: "cleanup_verify", LeasePath: lease.path, cause: errors.Join(ErrOwnership, err)}
	}
	return nil
}
