---
page_title: "autosql_migration Resource"
description: |-
  Apply an immutable versioned AutoSQL migration through Terraform or OpenTofu.
---

# autosql_migration

`autosql_migration` has the same digest, approval, target, import, refresh, and
destroy guarantees as `autosql_schema`, but declares the versioned-migration
workflow. AutoSQL remains the migration executor; the provider does not
duplicate planning or database mutation logic.

Required arguments are `id`, `source_ref`, `artifact_digest`, `policy_digest`,
`target_snapshot`, `target_id`, `environment`, `connection_ref`,
`approval_ref`, and `approval_digest`. Optional destroy fields must be supplied
as a complete approved-artifact set.

See [`docs/terraform-provider.md`](../terraform-provider.md) for examples.
