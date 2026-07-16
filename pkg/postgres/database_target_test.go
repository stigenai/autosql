package postgres

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"

	"autosql/pkg/bootstrap"

	"github.com/jackc/pgx/v5"
)

func TestDatabaseTargetValidationAndCreateSQLFailClosed(t *testing.T) {
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: "db", Port: 5432, TLSMode: "require"}, MaintenanceDatabase: "postgres", Name: `cell";drop database postgres`, Owner: "owner", LocaleProvider: "future", ConnectionLimit: -2}
	if err := target.Validate(); err == nil {
		t.Fatal("invalid target passed validation")
	}
	valid := target
	valid.Name = `cell"name`
	valid.LocaleProvider = "libc"
	valid.ConnectionLimit = -1
	valid = valid.Normalize()
	if err := valid.Validate(); err != nil {
		t.Fatal(err)
	}
	sql := renderCreateDatabase(valid, 16, true)
	if strings.Contains(sql, `;drop`) || !strings.Contains(sql, `"cell""name"`) {
		t.Fatalf("identifier was not safely rendered: %s", sql)
	}
}

func TestManagedAndExternalDatabaseTargetLifecycle(t *testing.T) {
	url := os.Getenv("AUTOSQL_TEST_POSTGRES_URL")
	if url == "" {
		t.Skip("AUTOSQL_TEST_POSTGRES_URL is not set")
	}
	ctx := context.Background()
	maintenance, err := pgx.Connect(ctx, url)
	if err != nil {
		t.Fatal(err)
	}
	defer maintenance.Close(context.Background())
	for _, name := range []string{"autosql_target_managed", "autosql_target_renamed", "autosql_target_closed"} {
		_, _ = maintenance.Exec(ctx, "drop database if exists "+pgx.Identifier{name}.Sanitize()+" with (force)")
	}
	defer func() {
		for _, name := range []string{"autosql_target_managed", "autosql_target_renamed", "autosql_target_closed"} {
			_, _ = maintenance.Exec(context.Background(), "drop database if exists "+pgx.Identifier{name}.Sanitize()+" with (force)")
		}
	}()
	config, err := pgx.ParseConfig(url)
	if err != nil {
		t.Fatal(err)
	}
	target := bootstrap.DatabaseTarget{
		Mode: bootstrap.ManagedDatabase, Endpoint: bootstrap.ServerEndpoint{Host: config.Host, Port: uint16(config.Port), TLSMode: "disable"},
		MaintenanceDatabase: config.Database, Name: "autosql_target_managed", Owner: "postgres", Encoding: "UTF8",
		Collation: "C", CharacterType: "C", Template: "template0", Tablespace: "pg_default", ConnectionLimit: 11, AllowConnections: true,
	}
	var serverVersion int
	if err := maintenance.QueryRow(ctx, `select current_setting('server_version_num')::integer`).Scan(&serverVersion); err != nil {
		t.Fatal(err)
	}
	if serverVersion/10000 >= 15 {
		target.LocaleProvider = "libc"
	}
	report, err := PreflightDatabaseTargetURL(ctx, url, target)
	if err != nil || !report.Supported || report.Exists {
		t.Fatalf("managed preflight=%+v err=%v", report, err)
	}
	prepared, err := PrepareDatabaseURL(ctx, url, target)
	if err != nil || !prepared.Created || prepared.Connection == nil {
		t.Fatalf("prepared=%+v err=%v", prepared, err)
	}
	var database string
	if err := prepared.Connection.QueryRow(ctx, `select current_database()`).Scan(&database); err != nil || database != target.Name {
		t.Fatalf("database=%s err=%v", database, err)
	}
	prepared.Connection.Close(ctx)

	report, err = PreflightDatabaseTargetURL(ctx, url, target)
	if err != nil || report.Supported || !report.Exists || len(report.Diagnostics) == 0 || report.Diagnostics[0].Class != "collision" {
		t.Fatalf("collision report=%+v err=%v", report, err)
	}
	external := target
	external.Mode = bootstrap.ExternalDatabase
	report, err = PreflightDatabaseTargetURL(ctx, url, external)
	if err != nil || !report.Supported {
		t.Fatalf("external verification=%+v err=%v", report, err)
	}
	existing, err := PrepareDatabaseURL(ctx, url, external)
	if err != nil || existing.Created || existing.Connection == nil {
		t.Fatalf("external prepare=%+v err=%v", existing, err)
	}
	existing.Connection.Close(ctx)

	immutableMismatch := external
	immutableMismatch.Encoding = "LATIN1"
	report, err = PreflightDatabaseTargetURL(ctx, url, immutableMismatch)
	if err != nil || report.Supported || !containsDatabaseDiagnostic(report.Diagnostics, "immutable_rebuild", "encoding") {
		t.Fatalf("immutable report=%+v err=%v", report, err)
	}
	if err := RenameDatabaseURL(ctx, url, target.Name, "autosql_target_renamed"); err != nil {
		t.Fatal(err)
	}
	external.Name = "autosql_target_renamed"
	if report, err = PreflightDatabaseTargetURL(ctx, url, external); err != nil || !report.Supported {
		t.Fatalf("renamed report=%+v err=%v", report, err)
	}
	if err := DropDatabaseURL(ctx, url, external.Name, false); err != nil {
		t.Fatal(err)
	}
	missing := external
	missing.Name = "autosql_target_missing"
	if report, err = PreflightDatabaseTargetURL(ctx, url, missing); err != nil || report.Supported || !containsDatabaseDiagnostic(report.Diagnostics, "missing", "name") {
		t.Fatalf("missing external report=%+v err=%v", report, err)
	}

	closed := target
	closed.Name, closed.AllowConnections = "autosql_target_closed", false
	prepared, err = PrepareDatabaseURL(ctx, url, closed)
	if err != nil || prepared.Connection == nil {
		t.Fatalf("closed target prepare=%+v err=%v", prepared, err)
	}
	var allowed bool
	if err := maintenance.QueryRow(ctx, `select datallowconn from pg_database where datname=$1`, closed.Name).Scan(&allowed); err != nil || allowed {
		t.Fatalf("allow connections=%v err=%v", allowed, err)
	}
	prepared.Connection.Close(ctx)
	if err := DropDatabaseURL(ctx, url, closed.Name, true); err != nil {
		t.Fatal(err)
	}

	_, _ = maintenance.Exec(ctx, `drop role if exists autosql_no_createdb`)
	if _, err := maintenance.Exec(ctx, `create role autosql_no_createdb login password 'test-only-password'`); err != nil {
		t.Fatal(err)
	}
	defer maintenance.Exec(context.Background(), `drop role if exists autosql_no_createdb`)
	limitedURL := fmt.Sprintf("postgres://autosql_no_createdb:test-only-password@%s:%d/%s?sslmode=disable", config.Host, config.Port, config.Database)
	permissionTarget := target
	permissionTarget.Name = "autosql_permission_denied"
	if report, err = PreflightDatabaseTargetURL(ctx, limitedURL, permissionTarget); err != nil || report.Supported || !containsDatabaseDiagnostic(report.Diagnostics, "permission", "create_database") {
		t.Fatalf("permission report=%+v err=%v", report, err)
	}
}

func TestDatabaseTargetPreflightRejectsEndpointMismatchBeforeConnection(t *testing.T) {
	target := bootstrap.DatabaseTarget{Mode: bootstrap.ExternalDatabase, Endpoint: bootstrap.ServerEndpoint{Host: "elsewhere", Port: 5432, TLSMode: "require"}, MaintenanceDatabase: "postgres", Name: "cell", Owner: "owner", ConnectionLimit: -1, AllowConnections: true}
	_, err := PreflightDatabaseTargetURL(context.Background(), "postgres://user:secret@127.0.0.1:1/postgres?sslmode=disable", target)
	if err == nil || strings.Contains(err.Error(), "secret") {
		t.Fatalf("endpoint mismatch err=%v", err)
	}
}

func containsDatabaseDiagnostic(diagnostics []DatabaseTargetDiagnostic, class, field string) bool {
	for _, diagnostic := range diagnostics {
		if diagnostic.Class == class && diagnostic.Field == field {
			return true
		}
	}
	return false
}
