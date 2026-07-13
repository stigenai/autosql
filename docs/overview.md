# How AutoSQL works

AutoSQL is a database change control plane. It is not only a migration file
generator and it is not a general-purpose database observability system. It
connects desired state, database inspection, change planning, safety evidence,
approval, execution, and operations into one verifiable lifecycle.

## The core model

AutoSQL represents a database as a canonical schema graph. A graph contains
stable resource IDs for schemas, tables, columns, indexes, constraints, views,
routines, roles, grants, policies, and other supported objects. A source can be
SQL, a native schema document, HCL, or an external provider. Providers inspect
real databases and render plans for a particular dialect.

Every important boundary has a version and digest:

| Boundary | What is bound |
| --- | --- |
| Source | Desired graph, source revision, and source locations |
| Inspection | Target identity, engine/version, observed graph, and capabilities |
| Plan | From/to fingerprints, changes, SQL steps, phases, locks, and impact |
| Safety | Rule-pack versions, analyzer attestations, configuration, findings |
| Approval | Plan, policy, environment, approvers, expiry, and evidence |
| Execution | Artifact, target, checkpoint, lifecycle state, and audit chain |
| Operations | Deployment event, drift report, health, analytics snapshot, and notification |

This binding prevents a plan from being approved for one schema and applied to
another, or from being silently reinterpreted after a rule, policy, or target
changes.

## End-to-end lifecycle

### 1. Author desired state

Use `autosql schema load` with SQL, native schema files, HCL, or a provider
adapter. Dynamic and composite sources can be combined, and external ORM
providers implement the versioned provider protocol. Declarative HCL supports
the same canonical graph rather than introducing a second schema model.

### 2. Inspect and normalize

The PostgreSQL provider inspects catalogs in a read-only, repeatable-read
transaction and retries transient catalog OID changes. Inspection captures
capabilities and source metadata, not credentials or row contents. Cloud
discovery and monitoring can find approved targets, redact connection
references, enforce target limits, and maintain resumable health checkpoints.

### 3. Diff and plan

Semantic diffing distinguishes a rename from a drop/create where identity makes
that possible. The planner validates both documents, calculates fingerprints,
orders dependencies, renders dialect-specific SQL, binds each statement to a
change, and groups statements into transaction-required or non-transactional
phases. Plans record lock level, rewrite, scan, and destructive impact.

Multi-dialect contracts make capability differences explicit. Unsupported
operations fail instead of being approximated as unsafe SQL.

### 4. Analyze, lint, and test

The standard safety pack covers compatibility and PostgreSQL operational risk:
destructive changes, narrowing types, defaults, constraints, table rewrites,
validation scans, blocking DDL, non-concurrent indexes, concurrent-index
transaction violations, and version-specific enum behavior.

The advanced pack adds:

- lower-snake-case naming and ownership-prefix rules;
- dynamic SQL assembled with `EXECUTE`, concatenation, or formatting;
- table-copy and full-table data dependencies;
- conflicting changes targeting one resource;
- reproducible simulation-test tokens for data-writing operations; and
- explicit provenance for untrusted AI or automation agents.

Diagnostics are stable JSON/SARIF records containing rule, evidence, affected
object, confidence, impact, remediation, and properties. They are suitable for
CI status checks, pull-request annotations, policy gates, and audit records.
AI-authored changes are never trusted merely because an agent produced them.

Database testing can generate migration fixtures, compatibility matrices,
fault scenarios, performance checks, shadow-column synchronization tests, and
online-backfill tests. Test execution uses disposable identities and refuses
production credential references.

### 5. Approve and apply

Guardrails combine safety, policy, preflight, approval, and durable audit into
a fail-closed boundary. Approval records identify the environment, requester,
approvers, risk requirements, expiry, plan digest, policy digest, and evidence.
The executor writes intent before mutation, acquires locks, runs live checks,
executes idempotent steps, records checkpoints, and distinguishes confirmed,
failed, canceled, and uncertain outcomes.

