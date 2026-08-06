package terraformprovider

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5"
)

func TestTerraformLifecycleAgainstPostgres(t *testing.T) {
	url := os.Getenv("AUTOSQL_TF_ACC_PG_URL")
	if url == "" {
		t.Skip("AUTOSQL_TF_ACC_PG_URL is not set")
	}
	if _, err := exec.LookPath("terraform"); err != nil {
		t.Skip("terraform is not installed")
	}
	if _, err := exec.LookPath("psql"); err != nil {
		t.Skip("psql is not installed")
	}
	dir := t.TempDir()
	plugins := filepath.Join(dir, "plugins")
	if err := os.Mkdir(plugins, 0700); err != nil {
		t.Fatal(err)
	}
	providerBinary := filepath.Join(plugins, "terraform-provider-autosql")
	run(t, "", nil, "go", "build", "-o", providerBinary, "../../cmd/terraform-provider-autosql")

	applyConfig := filepath.Join(dir, "apply.json")
	if err := os.WriteFile(applyConfig, []byte("{}"), 0600); err != nil {
		t.Fatal(err)
	}
	artifacts := map[string]string{"create.artifact": "create", "update.artifact": "update", "destroy.artifact": "destroy", "approval.json": "approval", "destroy-approval.json": "destroy approval"}
	for name, value := range artifacts {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
	}
	helper := filepath.Join(dir, "autosql-helper")
	helperBody := `#!/usr/bin/env sh
set -eu
artifact="$3"
case "$(basename "$artifact")" in
  create.artifact) sql='CREATE SCHEMA IF NOT EXISTS autosql_tf_acc; CREATE TABLE IF NOT EXISTS autosql_tf_acc.items (id bigint PRIMARY KEY)' ;;
  update.artifact) sql='ALTER TABLE autosql_tf_acc.items ADD COLUMN IF NOT EXISTS name text' ;;
  destroy.artifact) sql='DROP SCHEMA IF EXISTS autosql_tf_acc CASCADE' ;;
  *) echo 'unexpected artifact' >&2; exit 2 ;;
esac
psql "$AUTOSQL_TF_ACC_PG_URL" -v ON_ERROR_STOP=1 -q -c "$sql"`
	if err := os.WriteFile(helper, []byte(helperBody), 0700); err != nil {
		t.Fatal(err)
	}
	rc := filepath.Join(dir, "terraformrc")
	if err := os.WriteFile(rc, []byte(fmt.Sprintf("provider_installation {\n  dev_overrides {\n    \"registry.terraform.io/stigenai/autosql\" = %q\n  }\n  direct {}\n}\n", plugins)), 0600); err != nil {
		t.Fatal(err)
	}

	writeConfig := func(source string) {
		content := fmt.Sprintf(`terraform {
  required_providers {
    autosql = {
      source = "stigenai/autosql"
    }
  }
}
provider "autosql" {
  binary_path = %q
  apply_config_ref = %q
}
resource "autosql_schema" "acceptance" {
  id = "acceptance"
  source_ref = %q
  artifact_digest = %q
  policy_digest = %q
  target_snapshot = %q
  target_id = "postgres-acceptance"
  environment = "acceptance"
  connection_ref = "env://AUTOSQL_TF_ACC_PG_URL"
  approval_ref = %q
  approval_digest = %q
  destroy_source_ref = %q
  destroy_artifact_digest = %q
  destroy_approval_ref = %q
  destroy_approval_digest = %q
}
`, helper, "file://"+applyConfig, "file://"+filepath.Join(dir, source), digest(artifacts[source]), digest("policy"), digest("target"), "file://"+filepath.Join(dir, "approval.json"), digest("approval"), "file://"+filepath.Join(dir, "destroy.artifact"), digest("destroy"), "file://"+filepath.Join(dir, "destroy-approval.json"), digest("destroy approval"))
		if err := os.WriteFile(filepath.Join(dir, "main.tf"), []byte(content), 0600); err != nil {
			t.Fatal(err)
		}
	}
	env := []string{"TF_CLI_CONFIG_FILE=" + rc, "AUTOSQL_TF_ACC_PG_URL=" + url, "TF_IN_AUTOMATION=1"}
	writeConfig("create.artifact")
	run(t, dir, env, "terraform", "apply", "-auto-approve", "-input=false", "-no-color")
	assertQuery(t, url, `select count(*) from information_schema.columns where table_schema='autosql_tf_acc' and table_name='items'`, 1)
	writeConfig("update.artifact")
	run(t, dir, env, "terraform", "apply", "-auto-approve", "-input=false", "-no-color")
	assertQuery(t, url, `select count(*) from information_schema.columns where table_schema='autosql_tf_acc' and table_name='items'`, 2)

	run(t, dir, env, "terraform", "state", "rm", "autosql_schema.acceptance")
	imported := map[string]string{"ID": "acceptance", "SourceRef": "file://" + filepath.Join(dir, "update.artifact"), "ArtifactDigest": digest("update"), "PolicyDigest": digest("policy"), "TargetSnapshot": digest("target"), "TargetID": "postgres-acceptance", "Environment": "acceptance", "ConnectionRef": "env://AUTOSQL_TF_ACC_PG_URL", "ApprovalRef": "file://" + filepath.Join(dir, "approval.json"), "ApprovalDigest": digest("approval"), "DestroySourceRef": "file://" + filepath.Join(dir, "destroy.artifact"), "DestroyArtifactDigest": digest("destroy"), "DestroyApprovalRef": "file://" + filepath.Join(dir, "destroy-approval.json"), "DestroyApprovalDigest": digest("destroy approval")}
	identity, _ := json.Marshal(imported)
	run(t, dir, env, "terraform", "import", "-input=false", "autosql_schema.acceptance", string(identity))
	run(t, dir, env, "terraform", "apply", "-auto-approve", "-refresh-only", "-input=false", "-no-color")
	state := run(t, dir, env, "terraform", "show", "-json")
	if strings.Contains(state, url) {
		t.Fatal("resolved database URL entered Terraform state")
	}
	run(t, dir, env, "terraform", "destroy", "-auto-approve", "-input=false", "-no-color")
	assertQuery(t, url, `select count(*) from information_schema.schemata where schema_name='autosql_tf_acc'`, 0)
}

func digest(value string) string {
	h := sha256.Sum256([]byte(value))
	return "sha256:" + hex.EncodeToString(h[:])
}

func run(t *testing.T, dir string, env []string, command string, args ...string) string {
	t.Helper()
	cmd := exec.Command(command, args...)
	if dir != "" {
		cmd.Dir = dir
	}
	cmd.Env = append(os.Environ(), env...)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("%s %v failed: %v\n%s", command, args, err, out)
	}
	return string(out)
}

func assertQuery(t *testing.T, url, query string, want int) {
	t.Helper()
	conn, err := pgx.Connect(context.Background(), url)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close(context.Background())
	var got int
	if err := conn.QueryRow(context.Background(), query).Scan(&got); err != nil || got != want {
		t.Fatalf("query result=%d want=%d err=%v", got, want, err)
	}
}
