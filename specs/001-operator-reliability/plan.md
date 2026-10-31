# Implementation Plan: AutoSQL Reliability and Developer Workflow Roadmap

**Feature**: `001-operator-reliability` | **Date**: 2026-10-29

**Spec**: [spec.md](spec.md) | **Planning bead**: `autosql-b66`

**Git branch**: The branch remains `main`; this design is uncommitted. Spec
Kit's feature identifier is independent of the Git branch. Implementation should
start in a fresh worktree on `codex/autosql-roadmap` from revalidated upstream
main. The feature directory keeps its original name to preserve links; scope now
includes the full recommended roadmap selected by the user.

## Summary

Recover accurate readiness after completed database changes lose their
completion record, expose actionable operational signals, and repair the
demonstrated concurrent-change planning regression. Use a read-only completion
observer with strict release/history/state/audit checks, reliable persistence,
existing status fields, controller logging/metrics, and the existing PostgreSQL
qualification matrix. No production mutation or implementation is part of this
planning deliverable.

The current production cell resource is a motivating case, not a certified
recovery candidate. Its original failure cause and full signed-state equivalence
remain unproven. A safe outcome may be an actionable refusal and an authorized
fresh plan.

R2 adds unified project configuration and disposable database factories; R3 adds
schema/migration test files, CLI reports, and bounded release proof.

## Technical Context

**Language/Version**: Go 1.25.0; existing shell, YAML, JSON, and Markdown
assets.

**Primary Dependencies**: pgx v5.7.5, controller-runtime v0.23.3, Kubernetes
client v0.35.6, existing Prometheus client v1.23.2 and structured-logging
dependencies and HCL v2. Confirm versions against the implementation base; add
no new service. Container factory mode requires an explicit Docker-compatible
daemon through a narrow process adapter rather than a new SDK.

**Storage**: Existing PostgreSQL execution history, local operator FileStore,
hash-chained lifecycle audit, and Kubernetes status. No new production history
schema. R2/R3 add temporary development databases/roles and local ownership
leases.

**Testing**: Go tests/race detector, serial live PostgreSQL harness, envtest
Kubernetes 1.35, existing Helm render checks and kind conformance,
feature-catalog checks, and read-only post-rollout evidence.

**Target Platform**: Kubernetes operator and CLI/library on the project's
supported platforms; live PostgreSQL matrix 14–18. Initial development-factory
qualification is Linux/macOS, with Docker-compatible or approved existing-admin
backends.

**Project Type**: Existing Go CLI/library plus Kubernetes controller and
deployment assets; keep changes within established packages.

**Performance Goals**: Supported recovery within two minutes after dependencies
recover; transient retry cap 60 seconds; deterministic failure recheck at five
minutes; retain existing planning lock/statement/transaction deadlines. Factory
startup defaults to two minutes, cases to five minutes, and each cleanup action
to ten seconds. Case parallelism defaults to one and is bounded at 32. These are
qualification targets, not claims about current production performance.

**Constraints**: No SQL replay during observation; fail closed on ambiguity or
audit gaps; no credential leakage; no relaxed approval/expiry execution checks;
bounded metric labels; no production tests or automatically resumed paused work.

**Scale/Scope**: Ordinary declarative operator resources, two-replica
leader-election qualification, and existing database support, plus versioned
projects with local sources, owned development targets, and schema/upgrade
suites. No new throughput or fleet-size claim.

## Constitution Check

Authority is [CONSTITUTION.md](../../CONSTITUTION.md), version 1.0.0. The Spec
Kit copy preserves its text with relative links adjusted for its location.

| Gate                                                  | Before research                           | After design                                           | Implementation evidence required                                                           |
| ----------------------------------------------------- | ----------------------------------------- | ------------------------------------------------------ | ------------------------------------------------------------------------------------------ |
| I. Docs, executable demonstration, automated evidence | Pass: each slice has an evidence boundary | Pass: quickstart defines supported/rejected scenarios  | Public operator documentation plus contract/integration/live evidence                      |
| II. Catalog completeness                              | Pass: reuse existing feature areas        | Pass: no new package required                          | Update `examples/catalog.json` with exact existing evidence paths and commands             |
| III. Real supported path and fail-closed boundary     | Pass: observation cannot execute          | Pass: rejection matrix and disposable fixtures defined | Signed release path, exact history checks, redaction, cancellation, and unauthorized cases |
| IV. Evidence ships in the same change                 | Pass: required per slice                  | Pass: no implementation-only slice                     | Docs/catalog/tests/release notes in the same review                                        |
| V. Explicit exceptions                                | Pass: none requested                      | Pass: none needed                                      | Stop if evidence cannot be supplied; no implicit exception                                 |