Migration artifacts are immutable, signed, attestable, and registry-addressable.
Backup/fallback workflows are customer-owned and can be required by policy.

### 6. Observe, reconcile, and recover

The control plane records target inventory, deployment history, drift incidents,
audit events, notifications, and operational status. Fleet orchestration takes
deterministic target snapshots, stages canaries, bounds concurrency, pauses for
approval or health gates, and resumes from checkpoints.

Recovery includes controlled down migrations, rollback and repair, migration
start recovery, shadow-column synchronization, online backfills, virtual
version schemas, and reconciliation of uncertain execution. A retry never
blindly repeats a mutation whose durable outcome is unknown.

## Feature areas

### Schema and migration engineering

- canonical schema graph and semantic fingerprints;
- SQL, native, HCL, dynamic, composite, and ORM/provider sources;
- dependency-aware planning and dialect rendering;
- signed artifacts, registry tags, attestations, and immutable metadata;
- checkpoints, controlled down migrations, repair, and migration recovery;
- expand/contract upgrades with virtual schemas, shadow columns, and online
  backfills;
- rollback and compatibility contracts for zero-downtime releases; and
- bounded reference-data INSERT/UPSERT/SYNC reconciliation.

### Safety, policy, and security

- deterministic analyzers with implementation/version/config attestations;
- policy expressions, thresholds, suppressions, expiry, and auditability;
- database security as code for principals, grants, memberships, row-level
  policies, ownership, and external identities;
- secret references resolved only at runtime;
- redacted artifacts, status, diagnostics, logs, and audit metadata;
- SQL injection and unsafe dynamic SQL checks; and
- fail-closed behavior when capabilities, statistics, audit, or approval
  evidence are missing.

### Control plane and operations

- target inventory and deployment/event history;
- semantic drift detection and incident status;
- append-only, hash-linked audit search, export, retention, and tamper checks;
- customer-owned backup/fallback contracts;
- bounded cloud target discovery and monitoring with webhook notifications;
- schema documentation and ER diagram generation; and
- schema analytics with table/index/constraint metrics, growth snapshots,
  complexity scores, policy thresholds, retention, and permission summaries.

### Fleet and platform integrations

- staged tenant/fleet rollouts with canaries and bounded concurrency;
- Kubernetes operator reconciliation and status conditions;
- Terraform/OpenTofu-style deployment contracts;
- CircleCI, Bitbucket, Azure DevOps, Argo CD, GitHub, and GitLab contracts;
- artifact, policy, target, and approval binding across CI/GitOps callbacks;
- OIDC and short-lived credential references instead of embedded secrets;
- provider and ORM SDKs with capability negotiation;
- editor-neutral local development flows and webhooks; and
- machine-readable JSON, SARIF, status checks, and PR annotations.

## What AutoSQL intentionally does not do

AutoSQL does not grant autonomous approval, hide an LLM decision behind a green
check, store production credentials in migration artifacts, copy production
rows into generated tests, or pretend unsupported database capabilities are
safe. It also does not replace a full query-performance or infrastructure
observability platform. Analytics are intentionally bounded to schema and
upgrade governance.

## Where to go next

- Start with the [CLI contract](cli-contract.md), [schema sources](schema-sources.md),
  and [planning guide](planning.md).
- Add production controls with [guardrails](guardrail.md),
  [safety analysis](safety-analysis.md), and [database security](database-security.md).
- Operate changes with [fleet orchestration](fleet-orchestration.md),
  [cloud monitoring](cloud-monitoring.md), and [schema registry observability](schema-registry-observability.md).
- Integrate with delivery systems through [CI/CD packaging](ci-cd-integrations.md),
  [CI/GitOps ecosystem contracts](ci-gitops-ecosystem.md), and the
  [Terraform provider contract](terraform-provider.md).
- Review advanced checks in [advanced migration lint](advanced-migration-lint.md)
  and analytics in [schema analytics](schema-analytics.md).
