package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"

	"autosql/pkg/dbtest"
	"autosql/pkg/secret"

	"github.com/jackc/pgx/v5"
)

type devLifecycleOutput struct {
	OK   bool `json:"ok"`
	Data struct {
		dbtest.LeaseInfo
		CredentialReference secret.Reference `json:"credential_ref"`
	} `json:"data"`
	Error *EnvelopeError `json:"error"`
}

func liveDevCLI(t *testing.T) (string, *pgx.Conn, int) {
	t.Helper()
	raw := os.Getenv("AUTOSQL_DBTEST_ADMIN_DSN")
	if raw == "" {
		t.Skip("AUTOSQL_DBTEST_ADMIN_DSN unset; requires disposable PostgreSQL administrator")
	}
	adminURL := projectLiveDatabase(t, raw, "dev_cli")
	conn, err := pgx.Connect(context.Background(), adminURL)
	if err != nil {
		t.Fatal("connect disposable administration database")
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	var version int
	if err := conn.QueryRow(context.Background(), "SELECT current_setting('server_version_num')::int").Scan(&version); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_EXAMPLE_LOCAL_ADMIN", adminURL)
	u, _ := url.Parse(adminURL)
	u.User = nil
	u.RawQuery = ""
	body, err := os.ReadFile("../../examples/project-testing/development.hcl")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(string(body), "postgres://127.0.0.1:55432/postgres", u.String())
	source = strings.ReplaceAll(source, "= 17", "= "+strconv.Itoa(version/10000))
	project := filepath.Join(t.TempDir(), "development.hcl")
	if err := os.WriteFile(project, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	return project, conn, version / 10000
}

func TestDatabaseDevLiveCLIOutputAndRecovery(t *testing.T) {
	project, admin, major := liveDevCLI(t)
	root := filepath.Join(t.TempDir(), "leases")
	var out, stderr bytes.Buffer
	args := []string{"database", "dev", "create", "--config", project, "--postgres-version", strconv.Itoa(major), "--lease-dir", root, "--json"}
	if code := Run(context.Background(), args, Streams{Out: &out, Err: &stderr}); code != 0 {
		t.Fatalf("create code=%d result=%s", code, out.String())
	}
	var created devLifecycleOutput
	if err := json.Unmarshal(out.Bytes(), &created); err != nil {
		t.Fatal(err)
	}
	if !created.OK || created.Data.State != "ready" || len(created.Data.Resources) != 2 || created.Data.CredentialReference == "" || stderr.Len() != 0 {
		t.Fatalf("invalid create result: %s", out.String())
	}
	if strings.Contains(out.String(), "postgres://") || strings.Contains(out.String(), "CLI_DEV_ADMIN") || strings.Contains(out.String(), "disposable@") {
		t.Fatal("output exposed administrator data")
	}
	lease := created.Data.Path
	t.Cleanup(func() {
		if err := dbtest.CleanupAdminLease(context.Background(), lease, nil); err != nil {
			t.Error(err)
		}
	})
	value, err := secret.NewResolver().Resolve(context.Background(), created.Data.CredentialReference)
	if err != nil {
		t.Fatal(err)
	}
	fixture, err := pgx.Connect(context.Background(), value)
	if err != nil {
		t.Fatal("connect private fixture reference")
	}
	if _, err := fixture.Exec(context.Background(), "CREATE TABLE cli_fixture(id int)"); err != nil {
		t.Error(err)
	}
	fixture.Close(context.Background())
	// An interrupted interactive user can retry cleanup from this exact lease.
	var database string
	for _, r := range created.Data.Resources {
		if r.Kind == "database" {
			database = r.Name
		}
	}
	var marker string
	if err := admin.QueryRow(context.Background(), "SELECT shobj_description(oid,'pg_database') FROM pg_database WHERE datname=$1", database).Scan(&marker); err != nil {
		t.Fatal(err)
	}
	if _, err := admin.Exec(context.Background(), "COMMENT ON DATABASE "+pgx.Identifier{database}.Sanitize()+" IS 'foreign'"); err != nil {
		t.Fatal(err)
	}
	cleanup := []string{"database", "dev", "cleanup", "--lease", lease, "--json"}
	out.Reset()
	if code := Run(context.Background(), cleanup, Streams{Out: &out, Err: &stderr}); code != int(ExitConflict) {
		t.Fatalf("foreign resource cleanup code=%d result=%s", code, out.String())
	}
	var refused devLifecycleOutput
	if err := json.Unmarshal(out.Bytes(), &refused); err != nil {
		t.Fatal(err)
	}
	if refused.OK || refused.Data.State != "cleanup_failed" || len(refused.Data.CleanupErrors) == 0 || refused.Error.RecoveryGuidance == "" {
		t.Fatalf("residual omitted: %s", out.String())
	}
	if _, err := admin.Exec(context.Background(), "COMMENT ON DATABASE "+pgx.Identifier{database}.Sanitize()+" IS '"+marker+"'"); err != nil {
		t.Fatal(err)
	}
	out.Reset()
	if code := Run(context.Background(), cleanup, Streams{Out: &out, Err: &stderr}); code != 0 {
		t.Fatalf("recovery code=%d result=%s", code, out.String())
	}
	var cleaned devLifecycleOutput
	if err := json.Unmarshal(out.Bytes(), &cleaned); err != nil {
		t.Fatal(err)
	}
	if !cleaned.OK || cleaned.Data.State != "cleaned" {
		t.Fatalf("wrong cleanup result: %s", out.String())
	}
	for _, r := range cleaned.Data.Resources {
		var count int
		query := "SELECT count(*) FROM pg_roles WHERE rolname=$1"
		if r.Kind == "database" {
			query = "SELECT count(*) FROM pg_database WHERE datname=$1"
		}
		if err := admin.QueryRow(context.Background(), query, r.Name).Scan(&count); err != nil || count != 0 {
			t.Fatal("reported cleanup left owned resource")
		}
	}
	u, _ := url.Parse(string(created.Data.CredentialReference))
	if _, err := os.Stat(u.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("cleanup retained private credential")
	}
}

type failedDevOutput struct{}

func (failedDevOutput) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

func TestDatabaseDevLiveOutputFailureCleansAllocation(t *testing.T) {
	project, admin, major := liveDevCLI(t)
	root := filepath.Join(t.TempDir(), "leases")
	args := []string{"database", "dev", "create", "--config", project, "--postgres-version", strconv.Itoa(major), "--lease-dir", root, "--json"}
	if code := Run(context.Background(), args, Streams{Out: failedDevOutput{}}); code != int(ExitInternal) {
		t.Fatalf("write failure code=%d", code)
	}
	paths, err := filepath.Glob(filepath.Join(root, "*", "lease.json"))
	if err != nil || len(paths) != 1 {
		t.Fatal("missing recovery lease")
	}
	info, err := dbtest.InspectLease(paths[0])
	if err != nil || info.State != "cleaned" {
		t.Fatal("output failure retained allocation")
	}
	for _, r := range info.Resources {
		var count int
		query := "SELECT count(*) FROM pg_roles WHERE rolname=$1"
		if r.Kind == "database" {
			query = "SELECT count(*) FROM pg_database WHERE datname=$1"
		}
		if err := admin.QueryRow(context.Background(), query, r.Name).Scan(&count); err != nil || count != 0 {
			t.Fatal("output failure left owned resource")
		}
	}
}

func TestDatabaseDevLiveContainerLifecycle(t *testing.T) {
	endpoint, image, major := os.Getenv("AUTOSQL_DBTEST_DOCKER_ENDPOINT"), os.Getenv("AUTOSQL_DBTEST_POSTGRES_IMAGE"), os.Getenv("AUTOSQL_DBTEST_POSTGRES_MAJOR")
	if endpoint == "" || image == "" || major == "" {
		t.Skip("container factory integration settings unset")
	}
	body, err := os.ReadFile("../../examples/project-testing/container.hcl")
	if err != nil {
		t.Fatal(err)
	}
	source := strings.ReplaceAll(string(body), strconv.Quote("unix:///var/run/docker.sock"), strconv.Quote(endpoint))
	source = strings.ReplaceAll(source, strconv.Quote("postgres@sha256:eee6c6e1b0894fa4fe0075eb054af9fd1cf39a35321c2d4110626cfe506411fb"), strconv.Quote(image))
	source = strings.ReplaceAll(source, "= 17", "= "+major)
	project := filepath.Join(t.TempDir(), "container.hcl")
	if err := os.WriteFile(project, []byte(source), 0600); err != nil {
		t.Fatal(err)
	}
	for _, jsonMode := range []bool{true, false} {
		t.Run(strconv.FormatBool(jsonMode), func(t *testing.T) {
			root, err := os.MkdirTemp("", "autosql-container-cli-test-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if !t.Failed() {
					_ = os.RemoveAll(root)
				} else {
					t.Logf("recovery leases retained at %s", root)
				}
			})
			args := []string{"database", "dev", "create", "--config", project, "--env", "local", "--postgres-version", major, "--lease-dir", root}
			if jsonMode {
				args = append(args, "--json")
			}
			var out, stderr bytes.Buffer
			if code := Run(context.Background(), args, Streams{Out: &out, Err: &stderr}); code != 0 {
				t.Fatalf("container create failed: code=%d output=%s stderr=%s", code, out.String(), stderr.String())
			}
			var lease string
			if jsonMode {
				var result devLifecycleOutput
				if err := json.Unmarshal(out.Bytes(), &result); err != nil {
					t.Fatal(err)
				}
				if !result.OK || result.Data.State != "ready" || len(result.Data.Resources) != 3 || result.Data.CredentialReference == "" {
					t.Fatal("incomplete container lifecycle output")
				}
				lease = result.Data.Path
			} else {
				for _, line := range strings.Split(out.String(), "\n") {
					if path, ok := strings.CutPrefix(line, "lease: "); ok {
						lease = path
					}
				}
				if !strings.Contains(out.String(), "credential reference: file:") {
					t.Fatal("human output omitted restricted reference")
				}
			}
			if lease == "" || strings.Contains(out.String(), "postgres://") || strings.Contains(out.String(), "POSTGRES_PASSWORD") || stderr.Len() != 0 {
				t.Fatal("unsafe or missing lifecycle output")
			}
			t.Cleanup(func() {
				if err := dbtest.CleanupLease(context.Background(), lease, nil); err != nil {
					t.Error(err)
				}
			})
			out.Reset()
			cleanup := []string{"database", "dev", "cleanup", "--lease", lease, "--json"}
			if code := Run(context.Background(), cleanup, Streams{Out: &out, Err: &stderr}); code != 0 {
				t.Fatalf("container cleanup failed: code=%d output=%s", code, out.String())
			}
			info, err := dbtest.InspectLease(lease)
			if err != nil || info.State != "cleaned" {
				t.Fatal("CLI did not record container cleanup")
			}
		})
	}
}