These are design-compliance results. Runtime qualification remains unexecuted.

## Project Structure

### Documentation

```text
specs/001-operator-reliability/
├── spec.md
├── plan.md
├── research.md
├── data-model.md
├── quickstart.md
├── contracts/operator.md
├── contracts/project-testing.md
└── checklists/requirements.md
```

Beads owns task identities, dependencies, and execution status. Do not create a
parallel Markdown task tracker. This workflow stops after Spec Kit Phase 1
design.

### Implementation surfaces in the inspected baseline

```text
cmd/autosql/main.go                         operator endpoint options
pkg/artifact/                              observation-only verification
pkg/executor/                              read-only history/audit evidence
pkg/operator/                              reconciliation and persistence
pkg/postgres/                              reuse managed catalog inspection
pkg/config/                                versioned project normalization
pkg/dbtest/                                factories, located test cases, runner
pkg/source/                                reuse SQL/JSON/HCL loading
pkg/migrate/                               reuse immutable migration snapshots
pkg/zdm/expandplan/                        concurrent-change planning fence
internal/operatorcontroller/               adapter, status, logs, metrics
internal/cli/production.go                  reuse trusted policy/audit configuration
deploy/operator/                           standalone CRD/ports/probes
deploy/helm/autosql-operator/               mirrored CRD/chart/render tests
docs/kubernetes-operator.md                 diagnosis and recovery workflow
docs/operator-gitops.md                     delivery and collector integration
docs/cli-contract.md                        project/test flags, errors, outputs
docs/database-testing.md                    factory and DSL workflows
examples/project-testing/                   planned released workflow fixture
examples/operator-gitops/                   reproducible deployment examples
examples/catalog.json                      evidence registration
.github/workflows/zdm-postgres-matrix.yml   database qualification
.github/workflows/autosql.yml               unit/envtest/conformance gates
```

**Structure decision**: Extend these implementations and tests. A focused new
file inside an existing package is preferable to a new package, store, or
service.

### Roadmap boundaries

R1 is the operator/reliability milestone. R2 is project configuration and owned
development targets. R3 is file-based testing and bounded release proof.
Requirements FR-001–FR-016 retain the original reliability contract;
FR-017–FR-028 cover the expanded developer workflow. This plan specifies all
three; an individual release may deliver one complete slice with its own
docs/catalog/evidence.

## Phase 0: Research result

[research.md](research.md) records the pinned source, operational evidence,
decisions, alternatives, and limits. No product-policy clarification remains.
Reproduction of the missing-status incident and concurrency failure is an
explicit engineering gate; the plan does not claim their root causes have been
established.

## Phase 1: Design

### Completion recovery and persistence

Create the failing interruption fixtures before the fix: required pre-apply
save, post-apply save, audit completion, Kubernetes status conflict, leader
replacement, and expired release with missing status. Separate a completed
execution with intact audit evidence from SQL completion whose audit evidence is
missing.

Factor common artifact verification into an internal implementation with
distinct execution and completion-observation return types. The public execution
path keeps its exact lifetime rules. Observation permits elapsed artifact
lifetime only after all other checks, including current key and approval
validity, succeed.

Add a read-only evidence reader in `pkg/executor`, sharing step/phase binding
logic without calling history initialization or execution. Factor FileAudit's
existing hash-chain validation into a read-only reader that checks matching
lifecycle completion; the current append-only interface alone is insufficient.
Missing audit storage after a volume loss is a refusal, not reconstructible
proof. Use the same validated chain for the idempotent observation event.

Combine exact expected-step coverage, one unambiguous attempt, no uncertain
rows, target binding, managed-result fingerprint, and durable audit into a pure
decision. Inspect with bounded read-only sessions and existing target
coordination; recheck release/resource/reference identity before committing a
result. Never create or repair database bookkeeping through this path.

Handle relevant Store errors and update FileStore's memory only after successful
file persistence. A successful SQL result followed by a save error must enter
observation on retry, not normal Apply. Persist the observed checkpoint before
publishing success. Refresh status on conflicts, and abandon the result when
UID, generation, release, target, or references changed. Restore status from a
valid checkpoint only after the existing authorization checks; otherwise
re-observe.

### Diagnostics and delivery of signals

Implement [the operator contract](contracts/operator.md) using existing
conditions and recovery fields. Add `completionObservedAt` to both CRD copies
and round-trip tests. Use metadata generation consistently for conditions while
leaving top-level successful completion unchanged on failure.

Initialize structured logging, register metrics, wire endpoint flags through the
existing operator command, and add internal ports/probes/metrics Service
examples. Keep manager health independent of individual schema failures. Verify
transient backoff, deterministic rechecks, watched-reference invalidation,
redaction, bounded labels, and two-replica behavior. Keep failed writes
observable even when the API server cannot accept status.

