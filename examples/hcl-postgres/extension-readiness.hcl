database "extension_readiness_demo" {
  mode = "managed"

  endpoint = {
    host     = "postgres.internal"
    port     = 5432
    tls_mode = "verify-full"
  }

  maintenance_database = "postgres"
  owner                = "extension_readiness_owner"
  encoding             = "UTF8"
  template             = "template0"
}

schema "app" {}

extension "hstore" {
  schema  = "app"
  version = "1.8"
}

extension "pgcrypto" {
  schema  = "app"
  version = "1.3"
}
