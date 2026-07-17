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

Fresh PostgreSQL targets add a signed orchestration layer described in
[PostgreSQL database target bootstrap](postgresql-database-bootstrap.md). It
places managed creation or external verification in a server-scoped prohibited
phase, then binds the complete schema plan into scope- and resource-aware
phases with stable checkpoints. Access grants are an explicit final handoff,
and dependency edges reverse for teardown.

## PostgreSQL rendering

The PostgreSQL driver advertises managed lifecycle support for schemas, tables,
columns, views, and materialized views. Capabilities list the exact operations
and supported semantic features for each kind; all other inspected kinds remain
read-only. Schemas support create, drop, and rename. The table subset is limited
to permanent, non-partitioned tables plus row-level-security state and managed
child columns. Core columns support default, nullability, canonical relative
ordinal metadata, and only a narrow set of known implicit or assignment-safe
type casts; other conversions fail planning because no validated `USING`
expression is available. Create, alter, drop, and rename are available for tables,
columns, views, and materialized views within those feature subsets.
Native core types use a strict canonical allowlist rather than arbitrary SQL
fragments. Length/precision variants, `ARRAY` keyword spelling, collation,
constraints, and embedded clauses are rejected. Defaults are modeled per type:
simple numeric, boolean, quoted text, and canonical current-timestamp values;
casts, functions, array defaults, and other expressions fail closed.
Fixed-width integer defaults are canonical base-10 values with exact PostgreSQL
range checks; catalog-quoted negative integer casts normalize back to that form.
Leading zeros and out-of-range values fail, and real/double/numeric defaults stay
unsupported until their catalog round trip is modeled precisely.
Column ordinals describe canonical relative order. PostgreSQL can physically
append and drop columns but cannot insert or reorder them, so the planner proves
the complete sibling sequence: append and drop consequences are topology-only,
while middle insertion, reorder, or a simultaneous ordinal-and-attribute change
fails until a table rebuild strategy exists.
Column rename hints preserve the same physical slot (including nonfinal columns)
and cannot be combined with hidden attribute changes.
Column drop and rename also fail when a retained read-only table child or
table-referencing object might depend on the affected column. This deliberately
conservative guard remains until inspection can prove exact column-level edges
and complete dependent transitions.
The same principle applies to every managed renameable container—schema, table,
view, and materialized view: a retained index, constraint, trigger, view, or
other opaque descendant/dependent blocks the parent rename because PostgreSQL
may rewrite its identity or stored definition. Bare managed parents and their
canonical column topology continue to use proven rename transitions.

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
validated. Inspected complex queries may instead use their captured projection
graph when the definition parses as exactly one query and every projection is
contiguous, uniquely named, typed, and canonical. Shape-changing view alters
require an explicit proven rebuild; independent projection-child changes fail
closed.

Reference and type-use dependencies are part of rendered semantics, not advisory
metadata. For managed views, the renderer walks PostgreSQL's parsed query tree,
including joins, CTE bodies and nested `SELECT`/`EXISTS`, resolves each relation
against the desired graph, and requires exact equality with the declared
`references` set. Missing, extra, ambiguous, duplicate, self, or unsupported
catalog-qualified relations fail planning so post-apply inspection cannot
silently change the target graph. Column `uses` edges remain bound to the
conservative type grammar.
Automatic projection inference from authored SQL remains limited to direct
column projections with no `WHERE`, functions, casts, or trailing clauses;
proven integer/text literal views remain dependency-free, while object-bearing
casts such as `regclass` fail closed. Inspected complex definitions do not use
that inference path: their canonical projection resources and exact parsed
relation graph are validated directly. In the simple inference path, qualified
columns must use the sole relation's name, and `*` is valid only as the entire
projection list; foreign qualifiers and mixed `*, column` lists fail.
For user-defined column types, inspection follows array element OIDs and emits a
`uses` edge to the true enum/domain/composite resource. Validation resolves
canonical qualified, same-schema/public unqualified, quoted, and array spellings
against that exact dependency target.
Rendered UDT SQL always uses the dependency target's quoted schema and type name,
never the table schema or session search path. PostgreSQL's one-or-more array
brackets are canonicalized to a single `[]` before fingerprinting and rendering.
Normalization also rewrites accepted UDT aliases from the exact `uses` target to
the inspector spelling: public types are unqualified, nonpublic types qualified,
and mixed-case identifiers correctly quoted.
That rewrite occurs only after the original type spelling is proven to name the
exact dependency target while preserving scalar/array form; builtin types with
UDT edges and unrelated or ambiguous type edges are rejected rather than healed.

`postgres.RenderDocument` renders a full desired document from an empty schema
projection when every resource is in the managed matrix. Read-only kinds make
the operation fail closed. Rendering never executes SQL and returns no partial
statement list when any resource or transition is unsupported.

Call `postgres.PreflightProvisioning` before fresh creation to receive one
stable, value-redacted inventory of every unsupported managed annotation, spec
field, dependency, operation, and external/read-only prerequisite. This avoids
the first-error loop when evaluating a large inspected schema for provisioning.

The canonical `comment` annotation is managed metadata. Fresh plans emit typed
`COMMENT ON` statements after object creation, while comment-only plans support
add, change, and removal (`IS NULL`). Unknown annotations and extension fields
remain fail-closed.

PostgreSQL identity columns are supported during creation. Authored `always`
and `by_default` values normalize to the inspected `a` and `d` forms and render
as `GENERATED ALWAYS AS IDENTITY` and `GENERATED BY DEFAULT AS IDENTITY`.
Identity alteration remains unsupported, and identity cannot be combined with
a default or stored-generated expression.

Application- and extension-owned routines referenced by stored generated
columns are explicit external prerequisites; AutoSQL does not render inspected
routine bodies. `postgres.GeneratedRoutinePrerequisites` produces the stable
fingerprinted manifest, and `postgres.VerifyGeneratedRoutinePrerequisites`
checks a target for exact matches with `satisfied`, `missing`, or
`version_mismatch` status before planning. Generated-column changes are never
ordered before their declared routine prerequisites.

Stored generated columns are creation-only and use a dedicated bounded
PostgreSQL expression grammar. The renderer accepts one structurally parsed
expression containing literals, exact source-column references, core casts,
arrays, and calls to exact verified immutable SQL invoker-security routines.
It rejects subqueries, statement or comment syntax, operators outside that
grammar, SQL value/volatile/unmodeled functions, ambiguous or undeclared
identifiers, and every generated-column alteration without emitting SQL.
