package dbtest

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"autosql/pkg/secret"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

func liveAdminFactory(t *testing.T) (AdminFactory, *pgx.Conn) {
	t.Helper()
	base := os.Getenv("AUTOSQL_DBTEST_ADMIN_DSN")
	if base == "" {
		t.Skip("AUTOSQL_DBTEST_ADMIN_DSN unset; requires a disposable PostgreSQL administrator URL")
	}
	ctx := context.Background()
	baseConn, err := pgx.Connect(ctx, base)
	if err != nil {
		t.Fatal("connect disposable database test server")
	}
	name := fmt.Sprintf("autosql_dev_control_%x", time.Now().UnixNano())
	if _, err := baseConn.Exec(ctx, "CREATE DATABASE "+pgx.Identifier{name}.Sanitize()+" TEMPLATE template0"); err != nil {
		baseConn.Close(ctx)
		t.Fatal(err)
	}
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal("parse disposable test URL")
	}
	u.Path = "/" + name
	u.RawPath = ""
	conn, err := pgx.Connect(ctx, u.String())
	if err != nil {
		t.Fatal("connect disposable administration database")
	}
	t.Cleanup(func() {
		closeAdmin(conn, 10*time.Second)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if _, err := baseConn.Exec(ctx, "DROP DATABASE "+pgx.Identifier{name}.Sanitize()+" WITH (FORCE)"); err != nil {
			t.Errorf("remove test administration database: %v", err)
		}
		baseConn.Close(ctx)
	})
	root := t.TempDir()
	if err := os.Chmod(root, 0700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_OWNED_ADMIN", u.String())
	factory := testAdminFactory()
	factory.Definition.Admin = "env://AUTOSQL_OWNED_ADMIN"
	identity := *u
	identity.User = nil
	identity.RawQuery = ""
	factory.Definition.Allowlist = []string{identity.String()}
	factory.LeaseDirectory = root
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatal(err)
	}
	factory.Definition.PostgresVersion = version / 10000
	return factory, conn
}

func requireLeaseResourcesAbsent(t *testing.T, conn *pgx.Conn, record leaseRecord) {
	t.Helper()
	for _, resource := range record.Resources {
		oid, _, _, err := readLiveResource(context.Background(), conn, resource.Kind, resource.Name)
		if err != nil || oid != 0 {
			t.Fatalf("owned %s remains after cleanup: oid=%d err=%v", resource.Kind, oid, err)
		}
	}
}

func TestAdminFactoryLiveRestrictedExecutionAndCleanup(t *testing.T) {
	factory, admin := liveAdminFactory(t)
	target, err := factory.Create(context.Background(), "schema case")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := target.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	record := target.lease.snapshot()
	if err := target.Exec(context.Background(), "CREATE TABLE widgets(id bigint); INSERT INTO widgets VALUES (1)"); err != nil {
		t.Fatal(err)
	}
	if count, err := target.QueryCount(context.Background(), "SELECT count(*) FROM widgets"); err != nil || count != 1 {
		t.Fatalf("fixture query: %d %v", count, err)
	}
	if err := target.Exec(context.Background(), "CREATE ROLE forbidden_privilege_escalation SUPERUSER"); err == nil {
		t.Fatal("fixture created a role")
	}
	if err := target.Exec(context.Background(), "SET ROLE postgres"); err == nil {
		t.Fatal("fixture became administrator")
	}
	var errorState interface{ SQLState() string }
	err = target.Exec(context.Background(), "CREATE DATABASE forbidden_case_database")
	if !errors.As(err, &errorState) || errorState.SQLState() != "42501" {
		t.Fatalf("fixture retained database administration: %v", err)
	}
	value, err := secret.NewResolver().Resolve(context.Background(), target.CredentialReference())
	if err != nil {
		t.Fatal(err)
	}
	u, endpoint, err := parseFactoryURL(value, true)
	if err != nil || u.User.Username() != record.resourceName() || endpoint.database != record.resourceName() {
		t.Fatal("runtime credential did not identify restricted target")
	}
	credentialPath := filepath.Join(filepath.Dir(target.LeasePath()), credentialFilename)
	stat, err := os.Stat(credentialPath)
	if err != nil || stat.Mode().Perm() != 0600 {
		t.Fatal("runtime credential was not private")
	}
	controlURL, _ := url.Parse(os.Getenv("AUTOSQL_OWNED_ADMIN"))
	u.Path = controlURL.Path
	other, err := pgx.Connect(context.Background(), u.String())
	if err != nil {
		t.Fatal("connect to test administration database with restricted role")
	}
	_, err = other.Exec(context.Background(), "SELECT * FROM autosql_development_leases.resources")
	other.Close(context.Background())
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatal("fixture could read administrator ownership registry")
	}
	if err := target.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	requireLeaseResourcesAbsent(t, admin, record)
	if _, err := os.Stat(credentialPath); !os.IsNotExist(err) {
		t.Fatal("cleanup retained runtime credential")
	}
	if err := CleanupAdminLease(context.Background(), target.LeasePath(), nil); err != nil {
		t.Fatalf("idempotent verified cleanup: %v", err)
	}
}

