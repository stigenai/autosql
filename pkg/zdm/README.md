# Zero-downtime metadata

`zdm.Store` owns a reserved PostgreSQL schema (default `autosql_zdm`) that is
separate from application schemas and the ordinary migration revision store.
Initialization is an explicit, repeatable operation. It serializes concurrent
callers with a transaction-scoped advisory lock and applies supported metadata
upgrades atomically. A newer version or a malformed layout is refused; operators
must use a compatible AutoSQL binary or restore the control schema from audited
backup rather than editing it manually.

The connecting role needs `CONNECT` plus `CREATE` on the database to initialize
the control schema. Baseline additionally needs `USAGE` on selected application
schemas and catalog visibility for their objects. It does not need superuser,
`CREATEDB`, or `CREATEROLE`, and never executes captured application DDL.

An adoption baseline requires a stable target identity, environment, operator,
unique ID, and an explicit list of application schemas. It inspects those schemas
inside the same serializable transaction that records the canonical document,
semantic fingerprint, identity binding, and audit event. Retry returns the same
evidence only while the live fingerprint and stored canonical evidence still
agree. Active operations, recovery state, stale identity, drift, or tampering are
actionable conflicts and produce no partial baseline row.

CLI entry points are `migrate metadata-init`, `migrate metadata-status`, and
`migrate metadata-baseline`. Database URLs must be secret references and are
never included in human or JSON results.
