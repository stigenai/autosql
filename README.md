# AutoSQL

AutoSQL is a PostgreSQL-first database schema and upgrade platform. It turns
SQL, native schema files, and external ORM providers into a versioned canonical
schema graph, then plans, tests, approves, applies, observes, and recovers
database changes through stable CLI and JSON contracts.

## Capabilities

- **Desired state and safety:** deterministic inspection, semantic diffs,
  dependency-ordered plans, policy checks, approvals, simulation, and guarded
  apply.
- **Versioned upgrades:** signed migration artifacts, checkpoints, controlled
  down migrations, repair, and PostgreSQL expand/contract upgrades with virtual
  schemas, shadow columns, online backfills, rollback, and contract gates.
- **Control plane:** immutable artifact registry with tags and attestations,
  customer-owned backup/fallback, target inventory, deployment history, schema
  docs and ER diagrams, semantic drift incidents, audit search/retention, and
  notifications.
- **Fleet delivery:** deterministic target snapshots, staged canaries, bounded
  concurrency, resumable execution, approval gates, health hooks, status, and
  recovery operations.
- **Integrations:** external schema-provider SDKs, reference ORM adapters, CI
  changeset checks and signed attestations, pinned CI/CD packaging, Kubernetes
  reconciliation, Terraform-style deployment contracts, webhooks, and
  editor-neutral local workflows.

AutoSQL is explicit about database capabilities and refuses unsafe operations.
Secrets are passed by reference, resolved only at runtime, and excluded from
artifacts, state, status, and diagnostics.

## Build and test

```sh
go test ./...
go vet ./...
go build ./...
```

The PostgreSQL integration test is opt-in:

```sh
AUTOSQL_TEST_POSTGRES_URL='postgres://…' go test ./pkg/postgres -run Integration
```

## CLI

```sh
autosql version --json
autosql config validate --config autosql.json --preflight
autosql schema load --source sql:schema.sql --json
AUTOSQL_DATABASE_URL='postgres://…' \
  autosql schema inspect --url env://AUTOSQL_DATABASE_URL --schema public --json
```

Database URLs are supplied to the CLI through `env://` or `file://` secret
references. Resolved values are redacted and never included in serialized
output. See [the CLI contract](docs/cli-contract.md) and
[schema-source contract](docs/schema-sources.md).

## Documentation

- [Schema planning and safety](docs/planning.md) · [guardrails](docs/guardrail.md)
- [Migration lifecycle](docs/migration-generation.md) · [controlled down](docs/controlled-down.md)
- [Zero-downtime migrations](docs/zero-downtime-format.md) · [PostgreSQL compatibility](docs/postgresql-zero-downtime-compatibility.md)
- [Registry, inventory, backup, drift, and audit](docs/schema-registry-observability.md)
- [Fleet orchestration](docs/fleet-orchestration.md)
- [Provider and CI contracts](docs/schema-provider-protocol.md) · [CI changesets](docs/ci-changeset-contract.md)
- [CI/CD packaging and Kubernetes](docs/ci-cd-integrations.md) · [operator](docs/kubernetes-operator.md)
- [ORM, deployment, and developer integrations](docs/integrations.md)
