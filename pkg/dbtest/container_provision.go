package dbtest

import (
	"context"
	"errors"
	"net"
	"net/url"
	"strings"
	"time"
)

func (f ContainerFactory) OpenIsolated(ctx context.Context, caseName string) (Database, error) {
	return f.Create(ctx, caseName)
}

func (f ContainerFactory) Create(parent context.Context, caseName string) (*DisposableTarget, error) {
	budgets, err := f.validate()
	if err != nil || strings.TrimSpace(caseName) == "" {
		return nil, &FactoryError{Stage: "configuration", cause: ErrFactoryConfig}
	}
	ctx, cancel := context.WithTimeout(parent, budgets.startup)
	defer cancel()
	endpoint, protected, daemon, err := f.preflight(ctx)
	if err != nil {
		return nil, err
	}
	d := dockerCLI{endpoint: endpoint.daemon, command: f.command}
	metadata := leaseFactory{Mode: "container", Name: f.Name, Endpoint: endpoint.daemon, DaemonID: daemon, Image: f.Definition.Image, PostgresMajor: f.Definition.PostgresVersion, CleanupTimeout: budgets.cleanup.String()}
	lease, err := createLease(f.LeaseDirectory, metadata, time.Now().Add(budgets.execution))
	if err != nil {
		return nil, &FactoryError{Stage: "lease", cause: err}
	}
	target, err := f.provisionContainer(ctx, d, endpoint, protected, lease, budgets)
	if err == nil {
		target.containerCommand = f.command
		return target, nil
	}
	cleanupErr := cleanupContainerAllocation(context.WithoutCancel(parent), lease, d, budgets.cleanup)
	lease.close()
	return nil, errors.Join(&FactoryError{Stage: "container_provision", LeasePath: lease.path, cause: errors.Join(err, ctx.Err())}, cleanupErr)
}

func (f ContainerFactory) preflight(ctx context.Context) (containerEndpoint, []databaseEndpoint, string, error) {
	endpoint, err := f.approvedEndpoint(ctx)
	if err != nil {
		return containerEndpoint{}, nil, "", &FactoryError{Stage: "container_configuration", cause: errors.Join(err, ctx.Err())}
	}
	protected, err := factoryEndpointList(ctx, f.Definition.ProtectedTargets)
	if err != nil {
		return containerEndpoint{}, nil, "", &FactoryError{Stage: "container_protected_targets", cause: errors.Join(err, ctx.Err())}
	}
	id, err := (dockerCLI{endpoint: endpoint.daemon, command: f.command}).daemonID(ctx)
	if err != nil {
		return containerEndpoint{}, nil, "", &FactoryError{Stage: "container_daemon", cause: errors.Join(err, ctx.Err())}
	}
	return endpoint, protected, id, nil
}

func (f ContainerFactory) checkpointContainer(stage string, lease *leaseFile) error {
	if f.stage != nil {
		return f.stage(stage, lease.snapshot())
	}
	return nil
}

func (f ContainerFactory) provisionContainer(ctx context.Context, d dockerCLI, endpoint containerEndpoint, protected []databaseEndpoint, lease *leaseFile, budgets factoryBudgets) (*DisposableTarget, error) {
	image, err := d.image(ctx, lease.record.Factory.Image, lease.record.Factory.PostgresMajor)
	if err != nil {
		return nil, err
	}
	password, err := targetPassword()
	if err != nil {
		return nil, err
	}
	resource, err := f.createContainer(ctx, d, endpoint, lease, image, password)
	if err != nil {
		return nil, err
	}
	if _, err := d.run(ctx, nil, "container", "start", resource.ID); err != nil {
		return nil, err
	}
	if err := f.checkpointContainer("container_started", lease); err != nil {
		return nil, err
	}
	authority, err := waitContainerAdmin(ctx, d, endpoint, protected, lease.record, resource, password)
	if err != nil {
		return nil, err
	}
	defer closeAdmin(authority.conn, budgets.cleanup)
	// The same restricted-role/database path serves both factory modes. Its
	// administration credential exists only in this process and the owned daemon.
	provisioner := AdminFactory{Definition: f.Definition, stage: f.stage}
	return provisioner.provision(ctx, authority, lease, budgets)
}

