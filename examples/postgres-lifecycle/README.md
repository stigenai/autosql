# PostgreSQL lifecycle example

This example is a small, reproducible AutoSQL walkthrough. It uses the CLI
against a disposable PostgreSQL database and demonstrates the main control
loop:

1. Author desired state as SQL (`schema-v1.sql`).
2. Load SQL into AutoSQL's canonical schema graph.
3. Inspect a live PostgreSQL database through an `env://` secret reference.
4. Produce a semantic diff and dependency-aware plan for `schema-v1.sql` to
   `schema-v2.sql`.
5. Apply the v1 schema and seed data to PostgreSQL.
6. Apply the planned v2 expansion (two additive columns).
7. Inspect again and verify that live tables/columns converge with v2.
8. Initialize and query AutoSQL's zero-downtime metadata namespace.

The script deliberately applies SQL with `psql` so that it can be run against
any disposable PostgreSQL instance without requiring signing or production
approval configuration. In a release pipeline, replace that step with a
signed migration artifact and `migrate apply`/guardrail configuration.

## Run it

Requirements: Go, `psql`, and PostgreSQL 14+ (the verification run used
PostgreSQL 16).

```sh
createdb autosql_example
export AUTOSQL_DATABASE_URL='postgres://postgres:postgres@127.0.0.1:5432/autosql_example?sslmode=disable'
go build -o autosql ./cmd/autosql
AUTOSQL_BIN="$PWD/autosql" ./examples/postgres-lifecycle/run.sh
```

Set `AUTOSQL_KEEP_WORKDIR=1` to retain the JSON envelopes produced at each
stage. The script fails on the first unsuccessful command and never prints the
database URL in an AutoSQL response.

## Feature map

| Feature | Example entry point | Verified here |
| --- | --- | --- |
| SQL desired state and canonical graph | `schema-v1.sql`, `schema-v2.sql`, `schema load` | Yes |
| PostgreSQL catalog inspection | `schema inspect --url env://...` | Yes |
| Secret-by-reference URL handling | `AUTOSQL_DATABASE_URL`, `env://AUTOSQL_DATABASE_URL` | Yes |
| Semantic diff | `schema diff` | Yes (2 additive changes) |
| Dependency-aware PostgreSQL plan | `plan` | Yes (2 executable steps) |
| Real database upgrade | `apply-v1.sql`, `apply-v2.sql` | Yes, with PostgreSQL 16 |
| Convergence verification | second `schema inspect` plus graph identity checks | Yes |
| Zero-downtime metadata | `migrate metadata-init/status` | Yes |
| Seed/reference data | `seed.sql` | Yes, via `psql` |
| Signed artifacts, approvals, guardrails | `docs/migration-generation.md`, `docs/guardrail.md` | Covered by integration tests; requires configured keys |
| Expand/contract, virtual schemas, shadow sync, backfills | `docs/zero-downtime-format.md`, `docs/online-backfills.md` | Covered by integration tests; requires migration artifacts |
| Controlled down, repair, rollback | `docs/controlled-down.md`, `docs/zero-downtime-rollback-repair.md` | Covered by integration tests |
| Fleet, registry, analytics, security, providers, CI/CD | feature guides in `docs/` | Contract/documentation coverage |

The feature guides are intentionally linked from this example rather than
pretending that a disposable schema should exercise production-only signing,
fleet credentials, or destructive rollback operations.

## Observed verification

The checked-in script was run against a real PostgreSQL 16 container with a
fresh database. It completed with:

```text
verified: loaded=True changes=2 plan_steps=2 resources=17 converged=true metadata_initialized=true
Example completed successfully.
```

The 17 inspected resources include the `app` schema, `customers` and `orders`
tables, their columns, primary/foreign/check/unique constraints, and the
zero-downtime metadata is stored separately in `autosql_zdm`.
