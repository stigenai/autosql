package dbtest

import (
	"context"
	"net"
	"net/netip"
	"net/url"
	"path/filepath"
	"slices"
	"strconv"
	"strings"

	"autosql/pkg/config"
)

// ContainerFactory creates a fresh PostgreSQL container and restricted target
// on an explicitly allowlisted Docker-compatible daemon. Images are selected by
// immutable digest; writable database state is never reused.
type ContainerFactory struct {
	Name           string
	Classification string
	Definition     config.DevelopmentFactory
	LeaseDirectory string
	command        dockerCommand
	stage          func(string, leaseRecord) error
}

type containerEndpoint struct{ daemon, host string }

func (f ContainerFactory) validate() (factoryBudgets, error) {
	d := f.Definition
	if f.Name == "" || f.Classification != "development" || d.Mode != "container" || !leaseImage.MatchString(d.Image) {
		return factoryBudgets{}, ErrFactoryConfig
	}
	if err := f.validateContainerAuthority(); err != nil {
		return factoryBudgets{}, err
	}
	return parseFactoryBudgets(d)
}

func (f ContainerFactory) validateContainerAuthority() error {
	d := f.Definition
	if d.Admin != "" || d.AdminClassification != "" || d.PostgresVersion < 14 || d.PostgresVersion > 18 {
		return ErrFactoryConfig
	}
	if !leaseDaemonEndpoint(d.Endpoint) || len(d.Allowlist) == 0 || d.Isolation != "" && d.Isolation != "database" {
		return ErrFactoryConfig
	}
	return nil
}

func canonicalContainerEndpoint(ctx context.Context, value string) (containerEndpoint, error) {
	if !leaseDaemonEndpoint(value) {
		return containerEndpoint{}, ErrFactoryConfig
	}
	u, _ := url.Parse(value)
	if u.Scheme == "unix" {
		path := filepath.Clean(u.Path)
		if resolved, err := filepath.EvalSymlinks(path); err == nil {
			path = resolved
		}
		return containerEndpoint{daemon: (&url.URL{Scheme: "unix", Path: path}).String(), host: "127.0.0.1"}, nil
	}
	port, err := factoryPort(u.Port())
	if err != nil {
		return containerEndpoint{}, err
	}
	endpoint, err := resolveFactoryEndpoint(ctx, databaseEndpoint{host: strings.ToLower(u.Hostname()), port: port})
	if err != nil {
		return containerEndpoint{}, err
	}
	slices.SortFunc(endpoint.ips, func(a, b netip.Addr) int { return a.Compare(b) })
	host := endpoint.ips[0].String()
	return containerEndpoint{daemon: "tcp://" + net.JoinHostPort(host, port), host: host}, nil
}

func (f ContainerFactory) approvedEndpoint(ctx context.Context) (containerEndpoint, error) {
	endpoint, err := canonicalContainerEndpoint(ctx, f.Definition.Endpoint)
	if err != nil {
		return containerEndpoint{}, err
	}
	matched := false
	for _, value := range f.Definition.Allowlist {
		allowed, err := canonicalContainerEndpoint(ctx, value)
		if err != nil {
			return containerEndpoint{}, err
		}
		if allowed == endpoint {
			matched = true
		}
	}
	if matched {
		return endpoint, nil
	}
	return containerEndpoint{}, ErrFactoryConfig
}

func containerPostgresEndpoint(endpoint containerEndpoint, port string) (databaseEndpoint, error) {
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return databaseEndpoint{}, ErrFactoryConnection
	}
	ip, err := netip.ParseAddr(endpoint.host)
	if err != nil {
		return databaseEndpoint{}, ErrFactoryConfig
	}
	return databaseEndpoint{host: endpoint.host, port: port, database: "postgres", ips: []netip.Addr{ip.Unmap()}}, nil
}
