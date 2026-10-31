package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosql/pkg/config"
	"autosql/pkg/dbtest"
	"autosql/pkg/secret"
)

func devProjectFixture(t *testing.T, admin secret.Reference, allowlist string, major int) string {
	t.Helper()
	c := config.Config{Version: 2, Environment: "dev", Environments: map[string]config.Environment{
		"dev":  {Classification: "development", Target: "file:///must-not-read-selected-target", DevFactory: "local", SchemaSources: []config.SchemaSource{{Kind: "sql", Path: "not-read.sql"}}, MigrationDir: "not-read-migrations"},
		"prod": {Classification: "production", Target: "file:///must-not-read-production-target", SchemaSources: []config.SchemaSource{{Kind: "sql", Path: "not-read.sql"}}, MigrationDir: "not-read-migrations"},
	}, DevFactories: map[string]config.DevelopmentFactory{"local": {Mode: "existing_admin", Admin: admin, AdminClassification: "development", Allowlist: []string{allowlist}, PostgresVersion: major}}}
	raw, err := json.Marshal(c)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "project.json")
	if err := os.WriteFile(path, raw, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

func TestDatabaseDevCLIRefusals(t *testing.T) {
	t.Setenv("ABSENT_DEV_ADMIN", "")
	project := devProjectFixture(t, "env://ABSENT_DEV_ADMIN", "postgres://127.0.0.1:5432/postgres", 17)
	for _, test := range []struct {
		name string
		args []string
		code ExitCode
	}{
		{"missing version", []string{"create", "--config", project}, ExitUsage},
		{"production selection", []string{"create", "--config", project, "--env", "prod", "--postgres-version", "17"}, ExitConfig},
		{"configured major mismatch", []string{"create", "--config", project, "--postgres-version", "16"}, ExitConfig},
		{"missing administrator", []string{"create", "--config", project, "--postgres-version", "17", "--lease-dir", filepath.Join(t.TempDir(), "leases")}, ExitSecret},
		{"no cleanup all", []string{"cleanup", "--all"}, ExitUsage},
		{"missing lease", []string{"cleanup", "--lease", filepath.Join(t.TempDir(), "lease.json")}, ExitConflict},
	} {
		t.Run(test.name, func(t *testing.T) {
			var out, stderr bytes.Buffer
			args := append([]string{"database", "dev"}, test.args...)
			args = append(args, "--json")
			code := Run(context.Background(), args, Streams{Out: &out, Err: &stderr})
			var result Envelope
			if err := json.Unmarshal(out.Bytes(), &result); err != nil {
				t.Fatal(err)
			}
			if code != int(test.code) || result.OK || result.Error == nil || stderr.Len() != 0 {
				t.Fatalf("unexpected refusal code=%d output=%s stderr=%s", code, out.String(), stderr.String())
			}
		})
	}
}

func TestProjectAdminFactoryProtectsProductionReferenceWithoutResolution(t *testing.T) {
	project := devProjectFixture(t, "file:///must-not-read-production-target", "postgres://localhost/postgres", 17)
	selected, err := projectDevelopmentFactory(devCreateArgs{project: project, major: 17, leaseDirectory: filepath.Join(t.TempDir(), "leases")}, secret.NewRedactor())
	if err != nil {
		t.Fatal(err)
	}
	factory := selected.(dbtest.AdminFactory)
	calls := 0
	factory.Resolver.ReadFile = func(string) ([]byte, error) { calls++; return nil, errors.New("must not read") }
	if _, err := factory.Create(context.Background(), "refused"); !errors.Is(err, dbtest.ErrFactoryConfig) {
		t.Fatal("production reference reached runtime")
	}
	if calls != 0 {
		t.Fatal("factory read a protected reference")
	}
}

func TestDatabaseDevFailurePreservesRecoveryDataAndRedaction(t *testing.T) {
	original := &Error{Kind: "migration", Code: ExitMigration, Message: "failed", Cause: context.Canceled, RecoveryGuidance: "inspect recorded lease", Data: map[string]any{"state": "cleanup_failed", "password": "sentinel", "detail": "postgres://user:sentinel@host/db"}}
	classified := classify(original, nil)
	var out bytes.Buffer
	output{json: true, streams: Streams{Out: &out}, redactor: secret.NewRedactor("sentinel")}.failure(classified)
	var result Envelope
	if err := json.Unmarshal(out.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.Error.ExitCode != 130 || result.Data == nil || result.Error.RecoveryGuidance != "inspect recorded lease" || strings.Contains(out.String(), "sentinel") {
		t.Fatalf("recovery output lost or leaked: %s", out.String())
	}
}

func TestDatabaseDevCleanupFailurePreservesPrimaryExit(t *testing.T) {
	primary := &dbtest.FactoryError{Stage: "provision"}
	result := devFactoryError(errors.Join(primary, dbtest.ErrOwnership))
	if result.Code != ExitMigration {
		t.Fatalf("cleanup conflict replaced primary provisioning failure: %d", result.Code)
	}
}
