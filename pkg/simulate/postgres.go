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
	"github.com/jackc/pgx/v5/pgconn"
	"net"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
)

type PostgresFactory struct {
	// NamePrefix scopes generated databases for ownership and cleanup checks.
	// It must remain an unquoted PostgreSQL-safe AutoSQL namespace; 38 bytes
	// leaves room for the separator and 96-bit random suffix within NAMEDATALEN.
	NamePrefix string
	// DropPublicSchema makes the isolated database match an external bootstrap
	// target, whose documented precondition is an empty, public-less database.
	DropPublicSchema bool
	// RequiredRoles are cluster-global owner roles referenced by schema DDL but
	// intentionally not managed by the schema document. Missing roles are
	// leased as NOLOGIN roles for the lifetime of the isolated workspace.
	RequiredRoles []string
	// Render carries provisioning authorizations that the caller already
	// validated into scratch materialization. Controls absent from this map
	// remain disabled by the PostgreSQL renderer.
	Render      map[string]string
	AfterCreate func() error
}

var safeSimulationPrefix = regexp.MustCompile(`^autosql_sim(?:_[a-z0-9]+)*$`)

type PostgresWorkspace struct {
	adminURL, dbURL, name, identity string
	schemas                         []string
	roleLease                       *postgresRoleLease
	render                          map[string]string
}

func (f PostgresFactory) Create(ctx context.Context, c Config) (Isolation, error) {
	return f.CreateWorkspace(ctx, c)
}

// CreateWorkspace creates the same isolated PostgreSQL workspace used by
// simulation while also exposing its URL to trusted replay callers.
func (f PostgresFactory) CreateWorkspace(ctx context.Context, c Config) (*PostgresWorkspace, error) {
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
		return nil, lifecycleFailure("connect", e)
	}
	closeAdmin := true
	defer func() {
		if closeAdmin {
			_ = conn.Close(context.Background())
		}
	}()
	actual, e := runtimeIdentity(ctx, conn)
	if e != nil {
		return nil, lifecycleFailure("runtime_identity", e)
	}
	if actual != c.DevelopmentIdentity || actual == c.ProductionIdentity {
		return nil, fail("runtime_separation", ErrConfig)
	}
	roles, e := acquirePostgresRoleLease(ctx, conn, f.RequiredRoles)
	if e != nil {
		return nil, lifecycleFailure("prepare_roles", e)
	}
	cleanupRoles := true
	defer func() {
		if cleanupRoles {
			_ = roles.Close(context.Background())
		}
	}()
	_, e = conn.Exec(ctx, "CREATE DATABASE "+quote(name))
	if e == nil && f.AfterCreate != nil {
		e = f.AfterCreate()
	}
	copy := *u
	copy.Path = "/" + name
	copy.RawQuery = u.RawQuery
	if e == nil {
		e = preparePostgresDatabase(ctx, copy.String(), f.DropPublicSchema)
	}
	if e != nil {
		timeout := c.CleanupTimeout
		if timeout <= 0 {
			timeout = 15 * time.Second
		}
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		cleanupErr := cleanupDatabase(cleanupCtx, c.DevelopmentURL, name)
		primary := lifecycleFailure("create_database", e)
		if cleanupErr != nil {
			return nil, errors.Join(primary, Redacted(cleanupErr))
		}
		return nil, primary
	}
	cleanupRoles = false
	closeAdmin = roles == nil
	render := make(map[string]string, len(f.Render))
	for key, value := range f.Render {
		render[key] = value
	}
	return &PostgresWorkspace{adminURL: c.DevelopmentURL, dbURL: copy.String(), name: name, identity: actual + "/" + name, roleLease: roles, render: render}, nil
}

func preparePostgresDatabase(ctx context.Context, databaseURL string, dropPublic bool) error {
	if !dropPublic {
		return nil
	}
	conn, err := pgx.Connect(ctx, databaseURL)
	if err != nil {
		return err
	}
	defer conn.Close(context.Background())
	_, err = conn.Exec(ctx, "DROP SCHEMA public")
	return err
}

