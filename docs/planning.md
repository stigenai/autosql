# Deterministic migration planning

`pkg/plan` turns two validated, normalized schema graphs into a tamper-evident
execution plan. A plan binds the planner and driver versions, source and target
semantic fingerprints, canonical changes, SQL steps, transaction requirements,
lock and impact metadata, dependency edges, phases, and a digest. Planning is
all-or-nothing: an invalid graph or unsupported transition returns a zero plan.
Executable steps contain SQL. Topology steps bind database-derived descendants
to a parent transition, contain zero SQL, and are excluded from plugin, safety,
precheck, and apply statement lists.

Plans do not provide an apply function. `Plan.SafetyStatements` exposes exact
SQL/change bindings for the existing safety, policy, approval, precheck, and
guardrail path. Transaction-prohibited phases must be handled explicitly by a
guarded executor; they are never silently moved into a transaction.

## PostgreSQL rendering

The PostgreSQL driver advertises managed lifecycle support for schemas, tables,
columns, views, and materialized views. Capabilities list the exact operations
and supported semantic features for each kind; all other inspected kinds remain
read-only. Schemas support create, drop, and rename. The table subset is limited
to permanent, non-partitioned tables plus row-level-security state and managed
child columns. Core columns support type, default, nullability, and canonical
ordinal metadata. Create, alter, drop, and rename are available for tables,
columns, views, and materialized views within those feature subsets.

View definitions are replaceable, while materialized-view changes require the
explicit `allow_rebuild=true` strategy. A shape-changing view replacement also
requires explicit rebuild. Before either rebuild, every non-projection dependent
must have a complete managed drop transition; unchanged indexes, triggers,
grants, or dependent views fail planning. Concurrent/nontransactional rendering
is rejected until a phase-aware guarded executor is available.
View and materialized-view output columns remain canonical resources. The SQL
source normalizer derives them only for a conservative round-trip subset:
simple projections, expanded wildcards over known tables, and aliased integer
or text literals. Output name, type, nullability, dependency, and ordinal are
validated. Shape-changing view alters require an explicit proven rebuild;
independent projection-child changes fail closed. Other definitions fail closed
instead of planning drift.

`postgres.RenderDocument` renders a full desired document from an empty schema
projection when every resource is in the managed matrix. Read-only kinds make
the operation fail closed. Rendering never executes SQL and returns no partial
statement list when any resource or transition is unsupported.
