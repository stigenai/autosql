# HCL-managed PostgreSQL example

This runnable example uses AutoSQL's declarative HCL dialect as the schema
source of truth. It demonstrates the complete read-only planning loop and then
checks the result against a real PostgreSQL database:

1. Load two ergonomic HCL schema documents into AutoSQL's canonical graph.
2. Compute the semantic diff from v1 to v2.
3. Build a dependency-aware PostgreSQL plan for two additive columns.
4. Create v1 in a disposable PostgreSQL database.
5. Inspect the live database and render its author or canonical schema back to HCL.
6. Load the generated HCL again to prove deterministic round-tripping.
7. Apply the v2 SQL expansion and verify that desired and live identities
   converge.
8. Load a checked-in lossless HCL catalog containing every PostgreSQL resource
   kind advertised by the AutoSQL PostgreSQL driver.
9. Create the same advanced objects in PostgreSQL, inspect them with advanced
   security metadata enabled, render them to HCL, and load that HCL again.
10. Load and plan a focused catalog covering every managed PostgreSQL default
    expression family and its enum, domain, and sequence dependencies.

The example uses `psql` for mutation so it stays runnable without production
signing keys. In a release pipeline, feed the same HCL sources into AutoSQL's
signed artifact, approval, guardrail, and operator workflows.

## Run it

Requirements: Go, `psql`, and a disposable PostgreSQL 14+ database.

```sh
createdb autosql_hcl_example
export AUTOSQL_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/autosql_hcl_example?sslmode=disable'
go build -o autosql ./cmd/autosql
AUTOSQL_BIN="$PWD/autosql" ./examples/hcl-postgres/run.sh
```

Set `AUTOSQL_KEEP_WORKDIR=1` to retain the canonical JSON envelopes and the HCL
rendered from PostgreSQL. The script replaces the `hcl_demo` and `hcl_advanced`
schemas and recreates the `hcl_advanced_reader` and `hcl_advanced_app` roles in
the database supplied through `AUTOSQL_DATABASE_URL`; use a disposable database.

Expected final output:

```text
verified: hcl_load=true changes=2 plan_steps=2 postgres_round_trip=true converged=true advanced_kinds=23 helpers=true
Example completed successfully.
```

## Complete empty-database bootstrap

The count-bearing bootstrap demo generates one monolithic
`complete-bootstrap.hcl` containing a non-secret managed-database target and
1,007 resources in author HCL with per-resource canonical fallback only when
needed. It includes 315 indexes, 197 constraints, 47
routines, six triggers, 14 policies on seven RLS tables, two extensions, a
composite type, roles,
memberships, grants, default privileges, and comments. The runner loads the HCL
back, plans it twice byte-identically, creates a new database, interrupts once
before every phase, resumes, reinspects exact catalog parity, and proves a
second ordinary schema plan is a no-op.

```sh
export AUTOSQL_TEST_POSTGRES_URL='postgres://postgres:postgres@127.0.0.1:5432/postgres?sslmode=disable'
AUTOSQL_KEEP_WORKDIR=1 ./examples/hcl-postgres/run-complete-bootstrap.sh
```

The maintenance identity and database are external prerequisites. The runner
creates and removes a uniquely named target and its disposable roles. With
`AUTOSQL_KEEP_WORKDIR=1`, it retains the exact generated HCL for inspection;
otherwise the artifact is removed after successful verification. CI runs this
same proof on PostgreSQL 14–18.

Before running a retained `complete-bootstrap.hcl` outside the disposable demo,
use `autosql database bootstrap prepare --json` to review its complete
authorization inventory, then `autosql database bootstrap authorize` to create
one signed, expiring manifest. Execution accepts that manifest with an
independently supplied Ed25519 public key; the artifact binds the exact source
and plan while containing no SQL, routine source, executable steps, or
credentials. See [PostgreSQL database bootstrap](../../docs/postgresql-database-bootstrap.md)
for the full commands and key policy.

`database-bootstrap.hcl` also demonstrates the execution-side
`bootstrap_authorization` block. It contains only `env://`/`file://` runtime
references plus issuer, signer, and purpose; it cannot carry a private key or
resolved credential. Remove or replace the example paths before execution.

## HCL model

The checked-in files use ergonomic nested blocks:

```hcl
schema "hcl_demo" {}

table "accounts" {
  schema = "hcl_demo"

  column "email" {
    type     = "text"
    nullable = false
    ordinal  = 2
  }
}
```

Nested resources gain containment dependencies automatically. Symbolic
references such as `schema.hcl_demo`, `table["hcl_demo"]["accounts"]`, and
`column.email` replace copied stable IDs. `schema inspect --format hcl
--hcl-style author` emits native blocks, comments, and heredocs;
`--hcl-style canonical` emits the lossless `resource "kind" "name"` form.
Author output falls back per resource when extension fields have no native
representation, so mixed output remains lossless.

[`author-modules/main.hcl`](author-modules/main.hcl) is the executable language
example for typed variables, validation, locals, deterministic `for_each`,
isolated module inputs, typed outputs, and cross-file symbolic references.
[`rename-intent.hcl`](rename-intent.hcl) demonstrates `renamed_from` and
`moved` blocks; plans bind the normalized rename-hint digest.

## Comprehensive PostgreSQL catalog