type leasedPostgresRole struct {
	name    string
	managed bool
	created bool
}

type postgresRoleLease struct {
	conn  *pgx.Conn
	roles []leasedPostgresRole
}

const postgresRoleLockNamespace = "autosql.simulation.role/"
const postgresRoleMarker = "autosql temporary simulation owner role/v1"

func normalizedRequiredRoles(values []string) ([]string, error) {
	seen := map[string]bool{}
	roles := make([]string, 0, len(values))
	for _, value := range values {
		role := strings.TrimSpace(value)
		if role == "" {
			return nil, errors.New("required role is empty")
		}
		if !seen[role] {
			seen[role] = true
			roles = append(roles, role)
		}
	}
	sort.Strings(roles)
	return roles, nil
}

func acquirePostgresRoleLease(ctx context.Context, conn *pgx.Conn, values []string) (*postgresRoleLease, error) {
	roles, err := normalizedRequiredRoles(values)
	if err != nil || len(roles) == 0 {
		return nil, err
	}
	lease := &postgresRoleLease{conn: conn}
	for _, role := range roles {
		lockName := postgresRoleLockNamespace + role
		if _, err = conn.Exec(ctx, `select pg_advisory_lock_shared(hashtextextended($1, 0))`, lockName); err != nil {
			_ = lease.Close(context.Background())
			return nil, err
		}
		lease.roles = append(lease.roles, leasedPostgresRole{name: role})
		roleIndex := len(lease.roles) - 1
		var exists bool
		if err = conn.QueryRow(ctx, `select exists(select 1 from pg_roles where rolname=$1)`, role).Scan(&exists); err != nil {
			_ = lease.Close(context.Background())
			return nil, err
		}
		if exists {
			var marker string
			if err = conn.QueryRow(ctx, `select coalesce((select shobj_description(oid,'pg_authid') from pg_roles where rolname=$1),'')`, role).Scan(&marker); err != nil {
				_ = lease.Close(context.Background())
				return nil, err
			}
			lease.roles[roleIndex].managed = marker == postgresRoleMarker
		}
		if !exists {
			if _, err = conn.Exec(ctx, `select pg_advisory_unlock_shared(hashtextextended($1, 0))`, lockName); err != nil {
				_ = lease.Close(context.Background())
				return nil, err
			}
			if _, err = conn.Exec(ctx, `select pg_advisory_lock(hashtextextended($1, 0))`, lockName); err != nil {
				_ = lease.Close(context.Background())
				return nil, err
			}
			var marker string
			if err = conn.QueryRow(ctx, `select coalesce((select shobj_description(oid,'pg_authid') from pg_roles where rolname=$1),'')`, role).Scan(&marker); err == nil {
				exists = marker != ""
				if !exists {
					err = conn.QueryRow(ctx, `select exists(select 1 from pg_roles where rolname=$1)`, role).Scan(&exists)
				}
			}
			if err == nil && !exists {
				_, err = conn.Exec(ctx, "CREATE ROLE "+quote(role)+" NOLOGIN")
				lease.roles[roleIndex].managed = err == nil
				lease.roles[roleIndex].created = err == nil
				if err == nil {
					_, err = conn.Exec(ctx, "COMMENT ON ROLE "+quote(role)+" IS "+quoteLiteral(postgresRoleMarker))
				}
				if err == nil {
					_, err = conn.Exec(ctx, "GRANT "+quote(role)+" TO CURRENT_USER")
				}
			} else if err == nil {
				lease.roles[roleIndex].managed = marker == postgresRoleMarker
			}
			if err == nil {
				_, err = conn.Exec(ctx, `select pg_advisory_lock_shared(hashtextextended($1, 0))`, lockName)
			}
			_, unlockErr := conn.Exec(context.Background(), `select pg_advisory_unlock(hashtextextended($1, 0))`, lockName)
			if err == nil {
				err = unlockErr
			}
			if err != nil {
				_ = lease.Close(context.Background())
				return nil, err
			}
		}
	}
	return lease, nil
}