func TestAdminFactoryLivePartialSetupAndCancellationCleanup(t *testing.T) {
	factory, admin := liveAdminFactory(t)
	for _, stage := range []string{"role_planned", "role_created", "role_recorded", "database_planned", "database_created", "database_recorded", "ready"} {
		t.Run(stage, func(t *testing.T) {
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var captured leaseRecord
			f := factory
			f.stage = func(actual string, record leaseRecord) error {
				if actual == stage {
					captured = record
					cancel()
					return ctx.Err()
				}
				return nil
			}
			target, err := f.Create(ctx, "interrupted case")
			if target != nil || !errors.Is(err, context.Canceled) {
				t.Fatalf("unexpected setup result: target=%v error=%v", target != nil, err)
			}
			requireLeaseResourcesAbsent(t, admin, captured)
			var failure *FactoryError
			if !errors.As(err, &failure) || failure.LeasePath == "" {
				t.Fatal("interruption omitted recovery lease")
			}
			lease, err := openLeaseFile(failure.LeasePath)
			if err != nil {
				t.Fatal(err)
			}
			defer lease.close()
			if lease.record.State != "cleaned" {
				t.Fatalf("partial setup left residual state: %s", lease.record.State)
			}
		})
	}
}

func TestAdminFactoryLiveForeignOwnershipRefusesDeletion(t *testing.T) {
	factory, admin := liveAdminFactory(t)
	target, err := factory.Create(context.Background(), "ownership case")
	if err != nil {
		t.Fatal(err)
	}
	record := target.lease.snapshot()
	quoted := pgx.Identifier{record.resourceName()}.Sanitize()
	if _, err := admin.Exec(context.Background(), "COMMENT ON DATABASE "+quoted+" IS 'foreign-owner'"); err != nil {
		t.Fatal(err)
	}
	if err := target.Close(context.Background()); err == nil {
		t.Fatal("cleanup ignored changed live ownership")
	}
	oid, _, marker, err := readLiveResource(context.Background(), admin, "database", record.resourceName())
	if err != nil || oid == 0 || marker != "foreign-owner" {
		t.Fatal("cleanup deleted or changed foreign-owned database")
	}
	if _, err := admin.Exec(context.Background(), "COMMENT ON DATABASE "+quoted+" IS '"+record.marker()+"'"); err != nil {
		t.Fatal(err)
	}
	if err := CleanupAdminLease(context.Background(), target.LeasePath(), nil); err != nil {
		t.Fatal(err)
	}
	requireLeaseResourcesAbsent(t, admin, record)
}

