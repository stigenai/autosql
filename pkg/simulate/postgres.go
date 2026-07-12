package simulate

import (
	"autosql/pkg/plan"
	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"github.com/jackc/pgx/v5"
	"net"
	"net/url"
	"regexp"
	"strings"
	"time"
)

type PostgresFactory struct {
	// NamePrefix scopes generated databases for ownership and cleanup checks.
	// It must remain an unquoted PostgreSQL-safe AutoSQL namespace; 38 bytes
	// leaves room for the separator and 96-bit random suffix within NAMEDATALEN.
	NamePrefix  string
	AfterCreate func() error
}

var safeSimulationPrefix = regexp.MustCompile(`^autosql_sim(?:_[a-z0-9]+)*$`)

type postgresIsolation struct {
	adminURL, dbURL, name, identity string
	schemas                         []string
}

func (f PostgresFactory) Create(ctx context.Context, c Config) (Isolation, error) {
	prefix := f.NamePrefix
	if prefix == "" {
		prefix = "autosql_sim"
	}
	if len(prefix) > 38 || !safeSimulationPrefix.MatchString(prefix) {
		return nil, fail("database_name_prefix", ErrConfig)
	}
	u, e := url.Parse(c.DevelopmentURL)
	hasPassword := false
	if u != nil && u.User != nil {
		_, hasPassword = u.User.Password()
	}
	if e != nil || u == nil || u.Scheme != "postgres" && u.Scheme != "postgresql" || u.User == nil || u.User.Username() == "" || !hasPassword {
		return nil, fail("development_url", ErrConfig)
	}
	host := u.Hostname()
	allowed := host == "localhost" || net.ParseIP(host) != nil && net.ParseIP(host).IsLoopback()
	for _, h := range c.AllowedHosts {
		allowed = allowed || strings.EqualFold(host, h)
	}
	if !allowed {
		return nil, fail("development_endpoint", ErrConfig)
	}
	port := u.Port()
	if port == "" {
		port = "5432"
	}
	db := strings.TrimPrefix(u.Path, "/")
	identity := net.JoinHostPort(host, port) + "/" + db
	if c.DevelopmentIdentity == "" {
		return nil, fail("development_identity", ErrConfig)
	}
	if identity == c.ProductionIdentity || sameResolvedEndpoint(u, c.ProductionIdentity) {
		return nil, fail("credential_separation", ErrConfig)
	}
	random := make([]byte, 12)
	if _, e = rand.Read(random); e != nil {
		return nil, fail("random", ErrLifecycle)
	}
	name := prefix + "_" + hex.EncodeToString(random)
	conn, e := pgx.Connect(ctx, c.DevelopmentURL)
	if e != nil {
		return nil, fail("connect", ErrLifecycle)
	}
	defer conn.Close(context.Background())
	actual, e := runtimeIdentity(ctx, conn)
	if e != nil {
		return nil, fail("runtime_identity", ErrLifecycle)
	}
	if actual != c.DevelopmentIdentity || actual == c.ProductionIdentity {
		return nil, fail("runtime_separation", ErrConfig)
	}
	_, e = conn.Exec(ctx, "CREATE DATABASE "+quote(name))
	if e == nil && f.AfterCreate != nil {
		e = f.AfterCreate()
	}
	if e != nil {
		timeout := c.CleanupTimeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		cleanupErr := cleanupDatabase(cleanupCtx, c.DevelopmentURL, name)
		primary := fail("create_database", ErrLifecycle)
		if cleanupErr != nil {
			return nil, errors.Join(primary, cleanupErr)
		}
		return nil, primary
	}
	copy := *u
	copy.Path = "/" + name
	copy.RawQuery = u.RawQuery
	return &postgresIsolation{adminURL: c.DevelopmentURL, dbURL: copy.String(), name: name, identity: actual + "/" + name}, nil
}
func ResolvePostgresIdentity(ctx context.Context, developmentURL string) (string, error) {
	conn, e := pgx.Connect(ctx, developmentURL)
	if e != nil {
		return "", fail("identity_connect", ErrLifecycle)
	}
	defer conn.Close(context.Background())
	id, e := runtimeIdentity(ctx, conn)
	if e != nil {
		return "", fail("identity_query", ErrLifecycle)
	}
	return id, nil
}
func runtimeIdentity(ctx context.Context, conn *pgx.Conn) (string, error) {
	var address, database, system string
	var port int
	if e := conn.QueryRow(ctx, `select coalesce(inet_server_addr()::text,''),inet_server_port(),current_database(),system_identifier::text from pg_control_system()`).Scan(&address, &port, &database, &system); e != nil {
		return "", e
	}
	return net.JoinHostPort(address, fmt.Sprint(port)) + "/" + database + "#" + system, nil
}
func sameResolvedEndpoint(dev *url.URL, production string) bool {
	p, e := url.Parse(production)
	if e != nil || p.Scheme == "" {
		p, e = url.Parse("postgres://" + production)
	}
	if e != nil {
		return false
	}
	port := func(u *url.URL) string {
		if u.Port() != "" {
			return u.Port()
		}
		return "5432"
	}
	if port(dev) != port(p) || strings.TrimPrefix(dev.Path, "/") != strings.TrimPrefix(p.Path, "/") {
		return false
	}
	devIPs, _ := net.LookupIP(dev.Hostname())
	prodIPs, _ := net.LookupIP(p.Hostname())
	for _, a := range devIPs {
		for _, b := range prodIPs {
			if a.Equal(b) {
				return true
			}
		}
	}
	return false
}
func (p *postgresIsolation) Identity() string { return p.identity }
func (p *postgresIsolation) Materialize(ctx context.Context, doc schema.Document) error {
	statements, e := postgres.RenderDocument(ctx, doc, nil)
	if e != nil {
		return e
	}
	conn, e := pgx.Connect(ctx, p.dbURL)
	if e != nil {
		return e
	}
	defer conn.Close(context.Background())
	tx, e := conn.Begin(ctx)
	if e != nil {
		return e
	}
	for _, s := range statements {
		if _, e = tx.Exec(ctx, s.SQL); e != nil {
			tx.Rollback(ctx)
			return e
		}
	}
	if e = tx.Commit(ctx); e != nil {
		return e
	}
	seen := map[string]bool{}
	for _, r := range doc.Graph.Resources {
		if r.Kind == schema.KindSchema && !seen[r.Name.Name] {
			p.schemas = append(p.schemas, r.Name.Name)
			seen[r.Name.Name] = true
		}
	}
	if len(p.schemas) == 0 {
		return nil
	}
	actual, e := p.Inspect(ctx)
	if e != nil {
		return e
	}
	a, _ := schema.SemanticFingerprint(actual)
	want, _ := schema.SemanticFingerprint(doc)
	if a != want {
		return ErrFingerprint
	}
	return nil
}
func (p *postgresIsolation) Execute(ctx context.Context, pl plan.Plan) error {
	seen := map[string]bool{}
	for _, s := range p.schemas {
		seen[s] = true
	}
	for _, c := range pl.Changes.Changes {
		if c.After != nil && c.After.Kind == schema.KindSchema && !seen[c.After.Name.Name] {
			p.schemas = append(p.schemas, c.After.Name.Name)
			seen[c.After.Name.Name] = true
		}
	}
	conn, e := pgx.Connect(ctx, p.dbURL)
	if e != nil {
		return e
	}
	defer conn.Close(context.Background())
	for _, phase := range pl.Phases {
		if phase.Transaction != plan.TransactionRequired {
			return ErrConfig
		}
		tx, e := conn.Begin(ctx)
		if e != nil {
			return e
		}
		for _, id := range phase.StepIDs {
			for _, s := range pl.Steps {
				if s.ID == id && s.Kind == plan.StepExecutable {
					if _, e = tx.Exec(ctx, s.SQL); e != nil {
						tx.Rollback(ctx)
						return e
					}
				}
			}
		}
		if e = tx.Commit(ctx); e != nil {
			return e
		}
	}
	return nil
}
func (p *postgresIsolation) Inspect(ctx context.Context) (schema.Document, error) {
	doc, e := postgres.InspectURL(ctx, p.dbURL, postgres.Options{Schemas: p.schemas})
	if e != nil {
		return schema.Document{}, e
	}
	return postgres.New().Normalize(ctx, doc)
}
func (p *postgresIsolation) Cleanup(ctx context.Context) error {
	return cleanupDatabase(ctx, p.adminURL, p.name)
}
func cleanupDatabase(ctx context.Context, adminURL, name string) error {
	conn, e := pgx.Connect(ctx, adminURL)
	if e != nil {
		return fail("cleanup_connect", ErrLifecycle)
	}
	defer conn.Close(context.Background())
	_, _ = conn.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()", name)
	_, e = conn.Exec(ctx, "DROP DATABASE IF EXISTS "+quote(name))
	if e != nil {
		return fail("cleanup_database", ErrLifecycle)
	}
	return nil
}
func quote(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
