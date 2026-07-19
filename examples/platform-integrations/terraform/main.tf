terraform {
  required_providers {
    autosql = { source = "stigenai/autosql", version = "~> 0.1" }
  }
}

provider "autosql" {
  apply_config_ref = "file:///run/secrets/autosql/apply.json"
}

resource "autosql_schema" "orders" {
  id              = "orders-production"
  source_ref      = "file:///workspace/orders.artifact.json"
  artifact_digest = "sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
  policy_digest   = "sha256:bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
  target_snapshot = "sha256:cccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccccc"
  target_id       = "orders-primary"
  environment     = "production"
  connection_ref  = "env://AUTOSQL_DATABASE_URL"
  approval_ref    = "file:///workspace/orders.approval.json"
  approval_digest = "sha256:dddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddddd"
}