func TestAdminFactoryLiveTenConcurrentSameNamedCases(t *testing.T) {
	factory, admin := liveAdminFactory(t)
	type answer struct {
		target *DisposableTarget
		err    error
	}
	answers := make(chan answer, 10)
	for i := 0; i < 10; i++ {
		go func() {
			target, err := factory.Create(context.Background(), "same name")
			answers <- answer{target, err}
		}()
	}
	seen := map[string]bool{}
	for i := 0; i < 10; i++ {
		result := <-answers
		if result.err != nil {
			t.Error(result.err)
			continue
		}
		record := result.target.lease.snapshot()
		if seen[record.resourceName()] {
			t.Error("parallel cases reused a database")
		}
		seen[record.resourceName()] = true
		if err := result.target.Exec(context.Background(), "CREATE TABLE isolated(id integer); INSERT INTO isolated VALUES (1)"); err != nil {
			t.Error(err)
		}
		if count, err := result.target.QueryCount(context.Background(), "SELECT count(*) FROM isolated"); err != nil || count != 1 {
			t.Errorf("parallel case isolation: %d %v", count, err)
		}
		if err := result.target.Close(context.Background()); err != nil {
			t.Error(err)
		}
		requireLeaseResourcesAbsent(t, admin, record)
	}
	if len(seen) != 10 {
		t.Fatalf("created %d of 10 independent cases", len(seen))
	}
}

func TestAdminFactoryProcessHelper(t *testing.T) {
	if os.Getenv("AUTOSQL_DBTEST_HELPER") != "1" {
		t.Skip("subprocess helper")
	}
	f := testAdminFactory()
	f.Definition.Admin = "env://AUTOSQL_OWNED_ADMIN"
	f.LeaseDirectory = os.Getenv("AUTOSQL_DBTEST_HELPER_ROOT")
	f.Definition.PostgresVersion, _ = strconv.Atoi(os.Getenv("AUTOSQL_DBTEST_HELPER_MAJOR"))
	u, _ := url.Parse(os.Getenv("AUTOSQL_OWNED_ADMIN"))
	u.User = nil
	u.RawQuery = ""
	f.Definition.Allowlist = []string{u.String()}
	f.stage = func(stage string, record leaseRecord) error {
		if stage == "database_recorded" {
			fmt.Fprintln(os.Stdout, "LEASE="+filepath.Join(f.LeaseDirectory, record.RunID+"-"+record.CaseID, leaseFilename))
			<-time.NewTimer(time.Hour).C
		}
		return nil
	}
	if _, err := f.Create(context.Background(), "crash case"); err != nil {
		t.Fatal(err)
	}
}

func TestAdminFactoryLiveForceKilledProcessRecoversRecordedLease(t *testing.T) {
	factory, admin := liveAdminFactory(t)
	command := exec.Command(os.Args[0], "-test.run=^TestAdminFactoryProcessHelper$")
	command.Env = append(os.Environ(), "AUTOSQL_DBTEST_HELPER=1", "AUTOSQL_DBTEST_HELPER_ROOT="+factory.LeaseDirectory, "AUTOSQL_DBTEST_HELPER_MAJOR="+strconv.Itoa(factory.Definition.PostgresVersion))
	pipe, err := command.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := command.Start(); err != nil {
		t.Fatal(err)
	}
	lines := make(chan string, 1)
	go func() {
		scanner := bufio.NewScanner(pipe)
		for scanner.Scan() {
			if line, ok := strings.CutPrefix(scanner.Text(), "LEASE="); ok {
				lines <- line
				return
			}
		}
		lines <- ""
	}()
	var path string
	select {
	case path = <-lines:
	case <-time.After(20 * time.Second):
	}
	_ = command.Process.Kill()
	_ = command.Wait()
	if path == "" {
		t.Fatal("helper never persisted a recoverable allocation")
	}
	lease, err := openLeaseFile(path)
	if err != nil {
		t.Fatal(err)
	}
	record := lease.snapshot()
	lease.close()
	if err := CleanupAdminLease(context.Background(), path, nil); err != nil {
		t.Fatal(err)
	}
	requireLeaseResourcesAbsent(t, admin, record)
}

