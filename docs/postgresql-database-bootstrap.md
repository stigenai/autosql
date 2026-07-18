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
Extension and type dependencies precede signatures, columns, CHECK
constraints, and partial-index predicates; generated expression routines
precede their consumers; referenced primary/unique keys precede foreign keys;
mutual foreign keys are added only after both tables exist; and grants,
memberships, and default privileges form the final access handoff. Teardown
reverses these edges, revoking access and removing dependent objects first.
`PlanDatabaseTransition` exposes the same contract for upgrades and reviewed
teardown plans.

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
executor durably records intent first. On resume, an absent concurrent index is
safe to retry, while an exact valid and ready catalog match is safe to confirm;
the executor performs those two bounded reconciliations automatically. An
invalid, different, or otherwise ambiguous remnant stops with
`ErrBootstrapReconcile`; `DiagnoseDatabaseBootstrapURL` returns the bound step
ID and repair guidance without SQL. After an operator verifies an ambiguous
live postcondition, `ConfirmBootstrapStepURL` records that exact digest-bound
step and the same plan resumes. A different plan, schema digest, target
settings, or untracked managed-name collision fails before schema SQL.

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

Before authorizing execution, compare every requested extension with the live
server without changing either the maintenance or target database:

```bash
autosql database bootstrap preflight \
  --file complete-bootstrap.hcl \
  --maintenance-url env://AUTOSQL_MAINTENANCE_DATABASE_URL \
  --extension-allowlist hstore,pgcrypto \
  --extension-version hstore=1.8 \
  --extension-version pgcrypto=1.3 \
  --extension-schema hstore=app \
  --extension-schema pgcrypto=app \
  --json
```

The versioned structured report classifies every extension as `ready`,
`missing_package_control_file`, `unavailable_requested_version`,
`schema_conflicted`, `privilege_blocked`, or `unauthorized`, with exact
remediation. Text output contains the same status and remediation. Preflight
reads PostgreSQL's available-version, installed-extension, namespace,
update-path, and privilege catalogs; it never invokes a package manager or
installs a PostgreSQL control file. A missing package must be installed on the
server by its operator before retrying.

PostgreSQL's control metadata is interpreted using the connected server's
major version. A trusted extension may be installed by a non-superuser that
has the required database and schema privileges; an untrusted extension that
declares `superuser` remains privilege-blocked unless the executing role is a
superuser. AutoSQL's allowlist, exact-version, schema, and untrusted-extension
authorization are independent gates. Execution repeats this read-only check
before creating the target database, opening the bootstrap ledger, or applying
any schema mutation, so a readiness failure leaves no partial bootstrap.
Authorization is rebound to the exact in-memory plan as authenticated,
non-serializable capabilities; extension metadata in HCL cannot substitute for
legacy `allow_untrusted_extensions` authority or a verified signed manifest.
Signed-manifest authority is scoped to each exact extension resource ID, so an
untrusted approval for one extension cannot authorize another whose signed
metadata disagrees with the live control file. The legacy flag remains an
explicit global authorization, but is bound to one exact plan digest.

Privilege diagnosis follows the operation PostgreSQL will execute. A new
extension checks database and target-schema CREATE plus trusted/superuser
rules. An update checks the advertised version path and extension ownership,
without incorrectly requiring database CREATE. A schema move checks
relocatability, extension ownership, CREATE on the already-existing destination
schema, and ownership of relocatable member objects. Trusted packages can
create bootstrap-superuser-owned members (notably hstore on PostgreSQL 18), so
an extension owner may still need a superuser for relocation. An exact
installed no-op requires none of these mutation privileges.
Ownership includes PostgreSQL role membership (`pg_has_role`), not only an
exact username match. Preflight checks every known owner-bearing extension
member class through that effective membership and conservatively blocks a
relocation if it encounters a member class whose ownership semantics are not
known. A member may assume the owning role for the resulting ALTER operations.

```bash
autosql database bootstrap prepare \
  --file complete-bootstrap.hcl \
  --json
```

This credential-free preflight reports every routine source digest and every
extension allowlist, exact-version, target-schema, dependency, and server
package requirement in one deterministic inventory. It also identifies every
additional routine gate required by unsafe languages, privileged operations,
or procedure transaction control, rather than failing on those gates one at a
time. Extensions whose control metadata is not trusted carry an explicit
`untrusted_extension_authorization_required` gate, separate from whether the
PostgreSQL control file requires superuser. Prepare temporarily satisfies this
gate only to compute the canonical plan; execution remains fail-closed until
the operator supplies explicit authorization. The public prepare API returns
only the inventory: its plan and schema-plan digests plus step/phase counts are
non-executable review summaries. The synthetically authorized plan is discarded
internally and cannot be passed to `ExecuteDatabaseBootstrapURL`; execution
must build a new plan from explicit authority. The inventory is bound to the
canonical bootstrap plan digest. It omits
routine definitions by default; use `--include-routine-source` only with
`--json` or `--hcl` for an explicit, machine-verifiable source-review workflow.
Use `--hcl` instead of `--json` to produce a canonical, reviewable HCL
inventory suitable for version control. Emitted definitions are byte-bound to
their `source_digest`; AutoSQL never applies broad text redaction that would
silently change reviewed SQL.

After review, sign the complete inventory once with an Ed25519 key. The private
key is read only from a secret reference and is never written to the manifest:

```bash
autosql database bootstrap authorize \
  --file complete-bootstrap.hcl \
  --authorization-signing-key env://AUTOSQL_BOOTSTRAP_AUTH_PRIVATE_KEY \
  --authorization-key-id production-dba-2026 \
  --authorization-issuer security \
  --authorization-signer dba-reviewers \
  --authorization-purpose bootstrap-authorization \
  --valid-for 1h \
  --output bootstrap-authorization.json
```

The versioned manifest contains no SQL, routine source, executable plan, or
credential. It binds the canonical plan, schema-plan, and source digests;
signer identity and purpose; validity window; every exact routine digest and
additional routine gate; and every exact extension name, version, schema,
dependency, trust, and authority constraint. Unknown, missing, extra, stale,
not-yet-valid, expired, or overbroad entries fail verification.

Supply the manifest and its independently trusted public key to execution:

```bash
autosql database bootstrap \
  --file complete-bootstrap.hcl \
  --maintenance-url env://AUTOSQL_MAINTENANCE_DATABASE_URL \
  --authorization-manifest bootstrap-authorization.json \
  --authorization-public-key env://AUTOSQL_BOOTSTRAP_AUTH_PUBLIC_KEY \
  --authorization-issuer security \
  --authorization-signer dba-reviewers \
  --authorization-purpose bootstrap-authorization
```

Manifest verification and exact source/plan rebinding finish before the
maintenance URL is resolved and before any mutation. Only
`PlanDatabaseBootstrapAuthorized` can bridge the opaque verified token into an
executable plan; it materializes the gates internally and rechecks both plan
digests, so the token cannot authorize a different target or source graph. An
unverified or zero-value token cannot produce a plan. The repeatable
`--reviewed-routine-digest` and `--extension-allowlist` flags remain available
as a separate compatibility path and cannot be combined with a manifest.

The same policy may be declared directly in database HCL without embedding an
authorization artifact or signing key in the graph:

```hcl
database "cell" {
  # target fields omitted
  bootstrap_authorization = {
    manifest   = "file:///run/autosql/bootstrap-authorization.json"
    public_key = "env://AUTOSQL_BOOTSTRAP_AUTH_PUBLIC_KEY"
    issuer     = "security"
    signer     = "dba-reviewers"
    purpose    = "bootstrap-authorization"
  }
}
```

Only `env://` and `file://` runtime references are accepted. Unknown fields,
including private signing keys or credentials, are rejected. Explicit CLI
manifest flags and the HCL block are mutually exclusive, keeping the effective
policy unambiguous. Both routes use the same canonical manifest parser,
verifier, and opaque plan-bound authorization token.

Manifest mode is also mutually exclusive with every legacy authorization
input: `--reviewed-routine-digest`, `--extension-allowlist`,
`--extension-version`, and `--extension-schema`. AutoSQL determines the
effective direct-or-HCL manifest mode first and rejects a mixed invocation
before resolving a maintenance URL, public key, manifest, or database
credential. The rule is identical for preflight and execution.

After reviewing the inventory, execute the complete graph:

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
`TestSemanticCellSignedDirectBootstrapInterruptionResume` provisions the
sanitized, semantically faithful cell fixture into a new database. It combines
the DBOS epoch arithmetic default, parameterized types, enum/cast, JSON, array,
UUID and time defaults, a stored generated column and exact function
dependency, realistic routines and trigger bodies, exactly 248 comments,
constraints, indexes, triggers, RLS, extensions, roles, memberships, grants,
default privileges and ownership. It injects one interruption before every
execution phase, resumes from the digest-bound ledger, reinspects an exact
fingerprint, and requires both next-plan and adopt-existing convergence at zero
changes and zero steps.

The PostgreSQL 14–18 matrix runs that semantic fixture through signed direct,
native CLI prepare/authorize/bootstrap, and production Kubernetes controller
paths against disposable new databases. The controller independently verifies
a generated signed release artifact and the signed bootstrap manifest before
executing the exact whole-database plan; verification is not replaced by a test
seam. Routine postconditions compare PostgreSQL parse fingerprints so harmless
catalog whitespace reformatting does not weaken reviewed body digests.

`TestSyntheticScaleBootstrapInventoryManifest` is a separate generated scale
fixture. Its exact 1,007 resources, 1,026 execution steps, and 370 phases test
byte stability, cycle-free scheduling, the final access handoff, 315
non-transactional online indexes and large dependency fanout. The count-shaped
fixture is not described as a real cell schema and does not replace the
semantic proof.

## Complete-cell scale and lock budgets

The synthetic count-bearing fixture is guarded by deterministic structural
budgets rather than flaky wall-clock assertions. It may contain at most 1,250
managed resources, produce at most 4 MiB of HCL and SQL respectively, produce
an 8 MiB canonical whole-database plan, and schedule at most 2,000 steps in
1,000 phases. No step may have more than 1,200 direct dependencies; the large
fanout is deliberate on the final access-handoff barrier, which must follow
every construction step. The test
logs the actual resource, byte, step, phase, dependency, lock, transaction, and
scan totals, and generates the plan twice to require byte-for-byte equality.
Signed release artifacts are independently bounded at 8 MiB; the semantic
controller fixture is approximately 5.4 MiB because it carries executable SQL,
checks, resource specifications, comments and dependency metadata.

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
