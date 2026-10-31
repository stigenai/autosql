version     = 2
environment = "local"

dev_factory "local_admin" {
  mode                 = "existing_admin"
  admin                = "env://AUTOSQL_EXAMPLE_LOCAL_ADMIN"
  admin_classification = "development"
  postgres_version     = 17
  allowlist            = ["postgres://127.0.0.1:55432/postgres"]
  isolation            = "database"
  startup_timeout      = "2m"
  case_timeout         = "5m"
  cleanup_timeout      = "10s"
}

environment "local" {
  classification = "development"
  target         = "env://AUTOSQL_EXAMPLE_LOCAL_TARGET"
  dev_factory    = "local_admin"
  schema_sources = [{ kind = "hcl", path = "schema.hcl" }]
  migration_dir  = "migrations"
  tests          = { files = ["tests/*.test.hcl"], parallel = 1, timeout = "5m" }
}

environment "production" {
  classification = "production"
  target         = "env://AUTOSQL_EXAMPLE_PRODUCTION_TARGET"
  schema_sources = [{ kind = "hcl", path = "schema.hcl" }]
  migration_dir  = "migrations"
}
