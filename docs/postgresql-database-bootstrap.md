# PostgreSQL database target bootstrap

AutoSQL treats the PostgreSQL server, maintenance database, target database,
and in-database schema as distinct prerequisites. A database target contains
only non-secret identity and settings; its runtime maintenance URL is supplied
through a secret reference.

`mode = "managed"` requires `CREATE DATABASE` authority. AutoSQL validates the
endpoint, maintenance database, owner, template, tablespace, server version,
and name collision before issuing SQL. `CREATE DATABASE` runs outside a
transaction, then AutoSQL reconnects directly to the new target. Encoding,
collation, character type, locale provider, ICU locale, and template are
creation-time settings: changing them requires a new database and reviewed data
migration. Owner, tablespace, connection limit, and allow-connections drift is
reported separately.

`mode = "external"` never creates a database. It requires the database to
exist and verifies the declared owner, encoding, locale, tablespace, connection
limit, and connection policy before schema planning. A missing database or an
immutable-setting mismatch fails before schema SQL.

## Least-privilege runtime identities

Planning and HCL loading require no credentials. Runtime authority is split so
one secret does not need to cross every boundary:

- A managed-target maintenance identity needs `CREATEDB`; it also needs
  `CREATEROLE` when the plan creates roles or memberships. The declared owner,
  template, and tablespace must already exist. An external-target identity
  needs `CONNECT` to the target and the object privileges required by its plan,
  but never receives database-creation authority from AutoSQL.
- Extension packages and exact versions must already be installed on the
  server. `CREATE` on the target database is sufficient only for trusted
  extensions; PostgreSQL may require a superuser for an untrusted package.
  AutoSQL's extension allowlist and exact version/schema policy are additional
  checks, not a privilege escalation mechanism.
- Schema DDL should run as the intended object owner or a narrowly privileged
  bootstrap role. Role retirement and ownership transfer require the matching
  PostgreSQL role administration authority. Exact ACL provenance uses
  transaction-local `SET ROLE` to the declared grantor, so the runtime identity
  must be allowed to assume every managed grantor; AutoSQL resets the role
  before the phase commits. Passwords, OIDC tokens, and AWS
  IRSA-derived credentials are resolved at runtime from references and never
  enter HCL, plans, checkpoints, status, or diagnostics.

For the strongest separation, create the database and cluster roles with a
short-lived bootstrap identity, execute in-database objects as the declared
owner, perform the final grants/default privileges last, then retire the
bootstrap access. The whole-database plan's server/database scopes and final
access-handoff barrier make that split auditable.

## Whole-database plans

`postgres.PlanDatabaseBootstrap` produces one canonical, credential-free plan
covering database preparation and every in-database resource. The first step
is always a server-scoped, transaction-prohibited `prepare_database` action;
managed mode creates the target and external mode verifies it. All later steps
depend on that boundary and retain the exact IDs, dependencies, transaction
mode, locks, impacts, SQL, and digest from the nested schema plan.

Steps are grouped into deterministic stages for database target, roles,
namespaces, extensions, types, routines, storage, constraints, indexes,
triggers/policies, and access handoff. A phase splits whenever its stage,
scope, or transaction mode changes. Every phase has a digest-derived stable
checkpoint, allowing an executor to resume without treating a concurrent
index or privileged server operation as transactional. Runtime URLs and
credentials are not accepted by the planner and cannot enter the artifact.

The dependency graph, rather than resource array order, controls execution.
Extension and type dependencies precede signatures and columns; generated
expression routines precede their consumers; referenced primary/unique keys
precede foreign keys; mutual foreign keys are added only after both tables
exist; and grants, memberships, and default privileges form the final access
handoff. Teardown reverses these edges, revoking access and removing dependent
objects first. `PlanDatabaseTransition` exposes the same contract for upgrades
and reviewed teardown plans.

## Resumable execution and repair

`postgres.ExecuteDatabaseBootstrapURL` applies the whole-database plan. It
creates an `autosql_internal` ledger inside the target; this namespace is
reserved and excluded from PostgreSQL inspection so execution evidence cannot
appear as application drift. The ledger stores plan/target/phase/step hashes,
states, and timestamps only—never connection strings, SQL, routine bodies, or
secret values.

Transactional phases commit their DDL and confirmed step records together. An
injected error rolls both back, and the next invocation resumes at that phase.
For transaction-prohibited work such as `CREATE INDEX CONCURRENTLY`, the
executor durably records intent first. An interrupted or ambiguous step stops
with `ErrBootstrapReconcile`; `DiagnoseDatabaseBootstrapURL` returns the bound
step ID and repair guidance without SQL. After an operator verifies the live
postcondition, `ConfirmBootstrapStepURL` records that exact digest-bound step
and the same plan resumes. A different plan, schema digest, target settings, or
untracked managed-name collision fails before schema SQL.

