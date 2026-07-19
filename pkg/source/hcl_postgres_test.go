package source_test

import (
	"context"
	"encoding/json"
	"testing"

	"autosql/pkg/postgres"
	"autosql/pkg/schema"
	"autosql/pkg/source"
)

func TestNativeHCLNormalizesForPostgresProvisioning(t *testing.T) {
	doc, err := source.ParseHCL("native-postgres.hcl", []byte(`
schema "app" {}
enum "state" {
  schema = schema.app
  values = ["pending", "active"]
}
table "accounts" {
  schema = schema.app
  column "id" {
    type = "bigint"
    identity { mode = "always" }
  }
  column "state" {
    type = enum.state
    default = enum_value(enum.state, "pending")
    null = false
  }
  primary_key { columns = [column.id] }
  check "accounts_id_check" { expr = sql("id > 0") }
  index "accounts_state_idx" { columns = [column.state] }
}
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := postgres.New().Normalize(context.Background(), doc); err != nil {
		t.Fatalf("native HCL did not normalize through PostgreSQL: %v", err)
	}
	report, err := postgres.PreflightProvisioning(context.Background(), doc, map[string]string{"postgres_version": "18"})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Supported {
		t.Fatalf("native HCL is not fresh-provisionable: %+v", report.Diagnostics)
	}
}

func TestNativeSecurityHCLNormalizesAndPreflights(t *testing.T) {
	doc, err := source.ParseHCL("security-postgres.hcl", []byte(`
schema "app" {}
role "app" { login = true }
table "accounts" {
  schema = schema.app
  row_security = true
  column "id" { type = "bigint" }
  policy "tenant_read" {
    command = "select"
    roles = [role.app]
    using = sql("id > 0")
    depends_on = [column.id]
  }
  trigger "accounts_touch" {
    events = ["update"]
    function = function["touch()"]
  }
}
view "account_ids" {
  schema = schema.app
  query = "SELECT id FROM app.accounts"
  depends_on = [table.accounts]
}
function "touch()" {
  schema = schema.app
  result = "trigger"
  language = "plpgsql"
  body = <<-SQL
    BEGIN
      RETURN NEW;
    END
  SQL
}
grant "accounts_select" {
  target = table.accounts
  grantee = role.app
  grantor = "postgres"
  privilege = "select"
}
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := postgres.New().Normalize(context.Background(), doc)
	if err != nil {
		t.Fatalf("normalize native security HCL: %v", err)
	}
	digest := ""
	for _, resource := range normalized.Graph.Resources {
		if resource.Kind != schema.KindFunction {
			continue
		}
		var values map[string]any
		if err := json.Unmarshal(resource.Spec, &values); err != nil {
			t.Fatal(err)
		}
		digest, _ = values["body_digest"].(string)
	}
	if digest == "" {
		t.Fatal("routine normalization did not compute a review digest")
	}
	report, err := postgres.PreflightProvisioning(context.Background(), normalized, map[string]string{
		"postgres_version":         "18",
		"reviewed_routine_digests": digest,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Supported {
		t.Fatalf("native security HCL is not provisionable: %+v", report.Diagnostics)
	}
}

func TestTypedSQLNeverBypassesPostgresBoundedGrammar(t *testing.T) {
	doc, err := source.ParseHCL("unsafe.hcl", []byte(`
schema "app" {}
table "accounts" {
  schema = schema.app
  column "created_at" {
    type = "timestamptz"
    default = sql("now(); DROP TABLE app.accounts")
  }
}
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	report, err := postgres.PreflightProvisioning(context.Background(), doc, map[string]string{"postgres_version": "18"})
	if err != nil {
		t.Fatal(err)
	}
	if report.Supported || len(report.Diagnostics) == 0 {
		t.Fatal("sql() bypassed PostgreSQL's bounded provisioning grammar")
	}
}

func TestNativePartitionHCLNormalizesAndPreflights(t *testing.T) {
	doc, err := source.ParseHCL("partition.hcl", []byte(`
schema "app" {}
table "events" {
  schema = schema.app
  column "tenant_id" { type = "bigint" }
  column "created_at" { type = "timestamptz" }
  partition {
    by = "range"
    columns = [column.created_at]
  }
}
table "events_2026" {
  schema = schema.app
  partition_of = table.events
  bound = sql("FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')")
}
`), nil)
	if err != nil {
		t.Fatal(err)
	}
	normalized, err := postgres.New().Normalize(context.Background(), doc)
	if err != nil {
		t.Fatal(err)
	}
	partitionColumns := 0
	for _, resource := range normalized.Graph.Resources {
		if resource.Kind == schema.KindColumn && resource.Name.Name == "created_at" {
			partitionColumns++
		}
	}
	if partitionColumns != 2 {
		t.Fatalf("partition inherited columns were not canonicalized: %d", partitionColumns)
	}
	report, err := postgres.PreflightProvisioning(context.Background(), normalized, map[string]string{"postgres_version": "18"})
	if err != nil {
		t.Fatal(err)
	}
	if !report.Supported {
		t.Fatalf("native partition HCL is not provisionable: %+v", report.Diagnostics)
	}
}
