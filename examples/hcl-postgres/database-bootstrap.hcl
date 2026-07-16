database "autosql_cell" {
  mode = "managed"

  endpoint = {
    host     = "postgres.internal"
    port     = 5432
    tls_mode = "verify-full"
  }

  maintenance_database = "postgres"
  owner                = "autosql_cell_owner"
  encoding             = "UTF8"
  locale_provider      = "libc"
  collation            = "C"
  character_type       = "C"
  template              = "template0"
  tablespace            = "pg_default"
  connection_limit      = 50
  allow_connections     = true
}