func (f ContainerFactory) createContainer(ctx context.Context, d dockerCLI, endpoint containerEndpoint, lease *leaseFile, image dockerImage, password string) (leaseResource, error) {
	record := lease.snapshot()
	resource := leaseResource{Kind: "container", Name: record.resourceName(), Marker: record.marker(), State: "planned", ImageID: image.ID}
	record.Resources = append(record.Resources, resource)
	if err := lease.save(record); err != nil {
		return leaseResource{}, err
	}
	if err := f.checkpointContainer("container_planned", lease); err != nil {
		return leaseResource{}, err
	}
	raw, err := d.run(ctx, []string{"POSTGRES_PASSWORD=" + password}, containerCreateArgs(record, endpoint.host)...)
	if err != nil {
		return leaseResource{}, err
	}
	resource.ID = strings.TrimSpace(string(raw))
	if !leaseContainerID.MatchString(resource.ID) {
		return leaseResource{}, ErrFactoryProvision
	}
	if err := f.checkpointContainer("container_created", lease); err != nil {
		return leaseResource{}, err
	}
	resource.State = "acquired"
	if err := saveLeaseResource(lease, resource); err != nil {
		return leaseResource{}, err
	}
	if err := f.checkpointContainer("container_recorded", lease); err != nil {
		return leaseResource{}, err
	}
	return resource, nil
}

const containerTmpfsOptions = "rw,nosuid,nodev,size=1073741824"

func containerCreateArgs(lease leaseRecord, host string) []string {
	path := containerDataPath(lease.Factory.PostgresMajor)
	return []string{"container", "create", "--name", lease.resourceName(), "--label", ownershipLabel + "=" + lease.marker(), "--network", "bridge", "--restart", "no", "--tmpfs", path + ":" + containerTmpfsOptions, "--publish", net.JoinHostPort(host, "") + ":5432", "--env", "POSTGRES_USER=postgres", "--env", "POSTGRES_DB=postgres", "--env", "POSTGRES_PASSWORD", "--env", "POSTGRES_HOST_AUTH_METHOD=scram-sha-256", "--env", "PGDATA=" + path + "/autosql", lease.Factory.Image, "postgres", "-c", "password_encryption=scram-sha-256"}
}

func waitContainerAdmin(ctx context.Context, d dockerCLI, endpoint containerEndpoint, protected []databaseEndpoint, lease leaseRecord, resource leaseResource, password string) (adminAuthority, error) {
	for {
		container, exists, err := d.ownedContainer(ctx, lease, resource)
		if err != nil {
			return adminAuthority{}, errors.Join(err, ctx.Err())
		}
		if !exists {
			return adminAuthority{}, ErrOwnership
		}
		if !container.Running {
			return adminAuthority{}, ErrFactoryProvision
		}
		target, err := approvedContainerDatabase(container, endpoint, protected)
		if err != nil {
			return adminAuthority{}, err
		}
		authority, err := connectContainerAdmin(ctx, target, lease.Factory.PostgresMajor, password)
		if err == nil {
			return authority, nil
		}
		if !errors.Is(err, ErrFactoryConnection) {
			return adminAuthority{}, err
		}
		if err := waitContainerRetry(ctx); err != nil {
			return adminAuthority{}, err
		}
	}
}

func connectContainerAdmin(ctx context.Context, target databaseEndpoint, major int, password string) (adminAuthority, error) {
	u := &url.URL{Scheme: "postgres", Host: net.JoinHostPort(target.host, target.port), Path: "/postgres", User: url.UserPassword("postgres", password), RawQuery: "sslmode=disable&connect_timeout=2"}
	return connectApprovedAdmin(ctx, u, target, []databaseEndpoint{target}, nil, major)
}

func waitContainerRetry(ctx context.Context) error {
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

var _ Factory = ContainerFactory{}

func approvedContainerDatabase(container dockerContainer, endpoint containerEndpoint, protected []databaseEndpoint) (databaseEndpoint, error) {
	port, err := container.postgresPort(endpoint.host)
	if err != nil {
		return databaseEndpoint{}, err
	}
	target, err := containerPostgresEndpoint(endpoint, port)
	if err != nil {
		return databaseEndpoint{}, err
	}
	if err := approveFactoryEndpoint(target, []databaseEndpoint{target}, protected); err != nil {
		return databaseEndpoint{}, err
	}
	return target, nil
}
