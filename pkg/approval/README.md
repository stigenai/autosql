# Apply approval gate

`Gate` enforces environment allow-lists and risk-tiered approval requirements.
Plans may have an apply deadline. Approvals must match the immutable plan digest
and environment and be current. Identity, proof, roles, and emergency authority
come exclusively from an `IdentityAuthority`; approval payloads cannot assert
their own roles. Plan authors, apply requesters, and approvers are separated.

Every decision is persisted before mutation through a `Chain` backed by a
`DurableSink`. Records include sequence and previous-record hashes. `FileSink`
validates the complete chain, appends JSON lines, and synchronizes file and
directory storage before success. Persistence, context, or final expiry failure
is fail-closed. `GuardedApply` rechecks plan and approval expiry after audit and
immediately before calling the mutation adapter.
