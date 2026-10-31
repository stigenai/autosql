version     = 2
environment = "local"

variables = {
  ci_targets = { postgres = "AUTOSQL_EXAMPLE_CI_TARGET" }
}

dev_factory "local_admin" {
  mode                 = "existing_admin"
  admin                = "env://AUTOSQL_EXAMPLE_LOCAL_ADMIN"
  admin_classification = "development"
  postgres_version     = 17
  allowlist            = ["postgres://127.0.0.1:55432/postgres"]
  protected_targets    = ["postgres://production.example.invalid:5432/app"]
}

dev_factory "ci_admin" {
  mode                 = "existing_admin"
  admin                = "env://AUTOSQL_EXAMPLE_CI_ADMIN"
  admin_classification = "development"
  postgres_version     = 17
  allowlist            = ["postgres://postgres:5432/postgres"]
  protected_targets    = ["postgres://production.example.invalid:5432/app"]
}

environment "local" {
  classification = "development"
  target         = "env://AUTOSQL_EXAMPLE_LOCAL_TARGET"
  dev_factory    = "local_admin"
  schema_sources = [{ kind = "hcl", path = "schema.hcl" }]
  migration_dir  = "migrations"
  tests          = { files = ["tests/*.test.hcl"], parallel = 1, timeout = "5m" }
  reporting      = { json = false }
}

environment_set "ci" {
  for_each       = var.ci_targets
  classification = "development"
  target         = "env://${each.value}"
  dev_factory    = "ci_admin"
  schema_sources = [{ kind = "hcl", path = "schema.hcl" }]
  migration_dir  = "migrations"
  tests          = { files = ["tests/*.test.hcl"], parallel = 2, timeout = "5m" }
  reporting      = { json = true, junit = "reports/${each.key}.xml" }
}

environment "production" {
  classification = "production"
  target         = "env://AUTOSQL_EXAMPLE_PRODUCTION_TARGET"
  schema_sources = [{ kind = "hcl", path = "schema.hcl" }]
  migration_dir  = "migrations"
  timeout        = "30s"
}
