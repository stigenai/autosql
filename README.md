# AutoSQL

AutoSQL is a PostgreSQL-first database schema and upgrade platform. It turns
SQL, native schema files, HCL, and external ORM providers into a versioned
canonical schema graph, then plans, tests, approves, applies, observes, and
recovers database changes through stable CLI and JSON contracts.

The central idea is simple: a database change is an artifact-bound, reviewable
state transition. AutoSQL keeps the desired schema, inspected schema, plan,
safety evidence, approval, execution state, and audit history connected by
digests. That makes an upgrade explainable before it runs and recoverable when
the world changes during execution.

## Capabilities

- **Author-first desired state and safety:** native PostgreSQL HCL blocks,
  symbolic references, typed expressions and modules, reviewed rename intent,
  deterministic inspection, semantic diffs,
  dependency-ordered plans, advanced migration lint, policy checks, approvals,
  simulation, and guarded apply. Findings include evidence, confidence,
  impact, remediation, affected objects, and machine-readable properties.
- **Complete PostgreSQL bootstrap:** managed or externally supplied databases,
  schemas, types, tables, columns, defaults, generated columns, constraints,
  indexes, routines, triggers, RLS, extensions, comments, roles, grants,
  memberships, and default privileges through one resumable plan.
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
- **Security and reference data:** managed versus external principals, grants,
  memberships, row-level policies, short-lived credentials, and bounded
  INSERT/UPSERT/SYNC reconciliation for application-owned rows.
- **Schema analytics and governance:** bounded read-only table, index, and
  constraint metrics; growth snapshots; complexity scoring; retention;
  threshold findings; permission summaries; and target/artifact/schema-bound
  historical queries.
- **AI-safe review:** dynamic SQL and table-copy detection, naming and merge
  checks, reproducible disposable-environment test tokens, and explicit
  untrusted agent provenance. Agent-authored changes use the same gates as
  human-authored changes.

AutoSQL is explicit about database capabilities and refuses unsafe operations.
Secrets are passed by reference, resolved only at runtime, and excluded from
artifacts, state, status, and diagnostics.

The [AutoSQL Constitution](CONSTITUTION.md) makes examples and demos part of
feature completeness. The machine-checked [feature example and demo
catalog](examples/README.md) maps every production package and advertised
PostgreSQL capability to user documentation and executable evidence.

## How AutoSQL works

1. **Author or import desired state.** Load SQL, native schema files, HCL, or
   an ORM/provider output. AutoSQL normalizes each source into the canonical
   schema graph while preserving source locations and unknown extension data.
2. **Inspect the target.** Use a read-only provider transaction to capture the
   current graph, capabilities, version, and optional bounded statistics. URLs
   are passed by `env://` or `file://` reference and are never persisted.
3. **Diff and plan.** AutoSQL computes a semantic diff, resolves dependency
   order, renders dialect-specific statements, groups transaction phases, and
   records lock, rewrite, scan, and destructive impact.
4. **Lint and simulate.** Compatibility, PostgreSQL operational, advanced
   migration, security, and configured policy analyzers produce deterministic
   diagnostics. Simulation and database-test contracts exercise the plan in a
   disposable environment; generated tests cannot use production credentials.
5. **Approve an immutable bundle.** The plan, artifact, analyzer attestations,
   policy identity, target identity, provenance, and test evidence are bound by
   digests. Changing any input invalidates the approval.
6. **Apply through a guardrail.** The executor acquires the required locks,
   performs live prechecks, writes durable audit intent before mutation, and
   advances through idempotent checkpoints. Stale plans, missing evidence,
   failed audits, and unsafe capabilities fail closed.
7. **Observe and recover.** Inventory, drift, analytics, fleet health, and
   notifications record the outcome. Checkpointed migrations can resume;
   controlled down, repair, rollback, backup, and reconciliation workflows
   handle partial or uncertain outcomes.

See [How AutoSQL works](docs/overview.md) for the full architecture and
feature guide.

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

## Container image

The published CLI image is public at `ghcr.io/stigenai/autosql`. It is built with the repository's
multi-platform BuildKit builder and includes both `linux/amd64` and
`linux/arm64` variants. The image is nonroot and contains no credentials:

```sh
docker run --rm ghcr.io/stigenai/autosql@sha256:0e748a1c8fbc51c2bfbdf00be13bf0484d5c6f6be8b45612550ec211d4cd0905 version --json
```

Pin the release index digest shown in the release notes for deployments.

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

- [Project constitution](CONSTITUTION.md) · [feature examples and demos](examples/README.md)
- [How AutoSQL works: architecture and feature guide](docs/overview.md)
- [Schema planning and safety](docs/planning.md) · [guardrails](docs/guardrail.md)
- [Migration lifecycle](docs/migration-generation.md) · [controlled down](docs/controlled-down.md)
- [Zero-downtime migrations](docs/zero-downtime-format.md) · [PostgreSQL compatibility](docs/postgresql-zero-downtime-compatibility.md)
- [Registry, inventory, backup, drift, and audit](docs/schema-registry-observability.md)
- [Fleet orchestration](docs/fleet-orchestration.md)
- [Provider and CI contracts](docs/schema-provider-protocol.md) · [CI changesets](docs/ci-changeset-contract.md)
- [CI/CD packaging and Kubernetes](docs/ci-cd-integrations.md) · [operator](docs/kubernetes-operator.md) · [operator GitOps artifacts](docs/operator-gitops.md)
- [ORM, deployment, and developer integrations](docs/integrations.md)
- [Terraform/OpenTofu provider contract](docs/terraform-provider.md)
- [Platform integration support and upgrades](docs/platform-integrations.md)
- [Dynamic and composite sources](docs/dynamic-sources.md)
- [Declarative HCL authoring](docs/hcl-schema.md)
- [PostgreSQL default-expression support and safety](docs/postgresql-default-expressions.md)
- [PostgreSQL fresh-provisioning compatibility](docs/postgresql-provisioning-compatibility.md)
- [PostgreSQL whole-database bootstrap and recovery](docs/postgresql-database-bootstrap.md)
- [Cloud discovery and monitoring](docs/cloud-monitoring.md)
- [Tenant-scale fleet rollout](docs/tenant-fleet.md) · [multi-dialect contracts](docs/multi-dialect.md)
- [CI/CD and GitOps ecosystem](docs/ci-gitops-ecosystem.md)
- [Schema analytics and governance](docs/schema-analytics.md)
- [Advanced migration lint and AI-safe review](docs/advanced-migration-lint.md)
- [Integration testing and coverage](docs/integration-testing.md)
- [Release binaries and container images](docs/releasing.md)
- [Detailed PostgreSQL lifecycle example](examples/postgres-lifecycle/README.md)
- [HCL-managed PostgreSQL example](examples/hcl-postgres/README.md)
- [Complete HCL empty-database bootstrap runner](examples/hcl-postgres/run-complete-bootstrap.sh)
- [Database security as code](docs/database-security.md) · [reference data](docs/reference-data.md)
