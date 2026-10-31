version     = 2
environment = "local"

dev_factory "local_container" {
  mode             = "container"
  endpoint         = "unix:///var/run/docker.sock"
  allowlist        = ["unix:///var/run/docker.sock"]
  postgres_version = 17
  image            = "postgres@sha256:eee6c6e1b0894fa4fe0075eb054af9fd1cf39a35321c2d4110626cfe506411fb"
  isolation        = "database"
  startup_timeout  = "2m"
  case_timeout     = "5m"
  cleanup_timeout  = "10s"
}

environment "local" {
  classification = "development"
  target         = "env://AUTOSQL_EXAMPLE_LOCAL_TARGET"
  dev_factory    = "local_container"
  schema_sources = [{ kind = "hcl", path = "schema.hcl" }]
  migration_dir  = "migrations"
  tests          = { files = ["tests/*.test.hcl"], parallel = 2, timeout = "5m" }
}

environment "regression" {
  classification = "development"
  target         = "env://AUTOSQL_EXAMPLE_LOCAL_TARGET"
  dev_factory    = "local_container"
  schema_sources = [{ kind = "hcl", path = "schema.hcl" }]
  migration_dir  = "migrations"
  tests          = { files = ["failures/*.test.hcl"], parallel = 1, timeout = "5m" }
}

environment "production" {
  classification = "production"
  target         = "env://AUTOSQL_EXAMPLE_PRODUCTION_TARGET"
  schema_sources = [{ kind = "hcl", path = "schema.hcl" }]
  migration_dir  = "migrations"
}
