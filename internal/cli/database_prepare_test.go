package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosql/pkg/bootstrap"
	"autosql/pkg/postgres"
)

func TestDatabasePrepareCLIUsesHCLContractAndSecretReference(t *testing.T) {
	directory := t.TempDir()
	targetPath := filepath.Join(directory, "database.hcl")
	data := []byte(`database "cell" {
  mode = "managed"
  endpoint = { host = "db.internal", port = 5432, tls_mode = "require" }
  maintenance_database = "postgres"
  owner = "cell_owner"
  encoding = "UTF8"
  locale_provider = "libc"
  collation = "C"
  character_type = "C"
  template = "template0"
  tablespace = "pg_default"
  connection_limit = 20
  allow_connections = true
}`)
	if err := os.WriteFile(targetPath, data, 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("AUTOSQL_MAINTENANCE_TEST_URL", "postgres://secret:runtime-password@db.internal:5432/postgres")
	old := prepareDatabaseURL
	defer func() { prepareDatabaseURL = old }()
	var captured bootstrap.DatabaseTarget
	prepareDatabaseURL = func(_ context.Context, url string, target bootstrap.DatabaseTarget) (postgres.PreparedDatabase, error) {
		if !strings.Contains(url, "runtime-password") {
			t.Fatalf("resolved URL=%q", url)
		}
		captured = target
		return postgres.PreparedDatabase{Target: target, Created: true}, nil
	}
	var stdout, stderr bytes.Buffer
	code := Run(context.Background(), []string{"database", "prepare", "--target-hcl", targetPath, "--maintenance-url", "env://AUTOSQL_MAINTENANCE_TEST_URL", "--json"}, Streams{Out: &stdout, Err: &stderr})
	if code != 0 {
		t.Fatalf("code=%d stdout=%s stderr=%s", code, stdout.String(), stderr.String())
	}
	if captured.Name != "cell" || captured.Mode != bootstrap.ManagedDatabase || captured.Endpoint.Host != "db.internal" || captured.ConnectionLimit != 20 || !captured.AllowConnections {
		t.Fatalf("captured=%+v", captured)
	}
	combined := stdout.String() + stderr.String()
	if strings.Contains(combined, "runtime-password") || strings.Contains(combined, "postgres://") {
		t.Fatalf("CLI leaked maintenance secret: %s", combined)
	}
}
