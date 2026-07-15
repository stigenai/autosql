# Integration testing and coverage

AutoSQL has two complementary test layers:

1. **Deterministic unit and contract tests** run with `go test ./...`. They
   cover parsers, canonical models, analyzers, policy and approval rules,
   artifact validation, redaction, provider contracts, and failure-state
   transitions without requiring a database.
2. **Live PostgreSQL integration tests** use a disposable database to exercise
   catalog inspection, semantic planning, migration execution, metadata
   upgrades, zero-downtime workflows, CLI contracts, concurrency, recovery,
   and permission boundaries.

## Supported commands

Run the deterministic suite and static gates first:

```sh
go test ./...
go vet ./...
go build ./...
```

Run all live packages serially. Serial execution is intentional: catalog
inspection and DDL tests create/drop schemas, indexes, roles, and databases.

```sh
AUTOSQL_TEST_POSTGRES_URL='postgres://postgres:postgres@127.0.0.1:5432/autosql?sslmode=disable' \
AUTOSQL_POSTGRES_TEST_DSN="$AUTOSQL_TEST_POSTGRES_URL" \
scripts/test-live-serial.sh ./pkg/... ./internal/cli
```

The PostgreSQL version matrix is defined in
`.github/workflows/zdm-postgres-matrix.yml` and currently exercises the ZDM
and CLI live suites on PostgreSQL 14 through 18. `AUTOSQL_LIVE_COUNT` repeats
the live suite when investigating flakes.

The same matrix runs `TestCompleteCellProvisioningParity`, which adopts a rich
inspected cell, provisions its managed projection from empty, applies and
reinspects it, proves a second-plan no-op, and applies an unrelated incremental
change. See [PostgreSQL fresh-provisioning compatibility](postgresql-provisioning-compatibility.md)
for the exact managed and external/read-only boundary.

## Current audit baseline

The repository contains live tests for PostgreSQL inspection/planning,
migration generation/apply/down/repair/checkpoints, simulation, executor and
guardrails, CLI migration commands, and the zero-downtime packages (backfill,
compatibility matrices, contract gates, expand planning, metadata, rollback,
shadow sync, start, and virtual schemas).

The deterministic suite currently reports approximately 49.5% aggregate
statement coverage. The lowest-covered operational packages are the live-only
ZDM paths: backfill, contract, rollback, start, shadow sync, and virtual
schema. Their live tests are valuable, but they are opt-in and do not
contribute to a normal `go test ./...` coverage run unless a PostgreSQL URL is
provided.

The current Beads coverage program tracks the remaining work:

- repair the migration revision live schema mismatch (`autosql-dwj`);
- harden concurrent catalog tests (`autosql-0si`);
- add an end-to-end control-plane scenario across artifact, approval, audit,
  inventory, drift, analytics, and recovery (`autosql-deb`);
- add provider and delivery integration conformance fixtures (`autosql-q6e`);
- expand CI's serial live matrix and publish per-package coverage (`autosql-mxk`).

These are explicit gaps, not silently skipped success conditions. A live test
run that is unavailable, unexpectedly skipped, or fails due to a shared catalog
resource must remain visible in CI output and Beads.

## Coverage expectations by feature

| Feature area | Current evidence | Next integration target |
| --- | --- | --- |
| Schema sources, diff, plan, PostgreSQL rendering | Unit, golden, and live PostgreSQL tests | Keep provider fixtures in the conformance suite |
| Safety, policy, approval, redaction | Broad unit/contract tests | Bind results through the control-plane scenario |
| Migration execution and recovery | Live executor, migrate, CLI, and ZDM tests | Fix revision metadata mismatch and repeatability |
| Artifacts, audit, inventory, drift, analytics | Package-local tests | Cross-package artifact-to-status scenario |
| Fleet, monitoring, tenant rollout | Package-local tests | Disposable target fleet health/recovery scenario |
| ORM/provider/Terraform/CI/GitOps/operator | Package-local contract tests | Cross-contract capability and callback fixtures |
| Backfill, shadow sync, virtual schema, rollback | Live ZDM tests | Increase repeat count and failure injection coverage |

Integration tests must use disposable identities and databases, never embed
credentials in artifacts or output, clean up on cancellation, and verify both
the positive result and the refusal/recovery path.
