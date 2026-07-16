# AutoSQL feature examples and demos

Examples and demos are required by the [AutoSQL Constitution](../CONSTITUTION.md).
The machine-checked source of truth is [`catalog.json`](catalog.json). Evidence
levels are `example`, `contract`, `integration`, and `live`; see the
constitution for their exact meaning.

| Feature area | Documentation | Executable demo/evidence |
| --- | --- | --- |
| <!-- feature:schema-authoring-and-planning --> Schema authoring, providers, semantic diff, and planning | [Sources](../docs/schema-sources.md), [planning](../docs/planning.md), [HCL](../docs/hcl-schema.md) | `go test ./examples/hcl-postgres`; [`hcl-postgres/run.sh`](hcl-postgres/run.sh) |
| <!-- feature:postgresql-schema-management --> PostgreSQL inspection and managed schema changes | [Planning](../docs/planning.md), [defaults](../docs/postgresql-default-expressions.md), [fresh provisioning](../docs/postgresql-provisioning-compatibility.md) | `go test ./pkg/postgres`; [`hcl-postgres/run.sh`](hcl-postgres/run.sh) against disposable PostgreSQL |
| <!-- feature:cli-configuration-and-secrets --> CLI, configuration, and secret references | [CLI contract](../docs/cli-contract.md) | `go test ./internal/cli` |
| <!-- feature:migration-artifacts-and-recovery --> Artifacts, revisions, apply, down, and repair | [Migration generation](../docs/migration-generation.md), [recovery](../docs/migration-start-recovery.md) | `go test ./pkg/artifact ./pkg/migrate/...` |
| <!-- feature:approval-policy-and-safety --> Approval, policy, prechecks, simulation, and safety | [Guardrails](../docs/guardrail.md), [safety analysis](../docs/safety-analysis.md) | `go test ./pkg/approval ./pkg/guardrail ./pkg/policy ./pkg/precheck ./pkg/safety ./pkg/simulate` |
| <!-- feature:execution-audit-and-backup --> Execution, audit durability, and backup | [Executor](../docs/executor.md), [backup](../docs/artifact-backup.md) | `go test ./pkg/auditlog ./pkg/backup ./pkg/executor` |
| <!-- feature:inventory-observability-and-analytics --> Inventory, drift, monitoring, docs, and analytics | [Observability](../docs/schema-registry-observability.md), [analytics](../docs/schema-analytics.md) | `go test ./pkg/analytics ./pkg/drift ./pkg/inventory ./pkg/monitor ./pkg/schemadoc` |
| <!-- feature:fleet-and-tenant-rollouts --> Fleet and tenant rollouts | [Fleet](../docs/fleet-orchestration.md), [tenants](../docs/tenant-fleet.md) | `go test ./pkg/fleet ./pkg/tenantfleet` |
| <!-- feature:database-security-and-reference-data --> Security as code and reference data | [Security](../docs/database-security.md), [reference data](../docs/reference-data.md) | `go test ./pkg/reference ./pkg/security` |
| <!-- feature:delivery-and-developer-integrations --> CI, GitOps, ORM, IDE, deployment, and Terraform | [Integrations](../docs/integrations.md), [CI/CD](../docs/ci-cd-integrations.md) | `go test ./pkg/ci ./pkg/integration ./pkg/integrations/...` |
| <!-- feature:database-test-environments --> Disposable database tests | [Database testing](../docs/database-testing.md) | `go test ./pkg/dbtest` |
| <!-- feature:kubernetes-operator --> Kubernetes operator and CEL validation | [Operator](../docs/kubernetes-operator.md) | `go test ./pkg/operator ./internal/operatorcontroller` |
| <!-- feature:zero-downtime-migrations --> Expand/contract and zero-downtime migrations | [Format](../docs/zero-downtime-format.md), [compatibility](../docs/postgresql-zero-downtime-compatibility.md) | `go test ./pkg/zdm/... ./pkg/zerodowntime` |
| <!-- feature:multi-dialect-contracts --> Multi-dialect capability contracts | [Multi-dialect](../docs/multi-dialect.md) | `go test ./pkg/dialect` |

## Full live demos

- [`hcl-postgres`](hcl-postgres/README.md) exercises HCL authoring, semantic
  planning, PostgreSQL inspection, every advertised PostgreSQL resource kind,
  default expressions, deterministic round trips, and a complete 1,000+-resource
  empty-database bootstrap through
  [`run-complete-bootstrap.sh`](hcl-postgres/run-complete-bootstrap.sh).
- [`postgres-lifecycle`](postgres-lifecycle/README.md) exercises a PostgreSQL
  schema lifecycle and generated upgrades against a disposable database.

When adding or expanding a feature, update both this index and `catalog.json`
in the same change. The repository test suite checks catalog/package coverage,
PostgreSQL capability coverage, evidence levels, paths, and index entries.