[`advanced.hcl`](advanced.hcl) is a deterministic HCL snapshot produced from
the objects created by [`advanced.sql`](advanced.sql). The automated example
checks its resource kinds against `postgres.New().Info().Capabilities`, so a
new PostgreSQL capability added to AutoSQL makes the example test fail until
the catalog is expanded.

[`friendly-advanced.hcl`](friendly-advanced.hcl) demonstrates the compatibility
helpers used by older or generated HCL. New hand-authored schemas should prefer
native blocks such as:

```hcl
table "accounts" {
  schema = schema.friendly
  column "organization_id" { type = "bigint" }
  foreign_key "accounts_organization_fkey" {
    columns     = [column.organization_id]
    ref_columns = [table.organizations.column.id]
  }
}
```

Available pure helpers are `jsonencode(value)`, `schema_id(name)`,
`table_id(schema, table)`, `column_id(schema, table, column)`,
`resource_id(kind, schema, parent, name)`, and the typed dependency constructors
`contains(id)`, `references(id)`, `uses(id)`, and `owns(id)`. They perform no
filesystem, network, environment, or secret access, keeping HCL evaluation
deterministic and safe for CI.

Extensions and composite types have direct authoring blocks plus
`extension_id`, `composite_id`, `composite_attribute`, and
`collated_composite_attribute` helpers. Extension execution still requires the
separate render policy (`extension_allowlist`, exact `extension_version.<name>`,
and optional `extension_schemas.<name>`), so an HCL declaration cannot approve
its own privileged supply-chain input.

[`extension-readiness.hcl`](extension-readiness.hcl) is a non-secret, complete
preflight example. Run `autosql database bootstrap preflight` with an extension
allowlist plus repeatable `--extension-version` and `--extension-schema` flags
to get deterministic text or JSON readiness for each requested package. The
report separates authorization failures from missing server control files,
unavailable versions, fixed-schema conflicts, and insufficient database or
schema privilege; the command performs no bootstrap mutation.

[`database-bootstrap.hcl`](database-bootstrap.hcl) declares the separate
server/maintenance-database/target-database contract used by
`autosql database prepare`. It contains no connection URL or credential.

[`defaults.hcl`](defaults.hcl) is the executable default-expression reference.
It provisions scalar and cast literals, JSONB, UUIDs, CIDR/INET/MACADDR network
values, bounded arithmetic
(including the DBOS `extract(epoch from ...) * 1000` default), the
generated-function allowlist, enum and domain defaults, one-dimensional arrays,
temporal and interval forms, and an exactly qualified sequence-backed `nextval`. The
automated example test builds a clean-database plan and checks that sequence
creation precedes the dependent column. The complete grammar and its explicit
rejection boundaries are documented in
[PostgreSQL default expressions](../../docs/postgresql-default-expressions.md).

Stored generated columns may reference an application routine through exact
`references(...)` dependencies. Application functions are managed when their
normalized digest is explicitly supplied through `reviewed_routine_digests`;
the catalog definition remains inert without that independent review authority.
Extension-owned routines remain extension prerequisites and can still be
verified with `postgres.VerifyGeneratedRoutinePrerequisites`.
Verification distinguishes exact matches, missing routines, and definition or
extension-version mismatches without rendering arbitrary routine bodies.
[`generated.hcl`](generated.hcl) is the executable lifecycle-style example: it
declares the external routine, exact source-column/routine edges, and the
creation-only `generated = "s"` expression.

| PostgreSQL area | HCL resource kinds demonstrated | Example configuration |
| --- | --- | --- |
| Namespaces and extensions | `schema`, `extension` | Dedicated schema and `uuid-ossp` extension |
| User-defined types | `enum`, `domain`, `composite_type` | Account status, nonnegative money, contact record |
| Storage | `sequence`, `table`, `column` | Owned cached sequence, arrays, JSONB, typed/defaulted columns |
| Constraints | `primary_key`, `unique_constraint`, `check_constraint`, `foreign_key` | Named keys, email check, cascading organization reference |
| Access paths | `index` | Partial multicolumn btree with descending ordering |
| Derived relations | `view`, `materialized_view` | Active-account projection and grouped account counts |
| Server-side behavior | `function`, `procedure`, `trigger` | Immutable SQL function, trigger function, activation procedure |
| Row security | `policy` | Forced RLS with a role-scoped `SELECT` policy |
| Identities and access | `role`, `grant`, `membership`, `default_privilege` | NOLOGIN roles, inheritance, object grants, future-table grants |

The capability mode matters. AutoSQL now manages relational constraints,
standalone indexes, review-authorized functions/procedures, and triggers in
addition to schema, type, sequence, table, column, view, and materialized-view
transitions. Extension and database-security objects are fully planned and
maintained, but remain explicit authority boundaries: extension packages must
already exist, routine bodies need reviewed digests, and server/database/
security credentials are supplied only at runtime. The advanced catalog
continues to round-trip every advertised kind without field loss.

## Useful commands

```sh
autosql schema load --source hcl:examples/hcl-postgres/schema-v1.hcl --json
autosql schema diff \
  --from hcl:examples/hcl-postgres/schema-v1.hcl \
  --to hcl:examples/hcl-postgres/schema-v2.hcl --json
autosql plan \
  --from hcl:examples/hcl-postgres/schema-v1.hcl \
  --to hcl:examples/hcl-postgres/schema-v2.hcl --json
autosql schema inspect --url env://AUTOSQL_DATABASE_URL \
  --schema hcl_demo --format hcl --hcl-style author
```
