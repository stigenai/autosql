# Deterministic migration planning

`pkg/plan` turns two validated, normalized schema graphs into a tamper-evident
execution plan. A plan binds the planner and driver versions, source and target
semantic fingerprints, canonical changes, SQL steps, transaction requirements,
lock and impact metadata, dependency edges, phases, and a digest. Planning is
all-or-nothing: an invalid graph or unsupported transition returns a zero plan.

Plans do not provide an apply function. `Plan.SafetyStatements` exposes exact
SQL/change bindings for the existing safety, policy, approval, precheck, and
guardrail path. Transaction-prohibited phases must be handled explicitly by a
guarded executor; they are never silently moved into a transaction.

## PostgreSQL rendering

The PostgreSQL driver manages schemas, extensions, enums, domains, composite
types, sequences, tables, columns, primary/unique/check/foreign-key constraints,
indexes, views, and materialized views. It quotes identifiers and rejects
transitions it cannot express safely. Column type/default/nullability changes,
append-only enum changes, and replaceable view definitions are supported.
Index alteration is an explicit drop/create strategy and requires
`allow_rebuild=true`. Concurrent index operations use
`concurrent_indexes=true` and are marked transaction-prohibited.

`postgres.RenderDocument` renders a full desired document from an empty schema
projection. Rendering never executes SQL and returns no partial statement list
when any resource or transition is unsupported.
