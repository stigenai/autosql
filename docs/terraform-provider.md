# Terraform / OpenTofu provider contract

`pkg/integrations/terraform` is the protocol-neutral core for an AutoSQL
Terraform or OpenTofu provider. A provider adapter can map these types to the
Terraform Plugin Framework without changing the safety contract.

The `autosql_schema` and `autosql_migration` resources carry only opaque source,
artifact, policy, target-snapshot, and connection references. Resolved database
URLs and passwords are rejected and never serialized into state. The sensitive
`connection_ref` attribute is still only a reference; it is not a credential.

Plans are immutable bindings of:

- the AutoSQL artifact digest,
- the policy/rule-pack digest, and
- the target snapshot digest.

Apply requires an exact approved plan digest, acquires a per-resource state
lock, and delegates execution to the same deployment contract used by CLI and
fleet workflows. Destroy maps to an explicitly approved destructive action.

Import and refresh inspect live state while preserving the caller’s opaque
connection reference. The provider never resolves a secret while constructing
Terraform state. Offline plan validation therefore works in CI without a
database connection; acceptance tests can inject a live inspector and runner.