func (l *postgresRoleLease) Close(ctx context.Context) error {
	if l == nil || l.conn == nil {
		return nil
	}
	var cleanup error
	for index := len(l.roles) - 1; index >= 0; index-- {
		role := l.roles[index]
		lockName := postgresRoleLockNamespace + role.name
		if role.managed {
			_, unlockErr := l.conn.Exec(ctx, `select pg_advisory_unlock_shared(hashtextextended($1, 0))`, lockName)
			var locked bool
			lockErr := l.conn.QueryRow(ctx, `select pg_try_advisory_lock(hashtextextended($1, 0))`, lockName).Scan(&locked)
			var dropErr error
			if lockErr == nil && locked {
				var marker string
				if dropErr = l.conn.QueryRow(ctx, `select coalesce((select shobj_description(oid,'pg_authid') from pg_roles where rolname=$1),'')`, role.name).Scan(&marker); dropErr == nil && (role.created || marker == postgresRoleMarker) {
					_, dropErr = l.conn.Exec(ctx, "DROP ROLE IF EXISTS "+quote(role.name))
				}
			}
			var exclusiveUnlockErr error
			if locked {
				_, exclusiveUnlockErr = l.conn.Exec(context.Background(), `select pg_advisory_unlock(hashtextextended($1, 0))`, lockName)
			}
			cleanup = errors.Join(cleanup, unlockErr, lockErr, dropErr, exclusiveUnlockErr)
		} else {
			_, err := l.conn.Exec(ctx, `select pg_advisory_unlock_shared(hashtextextended($1, 0))`, lockName)
			cleanup = errors.Join(cleanup, err)
		}
	}
	cleanup = errors.Join(cleanup, l.conn.Close(ctx))
	l.conn = nil
	return cleanup
}

