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

  # Execution resolves these at runtime. The HCL graph contains neither the
  # signed manifest bytes nor public/private key material.
  bootstrap_authorization = {
    manifest   = "file:///run/autosql/bootstrap-authorization.json"
    public_key = "env://AUTOSQL_BOOTSTRAP_AUTH_PUBLIC_KEY"
    issuer     = "security"
    signer     = "dba-reviewers"
    purpose    = "bootstrap-authorization"
  }
}
