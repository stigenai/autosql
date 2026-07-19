# Terraform and OpenTofu provider

The first-party Terraform Plugin Framework provider exposes `autosql_schema`
and `autosql_migration` and delegates mutation to the released AutoSQL CLI.

```hcl
terraform {
  required_providers {
    autosql = { source = "stigenai/autosql", version = "~> 0.1" }
  }
}

provider "autosql" {
  apply_config_ref = "file:///run/secrets/autosql/apply.json"
}

resource "autosql_schema" "production" {
  id              = "orders-production"
  source_ref      = "file://${path.module}/orders.artifact.json"
  artifact_digest = "sha256:..."
  policy_digest   = "sha256:..."
  target_snapshot = "sha256:..."
  target_id       = "orders-primary"
  environment     = "production"
  connection_ref  = "env://AUTOSQL_DATABASE_URL"
  approval_ref    = "file://${path.module}/orders.approval.json"
  approval_digest = "sha256:..."
}
```

The provider hashes artifact and approval evidence before starting the CLI.
Resolved database URLs are rejected; state contains only opaque references.
Create and update run `autosql apply --artifact ... --no-edits`. Read verifies
local material without contacting the database or resolving a credential.

Removal forgets the Terraform ownership record by default. A database rollback
requires all four `destroy_source_ref`, `destroy_artifact_digest`,
`destroy_approval_ref`, and `destroy_approval_digest` fields. Partial
destructive authorization fails closed.

Import accepts a JSON object containing non-secret attributes and opaque
references. Release archives follow Terraform Registry naming for Linux,
macOS, and Windows on amd64 and arm64. Checksums are signed with the provider
publishing key. Terraform and OpenTofu versions released in the prior 24 months
are supported.