func (p *PostgresWorkspace) URL() string { return p.dbURL }
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
func (p *PostgresWorkspace) Identity() string { return p.identity }
func (p *PostgresWorkspace) Materialize(ctx context.Context, doc schema.Document) error {
	seen := map[string]bool{}
	for _, r := range doc.Graph.Resources {
		if r.Kind == schema.KindSchema && !seen[r.Name.Name] {
			p.schemas = append(p.schemas, r.Name.Name)
			seen[r.Name.Name] = true
		}
	}
	baseline := schema.Document{Version: doc.Version, Graph: schema.Graph{Extra: doc.Graph.Extra}, Annotations: doc.Annotations, Extra: doc.Extra}
	for _, resource := range doc.Graph.Resources {
		if resource.Kind == schema.KindSchema && resource.Name.Name == "public" && resource.Name.Schema == "" {
			baseline.Graph.Resources = append(baseline.Graph.Resources, schema.Resource{ID: resource.ID, Kind: resource.Kind, Name: resource.Name, Spec: []byte(`{}`)})
			break
		}
	}
	pl, e := plan.Build(ctx, postgres.New(), baseline, doc, plan.Options{Render: p.render})
	if e != nil {
		return e
	}
	if e = p.Execute(ctx, pl); e != nil {
		return e
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
func (p *PostgresWorkspace) Execute(ctx context.Context, pl plan.Plan) error {
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
		if phase.Transaction == plan.TransactionProhibited {
			for _, id := range phase.StepIDs {
				for _, s := range pl.Steps {
					if s.ID == id && s.Kind == plan.StepExecutable {
						if _, e = conn.Exec(ctx, s.SQL); e != nil {
							return e
						}
					}
				}
			}
			continue
		}
		tx, beginErr := conn.Begin(ctx)
		if beginErr != nil {
			return beginErr
		}
		for _, id := range phase.StepIDs {
			for _, s := range pl.Steps {
				if s.ID == id && s.Kind == plan.StepExecutable {
					if _, e = tx.Exec(ctx, s.SQL); e != nil {
						_ = tx.Rollback(ctx)
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
func (p *PostgresWorkspace) Inspect(ctx context.Context) (schema.Document, error) {
	doc, e := postgres.InspectURL(ctx, p.dbURL, postgres.Options{Schemas: p.schemas})
	if e != nil {
		return schema.Document{}, e
	}
	return postgres.New().Normalize(ctx, doc)
}
func (p *PostgresWorkspace) Cleanup(ctx context.Context) error {
	databaseErr := cleanupDatabase(ctx, p.adminURL, p.name)
	roleCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
	defer cancel()
	roleErr := p.roleLease.Close(roleCtx)
	p.roleLease = nil
	return errors.Join(databaseErr, roleErr)
}
func cleanupDatabase(ctx context.Context, adminURL, name string) error {
	cycle := func(attemptCtx context.Context) (bool, error) {
		conn, err := pgx.Connect(attemptCtx, adminURL)
		if err != nil {
			return false, err
		}
		closed := false
		defer func() {
			if !closed {
				_ = conn.Close(attemptCtx)
			}
		}()
		absent, attemptErr := cleanupDatabaseAttempt(attemptCtx,
			func(ctx context.Context) error {
				_, execErr := conn.Exec(ctx, "SELECT pg_terminate_backend(pid) FROM pg_stat_activity WHERE datname=$1 AND pid<>pg_backend_pid()", name)
				return execErr
			},
			func(ctx context.Context) error {
				_, execErr := conn.Exec(ctx, "DROP DATABASE IF EXISTS "+quote(name))
				return execErr
			},
			func(ctx context.Context) (bool, error) {
				var exists bool
				scanErr := conn.QueryRow(ctx, `select exists(select 1 from pg_database where datname=$1)`, name).Scan(&exists)
				return !exists, scanErr
			})
		closeErr := conn.Close(attemptCtx)
		closed = true
		if closeErr != nil {
			attemptErr = errors.Join(attemptErr, closeErr)
		}
		if attemptErr != nil {
			return false, attemptErr
		}
		return absent, nil
	}
	if err := cleanupDatabaseCycles(ctx, cycle, sleepCleanupRetry); err != nil {
		return errors.Join(fail("cleanup_database", ErrLifecycle), err)
	}
	return nil
}

func cleanupDatabaseAttempt(ctx context.Context, terminate func(context.Context) error, drop func(context.Context) error, absent func(context.Context) (bool, error)) (bool, error) {
	if err := terminate(ctx); err != nil {
		return false, err
	}
	if err := drop(ctx); err != nil {
		return false, err
	}
	return absent(ctx)
}

type databaseStillPresentError struct{ attempts int }

func (e *databaseStillPresentError) Error() string {
	return fmt.Sprintf("temporary database still present after cleanup attempt %d", e.attempts)
}

func transientCleanup(err error) bool {
	var present *databaseStillPresentError
	if errors.As(err, &present) {
		return true
	}
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) {
		return false
	}
	switch pgErr.Code {
	case "55006", "57P03", "53300", "55P03", "40001", "40P01":
		return true
	default:
		return false
	}
}

func sleepCleanupRetry(ctx context.Context, delay time.Duration) error {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func cleanupDatabaseCycles(ctx context.Context, cycle func(context.Context) (bool, error), sleep func(context.Context, time.Duration) error) error {
	const maxAttempts = 12
	var last error
	for attempt := 1; attempt <= maxAttempts; attempt++ {
		absent, err := cycle(ctx)
		if err == nil && absent {
			return nil
		}
		if err == nil {
			err = &databaseStillPresentError{attempts: attempt}
		}
		last = err
		if !transientCleanup(err) {
			return err
		}
		if attempt == maxAttempts {
			break
		}
		delay := min(5*time.Millisecond*time.Duration(1<<min(attempt-1, 6)), 250*time.Millisecond)
		if err = sleep(ctx, delay); err != nil {
			return errors.Join(last, err)
		}
	}
	return errors.Join(last, &databaseStillPresentError{attempts: maxAttempts})
}
func quote(s string) string        { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }
func quoteLiteral(s string) string { return `'` + strings.ReplaceAll(s, `'`, `''`) + `'` }