Update operator/GitOps docs and catalog evidence in this slice. Include
collector configuration and queries, plus a five-minute runbook exercise.
Endpoint emission alone does not prove ingestion by the production monitoring
backend.

### Concurrent-change qualification

Reproduce `TestLiveConcurrentDDLWaitsAndPlanRefusesFreshSnapshot` before
changing its implementation. Register cleanup before assertions; coordinate
sessions using observable lock state rather than timing sleeps. Cover a queued
conflicting DDL, completed DDL, stable schema, unrelated activity, cancellation,
and timeout.

Implement the queued-lock and fresh-inspection boundary described in research.
Limit lock inspection to the target database and inspected relation identities;
avoid query-text inspection. Test with the existing non-superuser fixture.
Preserve the current lock deadlines, post-release fingerprint comparison, and
apply-time validation. If the proposed boundary cannot satisfy minimum-role or
interference tests, stop this slice and revise the design; do not weaken the
assertion or expand database privileges silently.

Run 20 consecutive coordinated trials and stable controls on each PostgreSQL
major 14–18, serially within each database fixture. Use independent disposable
databases for any parallel matrix jobs. Diagnose the unrelated conformance
infrastructure failure separately; both required gates must eventually pass for
promotion.

### R2: Project configuration and disposable factories

Extend `pkg/config` with version-2 JSON/HCL parsing and source ranges,
deterministic bounded environment expansion, HCL source recognition,
tests/factory/policy/registry/ reporting fields, and effective input manifests.
Preserve version-1 decoding and existing precedence. Separate parse/validate,
local input inspection, and purpose-specific runtime resolution so testing never
resolves production targets incidentally. Retain direct flags; add project
selection to schema load/plan and the new test entry points through shared
adapters rather than duplicating configuration.

Compose effective-input identity into the approval bundle and stage configuration attestations for
project-aware release generation and verification, with tamper and legacy
artifact fixtures. Preserve existing production approval rules and artifact wire
format if existing fields suffice. A configuration/report hash alone is not
signed evidence. Policy and registry references are validated and digest-bound
when used; they do not silently enable new network/program sources. Unsupported
combinations fail explicitly.

Wrap the existing `pkg/dbtest` factory/runner with owned disposable leases. Add
a narrow Docker-compatible process adapter and an existing-admin provisioner,
both returning separately credentialed targets. Default to a new database per
case; restrict schema-only mode to compatible schema-local cases. Existing
administrator secrets never reach fixture execution. Validate positive
allowlists and protected resolved identities before mutation, requested/actual
PostgreSQL versions before case execution, and ownership before every cleanup.

Persist incremental leases before resource creation; use bounded startup and
fresh cleanup contexts. Provide `database dev create` and
`database dev cleanup --lease` for explicit lifecycle control, and automatic
cleanup for test commands. Test partial provisioning, identity aliases,
expired/corrupt ownership, cancellation, process interruption/recovery, ten
concurrent same-named cases, and unrelated-resource preservation on Linux/macOS
and PostgreSQL 14–18. Caching covers pinned image bytes only. The exact
contracts are in [project-testing.md](contracts/project-testing.md).

### R3: Test language, public commands, and release proof

Parse versioned `.test.hcl` into located cases in `pkg/dbtest`, using the
existing HCL dependency. Extend the runner for ordered initial-version → seed →
upgrade scenarios, scalar results, exact-SQLSTATE expected errors, filters,
reasoned skips, and bounded parallel scheduling. Keep legacy library callers
working. Reuse migration snapshot ordering and schema/SQL rendering; test-only
execution requires an owned disposable target capability and cannot bypass
production Apply approvals.

Add `schema test` and `migrate test` using existing output/redaction/exit
conventions. Generate human, JSON, and atomic JUnit reports from one ordered
result model, retaining primary, cleanup, and reporting failures. Add structured
suite data to failed JSON envelopes compatibly. Reject empty/all-skipped runs
and unsupported platform claims. Command help and runnable examples are part of
each added command, without claiming the entire `autosql-f2x.7`
command-discovery/completion backlog is complete.

Ship a versioned `examples/project-testing/` fixture and executable driver
through released commands, with local/CI/production configurations, local
SQL/JSON/HCL, schema tests, two migration versions, seed data, one intentional
regression, expected-error behavior, redaction probes, and JSON/JUnit. Validate
the production configuration without using it as a test target. Include a real
signed-artifact qualification scenario separately from fixture test execution.
Capture exact binary and input identities, actual tests executed, cleanup
evidence, and semantic report comparison across local/CI runs. Docs, catalog,
and compatibility notes ship together.