func TestAdminFactoryLivePreflightRefusalsLeaveNoAllocation(t *testing.T) {
	factory, admin := liveAdminFactory(t)
	for name, mutate := range map[string]func(*AdminFactory){
		"actual major mismatch": func(f *AdminFactory) {
			f.Definition.PostgresVersion = 14
			if factory.Definition.PostgresVersion == 14 {
				f.Definition.PostgresVersion = 15
			}
		},
		"unapproved administration database": func(f *AdminFactory) {
			u, _ := url.Parse(f.Definition.Allowlist[0])
			u.Path = "/unapproved"
			f.Definition.Allowlist = []string{u.String()}
		},
		"protected localhost alias": func(f *AdminFactory) {
			u, _ := url.Parse(f.Definition.Allowlist[0])
			u.Host = "localhost:" + u.Port()
			u.Path = "/production"
			f.Definition.ProtectedTargets = []string{u.String()}
		},
	} {
		t.Run(name, func(t *testing.T) {
			candidate := factory
			mutate(&candidate)
			candidate.stage = func(string, leaseRecord) error { t.Error("refused preflight began mutation"); return ErrFactoryConfig }
			if target, err := candidate.Create(context.Background(), "refused"); target != nil || !errors.Is(err, ErrFactoryConfig) {
				t.Fatalf("unsafe factory accepted: %v", err)
			}
			entries, err := os.ReadDir(factory.LeaseDirectory)
			if err != nil || len(entries) != 0 {
				t.Fatal("preflight refusal wrote a lease")
			}
			var registry bool
			if err := admin.QueryRow(context.Background(), "SELECT to_regnamespace($1) IS NOT NULL", registrySchema).Scan(&registry); err != nil || registry {
				t.Fatal("preflight refusal mutated server registry")
			}
		})
	}
}

func TestAdminFactoryLiveCorruptLeaseAndRedirectedAdminRefuseCleanup(t *testing.T) {
	factory, admin := liveAdminFactory(t)
	target, err := factory.Create(context.Background(), "recovery")
	if err != nil {
		t.Fatal(err)
	}
	record, path := target.lease.snapshot(), target.LeasePath()
	if err := target.Detach(); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, append(raw, 'x'), 0600); err != nil {
		t.Fatal(err)
	}
	if err := CleanupAdminLease(context.Background(), path, nil); !errors.Is(err, ErrLease) {
		t.Fatal("corrupt lease authorized cleanup")
	}
	for _, r := range record.Resources {
		oid, _, _, err := readLiveResource(context.Background(), admin, r.Kind, r.Name)
		if err != nil || oid != r.OID {
			t.Fatal("corrupt lease changed owned resources")
		}
	}
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	original := os.Getenv("AUTOSQL_OWNED_ADMIN")
	u, _ := url.Parse(original)
	u.Path = "/postgres"
	t.Setenv("AUTOSQL_OWNED_ADMIN", u.String())
	if err := CleanupAdminLease(context.Background(), path, nil); err == nil {
		t.Fatal("redirected administrator reference authorized cleanup")
	}
	for _, r := range record.Resources {
		oid, _, _, err := readLiveResource(context.Background(), admin, r.Kind, r.Name)
		if err != nil || oid != r.OID {
			t.Fatal("redirected reference changed owned resources")
		}
	}
	t.Setenv("AUTOSQL_OWNED_ADMIN", original)
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	if err := CleanupAdminLease(canceled, path, nil); err != nil {
		t.Fatalf("fresh recovery context failed: %v", err)
	}
	requireLeaseResourcesAbsent(t, admin, record)
}

func TestAdminFactoryLiveQueryTimeoutStillCleans(t *testing.T) {
	factory, admin := liveAdminFactory(t)
	target, err := factory.Create(context.Background(), "query timeout")
	if err != nil {
		t.Fatal(err)
	}
	record := target.lease.snapshot()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if err := target.Exec(ctx, "SELECT pg_sleep(30)"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("query deadline was not retained: %v", err)
	}
	if err := target.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requireLeaseResourcesAbsent(t, admin, record)
}

