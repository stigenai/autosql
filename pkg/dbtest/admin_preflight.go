package dbtest

import (
	"context"
	"errors"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"autosql/pkg/config"
	"autosql/pkg/secret"
	"autosql/pkg/simulate"

	"github.com/jackc/pgx/v5"
)

var (
	ErrFactorySecret     = errors.New("development administration secret unavailable")
	ErrFactoryConfig     = errors.New("development factory configuration refused")
	ErrFactoryConnection = errors.New("development factory connection failed")
	ErrFactoryProvision  = errors.New("development factory provisioning failed")
)

// AdminFactory provisions fresh databases on an explicitly approved existing
// development server. Only its restricted target implements fixture execution;
// the administrator connection is private to provisioning and cleanup.
type AdminFactory struct {
	Name                string
	Classification      string
	Definition          config.DevelopmentFactory
	ProtectedReferences []secret.Reference
	LeaseDirectory      string
	Resolver            *secret.Resolver
	stage               func(string, leaseRecord) error
}

type factoryBudgets struct{ startup, execution, cleanup time.Duration }

type adminAuthority struct {
	conn     *pgx.Conn
	url      *url.URL
	identity string
	owner    uint32
	major    int
}

func (f AdminFactory) validate() (factoryBudgets, error) {
	d := f.Definition
	if f.Name == "" || f.Classification != "development" || d.Mode != "existing_admin" || d.AdminClassification != "development" {
		return factoryBudgets{}, ErrFactoryConfig
	}
	if err := f.validateAuthority(); err != nil {
		return factoryBudgets{}, err
	}
	return parseFactoryBudgets(d)
}

func (f AdminFactory) validateAuthority() error {
	d := f.Definition
	if d.Admin.Validate() != nil || slices.Contains(f.ProtectedReferences, d.Admin) || len(d.Allowlist) == 0 || d.PostgresVersion < 14 || d.PostgresVersion > 18 {
		return ErrFactoryConfig
	}
	if d.Isolation != "" && d.Isolation != "database" || d.Endpoint != "" || d.Image != "" {
		return ErrFactoryConfig
	}
	return nil
}

func parseFactoryBudgets(d config.DevelopmentFactory) (factoryBudgets, error) {
	values := []time.Duration{2 * time.Minute, 5 * time.Minute, 10 * time.Second}
	for i, text := range []string{d.StartupTimeout, d.CaseTimeout, d.CleanupTimeout} {
		if text == "" {
			continue
		}
		value, err := time.ParseDuration(text)
		if err != nil || value <= 0 {
			return factoryBudgets{}, ErrFactoryConfig
		}
		values[i] = value
	}
	return factoryBudgets{values[0], values[1], values[2]}, nil
}

type databaseEndpoint struct {
	host, port, database string
	ips                  []netip.Addr
}

func parseFactoryURL(raw string, credential bool) (*url.URL, databaseEndpoint, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "postgres" && u.Scheme != "postgresql" || u.Hostname() == "" || u.Fragment != "" {
		return nil, databaseEndpoint{}, ErrFactoryConfig
	}
	db := strings.TrimPrefix(u.Path, "/")
	if db == "" || strings.ContainsAny(db, "/\\\x00") || !safeFactoryURLCredentials(u, credential) {
		return nil, databaseEndpoint{}, ErrFactoryConfig
	}
	port, err := factoryPort(u.Port())
	if err != nil {
		return nil, databaseEndpoint{}, err
	}
	return u, databaseEndpoint{host: strings.ToLower(u.Hostname()), port: port, database: db}, nil
}

func factoryPort(port string) (string, error) {
	if port == "" {
		port = "5432"
	}
	n, err := strconv.Atoi(port)
	if err != nil || n < 1 || n > 65535 {
		return "", ErrFactoryConfig
	}
	return strconv.Itoa(n), nil
}

func safeFactoryURLCredentials(u *url.URL, credential bool) bool {
	if !credential {
		return u.User == nil && u.RawQuery == ""
	}
	if u.User == nil || u.User.Username() == "" {
		return false
	}
	values, err := url.ParseQuery(u.RawQuery)
	if err != nil {
		return false
	}
	allowed := map[string]bool{"sslmode": true, "sslrootcert": true, "sslcert": true, "sslkey": true, "connect_timeout": true, "application_name": true}
	for key, values := range values {
		if !allowed[key] || len(values) != 1 {
			return false
		}
	}
	return true
}

func resolveFactoryEndpoint(ctx context.Context, endpoint databaseEndpoint) (databaseEndpoint, error) {
	if ip, err := netip.ParseAddr(endpoint.host); err == nil {
		endpoint.ips = []netip.Addr{ip.Unmap()}
		return endpoint, nil
	}
	ips, err := net.DefaultResolver.LookupNetIP(ctx, "ip", endpoint.host)
	if err != nil || len(ips) == 0 {
		return databaseEndpoint{}, ErrFactoryConfig
	}
	for _, ip := range ips {
		endpoint.ips = append(endpoint.ips, ip.Unmap())
	}
	return endpoint, nil
}