`AbortDatabaseBootstrapURL` is explicit cleanup. A managed target requires the
caller to authorize database deletion; AutoSQL drops the database and then
removes only cluster roles and memberships whose create steps were confirmed
by this bootstrap. An external target keeps all user objects and removes only
the matching execution ledger. Databases declared with connections disabled
are temporarily reopened for a resume/abort session and restored afterward.

For an upgrade, inspect the converged database, author the next desired HCL,
and call `PlanDatabaseTransition`; the executor accepts the new digest only
after every earlier plan for that target is complete. A failed transactional
phase rolls back and can be retried directly. An ambiguous non-transactional
step must be diagnosed, inspected in PostgreSQL, and explicitly confirmed or
repaired before resume. Never edit a serialized plan or ledger row: either
resume the bound digest, explicitly abort it, or publish a new plan after the
previous one reaches a terminal state.

The complete HCL contract is in
[`database-bootstrap.hcl`](../examples/hcl-postgres/database-bootstrap.hcl).
To create or verify its target:

```bash
autosql database prepare \
  --target-hcl examples/hcl-postgres/database-bootstrap.hcl \
  --maintenance-url env://AUTOSQL_MAINTENANCE_DATABASE_URL
```

To plan and execute a complete HCL graph through the resumable executor, place
the database block and desired resources in one file:

```bash
autosql database bootstrap \
  --file complete-bootstrap.hcl \
  --maintenance-url env://AUTOSQL_MAINTENANCE_DATABASE_URL \
  --extension-allowlist hstore,pgcrypto \
  --reviewed-routine-digest sha256:REVIEWED_DIGEST
```

Repeat `--reviewed-routine-digest` for each approved routine body. The command
enables concurrent standalone indexes by default and emits only database,
digest, checkpoint, and step identifiers. A retry uses the ledger in the
target and follows the same reconcile rules as the library executor.

The command output includes only the target name, mode, endpoint, and whether
creation occurred. The resolved URL and credentials are redacted. Kubernetes
uses the same `databaseTarget` object plus a `maintenanceDatabaseURL` Secret
reference and `bootstrapAuthority`; managed mode requires both.

Database rename is supported only from a maintenance connection and fails on
name collisions or active sessions. Drop is an explicit API operation; forced
drop must be selected by the caller. A managed target with
`allowConnections: false` is created connectable, opened for bootstrap, then
closed to new sessions while the bootstrap session remains valid.

`TestManagedAndExternalDatabaseTargetLifecycle` exercises managed create,
reconnect, collision handling, external verification, immutable drift, rename,
allow-connections handoff, and drop on PostgreSQL 14–18.
`TestCanonicalCompleteBootstrapInventoryManifest` builds the complete supplied
inventory (including 315 indexes and 197 constraints), then proves byte-stable,
cycle-free phases, a final access handoff, and 315 non-transactional online
index steps against a real PostgreSQL database. It then provisions that graph
into a new database, injects one interruption before every execution phase,
resumes each checkpoint, and verifies exact final catalog parity. Routine
postconditions compare PostgreSQL parse fingerprints so harmless catalog
whitespace reformatting does not weaken the reviewed source-body digest.
The PostgreSQL 14–18 matrix runs this proof plus real native CLI and Kubernetes
operator reconciliation paths against disposable newly created databases.

## Complete-cell scale and lock budgets

The canonical count-bearing cell is guarded by deterministic structural
budgets rather than flaky wall-clock assertions. It may contain at most 1,250
managed resources, produce at most 4 MiB of HCL and SQL respectively, produce
an 8 MiB canonical whole-database plan, and schedule at most 2,000 steps in
1,000 phases. No step may have more than 1,200 direct dependencies; the large
fanout is deliberate on the final access-handoff barrier, which must follow
every construction step. The test
logs the actual resource, byte, step, phase, dependency, lock, transaction, and
scan totals, and generates the plan twice to require byte-for-byte equality.

Lock exposure is part of the plan artifact. Each of the 315 standalone indexes
must be represented as a transaction-prohibited, share-lock, scan-impact step
when concurrent indexes are enabled. Constraint validation and any other scan
or exclusive-lock work remains visible through each schema step's `lock` and
`impact` fields; AutoSQL does not collapse these into an optimistic aggregate.
The access handoff remains after every storage, constraint, index, and behavior
phase, so no grant shortens the protected construction window accidentally.

`BenchmarkCompleteBootstrapPipeline` separately reports time and allocations
for inspection, normalization, HCL format/load, preflight plus diff/render/
scheduling, and canonical plan serialization. CI records three iterations on
PostgreSQL 16 as a downloadable benchmark artifact. These measurements reveal
material CPU or allocation regressions while the structural budgets enforce
portable correctness across PostgreSQL 14–18.
