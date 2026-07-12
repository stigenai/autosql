package cli

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"autosql/pkg/zerodowntime"
)

func TestMigrateValidateOnlineJSONAndYAML(t *testing.T) {
	m, err := zerodowntime.New("add_name", zerodowntime.VersionSchema{Name: "v1", ExposeDuringExpand: true}, zerodowntime.Requirements{MinimumPostgres: 14, LockTimeoutMS: 1000, StatementTimeoutMS: 5000}, []zerodowntime.Operation{{ID: "01", Kind: zerodowntime.AddColumn, Table: "users", Column: "name", DataType: "text"}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, format := range []string{"json", "yaml"} {
		t.Run(format, func(t *testing.T) {
			var data []byte
			if format == "json" {
				data, _ = m.MarshalJSONCanonical()
			} else {
				data, _ = m.MarshalYAMLCanonical()
			}
			path := filepath.Join(t.TempDir(), "artifact."+format)
			if err := os.WriteFile(path, data, 0600); err != nil {
				t.Fatal(err)
			}
			var out, stderr bytes.Buffer
			code := Run(context.Background(), []string{"migrate", "validate-online", "--file", path, "--format", format, "--json"}, Streams{Out: &out, Err: &stderr})
			if code != 0 {
				t.Fatalf("code=%d stderr=%s", code, stderr.String())
			}
			if !strings.Contains(out.String(), m.Digest) {
				t.Fatalf("missing digest: %s", out.String())
			}
		})
	}
}

func TestMigrateValidateOnlineRejectsBeforeTargetUse(t *testing.T) {
	path := filepath.Join(t.TempDir(), "bad.json")
	if err := os.WriteFile(path, []byte(`{"password":"do-not-leak"}`), 0600); err != nil {
		t.Fatal(err)
	}
	var out, stderr bytes.Buffer
	code := Run(context.Background(), []string{"migrate", "validate-online", "--file", path}, Streams{Out: &out, Err: &stderr})
	if code != int(ExitValidation) {
		t.Fatalf("code=%d", code)
	}
	if strings.Contains(stderr.String(), "do-not-leak") {
		t.Fatal("secret leaked")
	}
}
