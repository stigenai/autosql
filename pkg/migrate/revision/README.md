# PostgreSQL revision store

The revision store is an independent, versioned PostgreSQL schema. `Init` runs
numbered internal migrations in one serializable transaction while holding a
transaction-scoped advisory lock. Schema, table, and executor-history placement
is configurable through validated lowercase identifiers and always rendered as
quoted identifiers. A failed migration rolls the entire initialization back.

`revisions` binds each migration version to its description, kind, source file,
verified manifest, artifact, plan, checks, and guardrail bundle. It preserves
the explicit lifecycle state, statement ordinal, attempt, redacted error,
operator, timestamps, durable execution duration, manifest generation, from/to
versions, superseded revisions, and reversal link. `statement_attempts` and
`events` retain immutable per-attempt evidence. Internal schema v3 adds duration
and generation without rewriting v0/v1/v2 revision history.

`Store.Status` is deliberately read-only and executes every database read in a
single pinned PostgreSQL read-only transaction. Callers must pass a manifest already
verified by `pkg/migrate`. Status reports stored lifecycle states exactly as
recorded; confirmed statements cannot silently reinterpret a partial revision.
Unknown revisions, manifest binding drift, and incomplete executor intentions
are reported with remediation guidance.
