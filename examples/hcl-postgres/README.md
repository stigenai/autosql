# HCL-managed PostgreSQL example

This runnable example uses AutoSQL's declarative HCL dialect as the schema
source of truth. It demonstrates the complete read-only planning loop and then
checks the result against a real PostgreSQL database:

1. Load two ergonomic HCL schema documents into AutoSQL's canonical graph.
2. Compute the semantic diff from v1 to v2.
3. Build a dependency-aware PostgreSQL plan for two additive columns.
4. Create v1 in a disposable PostgreSQL database.
5. Inspect the live database and render its canonical schema back to HCL.
6. Load the generated HCL again to prove deterministic round-tripping.
7. Apply the v2 SQL expansion and verify that desired and live identities
   converge.
8. Load a checked-in lossless HCL catalog containing every PostgreSQL resource
   kind advertised by the AutoSQL PostgreSQL driver.
9. Create the same advanced objects in PostgreSQL, inspect them with advanced
   security metadata enabled, render them to HCL, and load that HCL again.

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

Nested resources gain containment dependencies automatically. AutoSQL also
supports a lossless `resource "kind" "name"` form; `schema inspect --format
hcl` emits that deterministic form so every supported canonical resource,
including PostgreSQL-specific constraints, can round-trip without information
loss.

## Comprehensive PostgreSQL catalog

[`advanced.hcl`](advanced.hcl) is a deterministic HCL snapshot produced from
the objects created by [`advanced.sql`](advanced.sql). The automated example
checks its resource kinds against `postgres.New().Info().Capabilities`, so a
new PostgreSQL capability added to AutoSQL makes the example test fail until
the catalog is expanded.

[`friendly-advanced.hcl`](friendly-advanced.hcl) expresses the commonly
hand-authored subset without escaped JSON or copied stable IDs. For example:

```hcl
resource "foreign_key" "accounts_organization_fkey" {
  schema    = "friendly"
  parent    = table_id("friendly", "accounts")
  spec_json = jsonencode({
    definition = "FOREIGN KEY (organization_id) REFERENCES friendly.organizations(id)"
  })
  deps_json = jsonencode([
    contains(table_id("friendly", "accounts")),
    references(column_id("friendly", "accounts", "organization_id")),
    references(column_id("friendly", "organizations", "id")),
  ])
}
```

Available pure helpers are `jsonencode(value)`, `schema_id(name)`,
`table_id(schema, table)`, `column_id(schema, table, column)`,
`resource_id(kind, schema, parent, name)`, and the typed dependency constructors
`contains(id)`, `references(id)`, `uses(id)`, and `owns(id)`. They perform no
filesystem, network, environment, or secret access, keeping HCL evaluation
deterministic and safe for CI.

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

The capability mode matters. AutoSQL currently manages schema, table, column,
view, and materialized-view transitions through its PostgreSQL planner. The
other PostgreSQL kinds are losslessly inspected and represented as read-only
catalog resources, allowing drift detection, policy review, and signed evidence
without pretending AutoSQL can safely synthesize every mutation. The example
uses explicit SQL to create those advanced objects, then proves their HCL
representation round-trips.

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
  --schema hcl_demo --format hcl
```