## Delivery order and dependencies

| Milestone/slice                   | Depends on                                                              | Exit evidence                                                                      | Responsible scope                                     |
| --------------------------------- | ----------------------------------------------------------------------- | ---------------------------------------------------------------------------------- | ----------------------------------------------------- |
| R1 persistence/recovery           | Fresh base and reproducers                                              | No SQL replay; recovery/refusal matrix; docs/catalog                               | AutoSQL implementation owner                          |
| R1 diagnostics                    | Can start independently; recovery reasons integrate with recovery slice | Status/log/metric contract; backend ingestion; runbook                             | AutoSQL implementation and production owners          |
| R1 concurrency                    | Deterministic regression reproduction                                   | 20 trials/major; stable/minimum-role/cleanup controls                              | AutoSQL implementation owner                          |
| R2 project configuration          | Reviewed project contract; legacy compatibility fixtures                | Exact selection, provenance, digests, source locations, no validation side effects | AutoSQL CLI/config owner; `autosql-f2x.1`             |
| R2 disposable factories           | Project factory/identity contract                                       | Both modes, version/platform matrix, ownership/fault/parallel cleanup              | AutoSQL testing owner; `autosql-f2x.4`                |
| R3 test DSL and CLI               | R2 configuration and factories                                          | Schema/upgrade/expected-error cases; stable reports; failure semantics             | AutoSQL testing/CLI owner; `autosql-f2x.3`            |
| R3 bounded released workflow      | R2/R3 implemented; R1 qualified for combined release                    | Clean-checkout released-binary proof locally/CI, real signed-path evidence         | AutoSQL release owner; contributes to `autosql-f2x.8` |
| Production operator qualification | Published R1 candidate and authorized integration                       | Rollout, ingestion, supported recovery or explicit refusal                         | `stigen-flux` production owner                        |

R2 development can proceed independently of R1 implementation once the
configuration and identity contract is reviewed. The complete roadmap release
requires both. The existing Beads edge `.4` → `.3` remains valid; `.1` is also a
design dependency for the public workflow. Future task decomposition should
encode these edges in Beads, preserving current ownership. No roadmap formatting
changes Beads state.

Owner labels are responsibilities, not claims that another person accepted work.
Create linked Beads implementation tasks when execution begins; reference the
closed recovery/concurrency predecessors, reuse `.1`/`.4`/`.3`, and narrow the
overlap with `.7`/`.8`. Do not close `.8` until all of its existing dependencies
and broader export/source/visualization acceptance pass. Record any residual
`.1`/`.4`/`.3` acceptance gap explicitly instead of claiming package-level work
fulfills the issue.

## Validation and promotion

[quickstart.md](quickstart.md) defines exact current commands and proposed
regression suite names. Reject a skipped live suite or a pattern matching zero
tests. Run focused tests first, then required repository checks once; repeat
only for failures or subsequent changes. Format/lint changed file types and
validate chart/CRD structure with existing tooling.

After publication, production rollout is a separately authorized integration
step. Capture desired/new/ready replica counts, image digest, previous/current
logs, resource generation/digests/reasons, and 30 minutes of monitoring
ingestion. Use read-only observation for the existing cell resource. If proof is
incomplete or authorization is no longer valid, retain failure and supply the
existing fresh-plan or adoption path for its owner. No automatic SQL retry or
production failure injection.

Stop promotion on any false recovery, unexpected SQL, privilege expansion, stale
plan acceptance, missing required audit, unresolved required CI failure, rollout
regression, or missing monitoring evidence. Keep source, release, deployment,
and qualification status separate. External schema-before-service gating in
`infra-blocks` is a related delivery dependency, not implemented by this
milestone.

### Expanded validation gates

The quickstart includes R2/R3 project compatibility, target identity rejection,
factory fault/parallel cleanup, located test-language failures, stable
JSON/JUnit, and clean-checkout release commands. Add runtime evidence on
Linux/macOS and both factory modes for each supported PostgreSQL major; an
unexecuted platform remains unqualified. Preserve existing required checks and
signed-release integration tests.

Constitution re-check after the scope expansion: all five design gates still
pass. The new workflow stays in existing packages, has a catalog-linked end-user
example, proves both successful and rejected paths, and ships docs/tests/catalog
in the same reviews. No exception is requested. Runtime evidence is still future
work, not a planning validation claim.

## Complexity Tracking

No constitution exception is proposed. R1 adds observation verification,
evidence readers, a status timestamp, metrics, and a planning correction. R2/R3
add normalized project inputs, owned development leases, a small test parser,
two factory adapters, and CLI/report adapters around existing packages. No new
hosted service, production database, test framework, or parallel task tracker is
proposed.