func TestAdminFactoryLiveNonSuperuserProvisioner(t *testing.T) {
	factory, admin := liveAdminFactory(t)
	name := fmt.Sprintf("autosql_test_admin_%x", time.Now().UnixNano())
	identifier := pgx.Identifier{name}.Sanitize()
	ctx := context.Background()
	if _, err := admin.Exec(ctx, "CREATE ROLE "+identifier+" LOGIN CREATEDB CREATEROLE PASSWORD 'test-only-admin'; GRANT EXECUTE ON FUNCTION pg_catalog.pg_control_system() TO "+identifier); err != nil {
		t.Fatal(err)
	}
	// The control database belongs to this provisioner, which owns the registry.
	var database string
	if err := admin.QueryRow(ctx, "SELECT current_database()").Scan(&database); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+" OWNER TO "+identifier); err != nil {
		t.Fatal(err)
	}
	defer func() {
		_, err := admin.Exec(ctx, "ALTER DATABASE "+pgx.Identifier{database}.Sanitize()+" OWNER TO postgres; DROP OWNED BY "+identifier+"; DROP ROLE "+identifier)
		if err != nil {
			t.Errorf("remove disposable provisioner: %v", err)
		}
	}()
	u, _ := url.Parse(os.Getenv("AUTOSQL_OWNED_ADMIN"))
	u.User = url.UserPassword(name, "test-only-admin")
	t.Setenv("AUTOSQL_OWNED_ADMIN", u.String())
	target, err := factory.Create(ctx, "non-superuser provisioner")
	if err != nil {
		t.Fatal(err)
	}
	record := target.lease.snapshot()
	if err := target.Exec(ctx, "CREATE TABLE restricted_fixture(id integer)"); err != nil {
		t.Error(err)
	}
	if err := target.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requireLeaseResourcesAbsent(t, admin, record)
}

func TestAdminFactoryLiveRecreatedNameCannotReuseOldLease(t *testing.T) {
	factory, admin := liveAdminFactory(t)
	target, err := factory.Create(context.Background(), "replaced database")
	if err != nil {
		t.Fatal(err)
	}
	record, path := target.lease.snapshot(), target.LeasePath()
	if err := target.Detach(); err != nil {
		t.Fatal(err)
	}
	name := pgx.Identifier{record.resourceName()}.Sanitize()
	ctx := context.Background()
	if _, err := admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name+" TEMPLATE template0"); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(ctx, "COMMENT ON DATABASE "+name+" IS '"+record.marker()+"'"); err != nil {
		t.Fatal(err)
	}
	replacement, _, _, err := readLiveResource(ctx, admin, "database", record.resourceName())
	if err != nil {
		t.Fatal(err)
	}
	if err := CleanupAdminLease(ctx, path, nil); !errors.Is(err, ErrOwnership) {
		t.Fatalf("replacement database accepted old lease: %v", err)
	}
	actual, _, _, err := readLiveResource(ctx, admin, "database", record.resourceName())
	if err != nil || actual != replacement {
		t.Fatal("cleanup removed replacement with copied ownership marker")
	}
	// Only this test, which created the replacement, removes it. The factory
	// must leave it alone even though its random name and marker match the lease.
	if _, err := admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
		t.Fatal(err)
	}
	if err := CleanupAdminLease(ctx, path, nil); err != nil {
		t.Fatal(err)
	}
	requireLeaseResourcesAbsent(t, admin, record)
}

func TestAdminFactoryLiveForeignCredentialFileIsRetained(t *testing.T) {
	factory, admin := liveAdminFactory(t)
	target, err := factory.Create(context.Background(), "credential replacement")
	if err != nil {
		t.Fatal(err)
	}
	record, path := target.lease.snapshot(), target.LeasePath()
	credentialPath := filepath.Join(filepath.Dir(path), credentialFilename)
	original, err := os.ReadFile(credentialPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(credentialPath, []byte("foreign-file"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := target.Close(context.Background()); !errors.Is(err, ErrOwnership) {
		t.Fatalf("foreign credential silently removed: %v", err)
	}
	requireLeaseResourcesAbsent(t, admin, record)
	current, err := os.ReadFile(credentialPath)
	if err != nil || string(current) != "foreign-file" {
		t.Fatal("cleanup deleted foreign credential file")
	}
	info, err := InspectLease(path)
	if err != nil || len(info.CleanupErrors) != 1 || info.CleanupErrors[0].Kind != "credentials" {
		t.Fatal("credential residual was not recorded")
	}
	if err := os.WriteFile(credentialPath, original, 0600); err != nil {
		t.Fatal(err)
	}
	if err := CleanupAdminLease(context.Background(), path, nil); err != nil {
		t.Fatal(err)
	}
}
