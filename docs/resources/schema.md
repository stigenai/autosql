---
page_title: "autosql_schema Resource"
description: |-
  Reconcile a declarative PostgreSQL schema from an immutable AutoSQL artifact.
---

# autosql_schema

`autosql_schema` applies a declarative AutoSQL artifact after independently
verifying the artifact and approval bytes. Updates repeat the same guarded
operation. Import accepts the documented non-secret JSON identity. Destroy
forgets Terraform state unless a separate signed and approved destroy artifact
is configured.

Required arguments are `id`, `source_ref`, `artifact_digest`, `policy_digest`,
`target_snapshot`, `target_id`, `environment`, `connection_ref`,
`approval_ref`, and `approval_digest`. `connection_ref` and `approval_ref` are
sensitive opaque references.

See the complete example and lifecycle behavior in
[`docs/terraform-provider.md`](../terraform-provider.md).