func factoryEndpointList(ctx context.Context, values []string) ([]databaseEndpoint, error) {
	var result []databaseEndpoint
	for _, value := range values {
		_, endpoint, err := parseFactoryURL(value, false)
		if err != nil {
			return nil, err
		}
		endpoint, err = resolveFactoryEndpoint(ctx, endpoint)
		if err != nil {
			return nil, err
		}
		result = append(result, endpoint)
	}
	return result, nil
}

func endpointsOverlap(a, b databaseEndpoint) bool {
	if a.port != b.port {
		return false
	}
	for _, ip := range a.ips {
		if slices.Contains(b.ips, ip) {
			return true
		}
	}
	return false
}

func approveFactoryEndpoint(endpoint databaseEndpoint, allowed, protected []databaseEndpoint) error {
	for _, other := range protected {
		// Protect the whole configured server endpoint, even if its production
		// database differs from the selected administration database.
		if endpointsOverlap(endpoint, other) {
			return ErrFactoryConfig
		}
	}
	for _, other := range allowed {
		if endpoint.database == other.database && endpointsOverlap(endpoint, other) {
			return nil
		}
	}
	return ErrFactoryConfig
}

func (f AdminFactory) connect(ctx context.Context) (adminAuthority, error) {
	resolver := f.Resolver
	if resolver == nil {
		resolver = secret.NewResolver()
	}
	raw, err := resolver.Resolve(ctx, f.Definition.Admin)
	if err != nil {
		return adminAuthority{}, ErrFactorySecret
	}
	u, endpoint, err := parseFactoryURL(raw, true)
	if err != nil {
		return adminAuthority{}, err
	}
	endpoint, err = resolveFactoryEndpoint(ctx, endpoint)
	if err != nil {
		return adminAuthority{}, err
	}
	allowed, err := factoryEndpointList(ctx, f.Definition.Allowlist)
	if err != nil {
		return adminAuthority{}, err
	}
	protected, err := factoryEndpointList(ctx, f.Definition.ProtectedTargets)
	if err != nil {
		return adminAuthority{}, err
	}
	if err := approveFactoryEndpoint(endpoint, allowed, protected); err != nil {
		return adminAuthority{}, err
	}
	return connectApprovedAdmin(ctx, u, endpoint, allowed, protected, f.Definition.PostgresVersion)
}

func connectApprovedAdmin(ctx context.Context, u *url.URL, endpoint databaseEndpoint, allowed, protected []databaseEndpoint, major int) (adminAuthority, error) {
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		return adminAuthority{}, ErrFactoryConnection
	}
	authority := adminAuthority{conn: conn, url: u}
	if err := verifyAdminPeer(conn, endpoint, allowed, protected); err != nil {
		closeAdmin(conn, 10*time.Second)
		return adminAuthority{}, err
	}
	if err := authority.inspect(ctx, endpoint.database, major); err != nil {
		closeAdmin(conn, 10*time.Second)
		return adminAuthority{}, err
	}
	return authority, nil
}

func verifyAdminPeer(conn *pgx.Conn, endpoint databaseEndpoint, allowed, protected []databaseEndpoint) error {
	peer, ok := conn.PgConn().Conn().RemoteAddr().(*net.TCPAddr)
	if !ok {
		return ErrFactoryConfig
	}
	ip, ok := netip.AddrFromSlice(peer.IP)
	if !ok || !slices.Contains(endpoint.ips, ip.Unmap()) {
		return ErrFactoryConfig
	}
	endpoint.ips, endpoint.port = []netip.Addr{ip.Unmap()}, strconv.Itoa(peer.Port)
	return approveFactoryEndpoint(endpoint, allowed, protected)
}

func (a *adminAuthority) inspect(ctx context.Context, expectedDatabase string, expectedMajor int) error {
	var database string
	var version int
	var permitted bool
	err := a.conn.QueryRow(ctx, `SELECT current_database(), current_setting('server_version_num')::integer,
 oid, rolsuper OR (rolcreatedb AND rolcreaterole) FROM pg_catalog.pg_roles WHERE rolname=current_user`).Scan(&database, &version, &a.owner, &permitted)
	if err != nil || !permitted || database != expectedDatabase || version/10000 != expectedMajor {
		return ErrFactoryConfig
	}
	a.identity, err = simulate.ResolvePostgresConnectionIdentity(ctx, a.conn)
	if err != nil {
		return ErrFactoryConfig
	}
	a.major = version / 10000
	if !leaseServerIdentity.MatchString(a.identity) {
		return ErrFactoryConfig
	}
	return nil
}

// FactoryError contains bounded categories and a non-secret recovery path.
// Backend diagnostics and credential values are never included in Error().
type FactoryError struct {
	Stage     string
	LeasePath string
	cause     error
}

func (e *FactoryError) Error() string {
	if e.Stage == "container_daemon" {
		return "development container runtime unavailable; check the Docker CLI and configured daemon"
	}
	if e.LeasePath == "" {
		return "development factory refused at " + e.Stage
	}
	return fmt.Sprintf("development factory failed at %s; inspect lease %s", e.Stage, e.LeasePath)
}
func (e *FactoryError) Unwrap() error { return e.cause }
