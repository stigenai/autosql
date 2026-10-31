# Research: AutoSQL Reliability and Developer Workflow Roadmap

## Evidence and limits

The October 29, 2026 review observed both production operators available at 2/2
on v0.1.52. Auth, global, and billing-sandbox were Ready. Cell was Failed with
retry count 132, no applied digest, observed generation 0, and an
`artifact time` error. Its database contained 50 confirmed history rows. The 25
maintenance policies inspected no longer granted the application role access.
Neither a full comparison with the signed desired state nor the original
bookkeeping failure was established.

The same review found the controller logger initialization warning and no
AutoSQL series in the monitoring backend over seven days. Operator availability
recovered from an earlier interruption; that is not evidence of application
database downtime. These are point-in-time review observations, not measurements
refreshed while writing this specification.

The implementation baseline inspected for planning is upstream main
[`588dafe19127ebdf9e28afbd2707ad3cd8184cd1`](https://github.com/stigenai/autosql/tree/588dafe19127ebdf9e28afbd2707ad3cd8184cd1),
associated with v0.1.53. The user's working checkout is older (`c08aed0`) and
dirty. A separate, read-only source snapshot supplied the current implementation
details; the working checkout was not upgraded. Revalidate the baseline when
implementation starts, and apply changes in an isolated checkout.

| Evidence                                                                                                                                                                               | Consequence                                                                                                             |
| -------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- | ----------------------------------------------------------------------------------------------------------------------- |
| `internal/operatorcontroller/controller.go` verifies expiry before normal reconciliation; its existing converged-expiry path requires recorded applied digest and generation           | Missing status cannot use that path; preserve it while adding a distinct observation path                               |
| `pkg/operator/operator.go` ignores several `Store.Save` errors, including completion persistence                                                                                       | A confirmed defect to cover, but not a proven cause of the production incident                                          |
| `pkg/operator/file_store.go` updates memory before committing the file                                                                                                                 | A failed disk write can leave an optimistic in-memory checkpoint                                                        |
| `pkg/artifact/artifact.go` validates trust, bindings, approvals, and signatures before lifetime; `ErrExpired` also covers future-created artifacts                                     | Elapsed lifetime must be distinguished from all other verification failures                                             |
| `pkg/executor/executor.go` validates detailed confirmed-step bindings; normal execution also creates/repairs history storage                                                           | Reuse binding validation without invoking the mutating executor                                                         |
| Executor completion can fail because durable lifecycle audit delivery failed                                                                                                           | Confirmed SQL history is insufficient to declare completion by itself                                                   |
| `pkg/zdm/expandplan/live.go` releases planning locks before its fresh inspection                                                                                                       | A scheduling race is a plausible contributor to the observed stale-plan failure; reproduce before attributing the cause |
| [PostgreSQL 18 failure](https://github.com/stigenai/autosql/actions/runs/38910370165/job/117004377222) reports a stale plan in `TestLiveConcurrentDDLWaitsAndPlanRefusesFreshSnapshot` | Keep the safety assertion and qualify the fix across versions 14–18                                                     |
| [Conformance failure](https://github.com/stigenai/autosql/actions/runs/38910370049/job/117004375673) was an Argo CD Redis readiness timeout                                            | Track infrastructure failure separately from the planning regression                                                    |

Paths in this table refer to the pinned baseline, not necessarily the older
local checkout. Evidence contains no credentials or production SQL payloads.

## Decision 1: Separate observation from execution authority

**Decision**: Add a purpose-specific verified completion-observation value. It
shares existing cryptographic and binding validation but cannot be passed to the
executor. It permits only an elapsed artifact lifetime, not a future creation
time, revoked/expired signing authority, invalid attestation, or expired
approval proof. Current policy is authoritative. Leave `VerifyTrusted` execution
behavior unchanged.

**Rationale**: A release can be too old to execute yet still describe completed
work. A generic `ignoreExpiry` switch or a normal execution token would expand
mutation authority accidentally. Some old releases will remain unrecoverable
under current approval policy; that is intentional.

**Alternatives considered**: Ignoring all verification errors; replaying the
expired release; automatically adopting any matching database. All lose
necessary authorization or execution provenance. A newly signed release or
explicit adoption remains an operator-directed alternative.

## Decision 2: Reconstruct from exact evidence, not local state

**Decision**: Limit automatic reconstruction to ordinary declarative,
non-bootstrap releases. Read existing history using a read-only session; require
one unambiguous completed attempt, exact expected executable-step coverage and
bindings, no unresolved intended steps, and the signed managed result
fingerprint. Empty plans, topology steps, ambiguous attempts, legacy rows
lacking bindings, and missing history are not eligible in the first version.
Reuse the managed-state filtering already used by artifact application.

Require durable lifecycle audit evidence. If an existing required audit record
is missing or its sink cannot establish durability, refuse recovery and retain
the existing audit-repair guidance. Record a separate, idempotently identifiable
`completion_observed` event only after that prerequisite; do not fabricate an
original `completed` event or its timestamp.

**Rationale**: This survives a lost local volume without treating a matching
schema or row count as execution proof. Conservative scope avoids mixing
bootstrap's separate journal with migration history or silently repairing audit
failures.

**Alternatives considered**: Make persistent local storage the only recovery
path; trust the Kubernetes status; reconstruct bootstrap and every migration
kind at once. These either retain the failure mode or expand the milestone
unnecessarily.

## Decision 3: Make persistence failures real failures

**Decision**: Handle every relevant state-save error. Do not execute after a
failed required pre-execution save. Publish the FileStore map only after the
atomic file update succeeds. Completion-save failure returns a typed persistence
failure; a subsequent retry must evaluate durable execution evidence rather than
blindly call Apply. Refresh and conditionally update Kubernetes status against
the resource UID, generation, digest, target, and relevant reference versions.

**Rationale**: Fixes two independently visible code weaknesses. Status conflicts
must retry publication, not database execution. File durability is bounded to
the existing local-store contract; loss of the entire volume is handled through
observation, not by adding a new storage service.

**Alternatives considered**: Best-effort persistence, a new database, or
trusting an in-memory success after disk failure. None meets the observed
recovery need.

## Decision 4: Reuse current status, logging, and metrics infrastructure

**Decision**: Reuse status conditions and recovery fields; add only an optional
`completionObservedAt` timestamp. Initialize the controller logger before
manager creation. Use controller-runtime's existing metrics registry and the
existing Prometheus dependency. Provide bounded outcome labels and internal
metrics/health endpoints with compatible chart and standalone-manifest wiring.

No new diagnostic CLI or dashboard service is required. The runbook combines
resource status, structured logs, and documented monitoring queries. Production
ingestion requires a separate `stigen-flux` integration change because its
manifests do not simply inherit this repository's Helm defaults.

**Rationale**: Most user-facing diagnostic fields already exist. The missing
signal delivery needs a small implementation and explicit end-to-end
verification.

**Alternatives considered**: A new observability stack; a broad `diagnose`
command; per-resource/digest metric labels. They add maintenance or unbounded
cardinality.

## Decision 5: Preserve the concurrency assertion and strengthen its boundary

**Decision**: First reproduce the existing failure with deterministic session
coordination. While planning locks are held, reject a conflicting relation-lock
request queued against the inspected relation set. Compare a fresh managed
catalog snapshot as well as the existing snapshot, retaining the post-release
fingerprint check and apply-time preconditions. Inspect lock identity/mode and
blocked process identity, not arbitrary query text or a sleep-based assumption.
Inability to inspect the necessary evidence is a refusal, not permission to
accept the snapshot.

Validate with the existing minimum-role fixture before accepting this approach;
do not grant monitoring superuser privileges to make it work. Register
cancellation, session closure, and goroutine cleanup before any assertion can
terminate a test.

**Rationale**: Re-reading within repeatable read cannot by itself prove that no
concurrent change exists. Releasing a lock and immediately reading again does
not prove that a queued change has already finished. The targeted queued-lock
check addresses that gap without promising perpetual schema stability.

**Alternatives considered**: Longer sleeps, removing the test, table-wide
exclusive planning locks, or assuming advisory locks constrain external DDL.
Reject these. This design is a hypothesis to qualify, not a claim that the cause
has been proven. If the minimum-role or interference tests disprove it, stop
that slice and revise the design with evidence before proceeding.

## Decision 6: Deliver R1 in three slices with a shared qualification gate

**Decision**: Build persistence/recovery first, observability second, and the
concurrency correction as an independent slice once its reproducer exists. Every
slice includes its own documentation, example/catalog updates, and tests. Only
promote the combined release after current main, matrix, and deployment gates
pass.

**Rationale**: These resolve demonstrated trust and operational gaps before
broader feature development. R1 needs no new language, runtime, service, or
database.

**Backlog relationships**: Recovery follows closed `autosql-ptg.3`,
`autosql-q40`, and `autosql-v00`; concurrency follows closed `autosql-0si`;
diagnostic scope overlaps only a bounded part of `autosql-f2x.7`. Do not reopen
or claim all of those issues automatically. Project configuration, factories,
tests, and bounded release proof are specified below as R2/R3. Broader Beads
retain their remaining acceptance criteria; `autosql-45s` stays deferred.
Executable implementation tasks and dependencies belong in Beads when the user
starts that phase.

## Decision 7: Extend the project model and preserve legacy commands

**Evidence**: The pinned baseline already has `pkg/config.Config`, named JSON
version-1 environments, secret references, override precedence, and preflight.
`config validate`, `migrate apply`, and `migrate status` consume it.
Configuration accepts SQL/JSON source kinds while the schema command already
accepts HCL. `Config.Preflight` resolves target and development references
together, which is unsuitable for test discovery that must not resolve
production secrets.

**Decision**: Add version-2 JSON/HCL decoding to one normalized model, located
errors, bounded deterministic environment expansion, HCL source support,
tests/factories, and policy/registry/reporting references. Separate parse,
effective-input inspection, and purpose-specific secret resolution. Preserve
version 1 and direct flags; require explicit version 2 for new fields. Bind
effective inputs through existing stage configuration digests for project-aware
releases and verify those bindings on apply.

**Rationale**: Reusing selection and precedence avoids a second incompatible
configuration system. Source locations and distinct configuration/input
identities make overrides and byte changes reviewable. New project-aware
releases must prove binding, while legacy releases retain their old validation
contract.

**Alternatives considered**: Replace JSON with HCL immediately; infer an
environment; resolve all credentials during validation; call a report digest an
approval binding. Reject these because they break compatibility or hide
authority changes. Full dynamic source evaluation (`autosql-f2x.2`) is not
necessary for the bounded local-file workflow.

## Decision 8: Add factories around existing lifecycle interfaces

**Evidence**: `pkg/dbtest` already defines Factory/Database, a Runner, located
cases, setup/fixtures/seeds, named versions, integer-count assertions, and
bounded reverse cleanup. `PostgresFactory` creates a random schema on a supplied
DSN; it does not provision a container, create a fresh database per case,
enforce target identity, or prevent explicit-schema SQL from touching another
case.

**Decision**: Preserve the library interfaces where possible, add owned database
leases and container/existing-admin implementations, and default end-user tests
to fresh databases with restricted roles. Retain schema isolation only as an
explicit limited mode. Use the existing pgx dependency and a narrow Docker CLI
process adapter with explicit arguments and cancellation, rather than adding an
engine SDK or service. A Docker-compatible daemon is an explicit new end-user
prerequisite for the container mode, justified by the requested disposable
container workflow; the admin mode does not require it. Initial factory
qualification covers Linux and macOS.

**Rationale**: Existing lifecycle and cleanup code is reusable, but random
schema names alone do not isolate versioned migrations or fully qualified
fixture SQL. Durable ownership records allow safe cleanup after abrupt process
termination without promising cleanup can run inside a killed process. Cache
image bytes, not dirty data.

**Alternatives considered**: Run tests on the configured target; assume a dev
label is a safety boundary; execute fixtures with the administrator; delete all
prefixed resources. Reject all four. Existing-admin targets must be explicitly
authorized and protected identities checked before mutation.

## Decision 9: Parse a small test language into the existing runner

**Evidence**: The runner supports located ordered operations and per-version
checks, but lacks a file parser, typed scalar results, expected-error
operations, discovery, filtering/skips, public test commands, and JUnit output.
The CLI already defines `autosql.output/v1`, a redactor, and shared exit
categories.

**Decision**: Add versioned `.test.hcl`, `schema test`, and `migrate test`
through existing config/source/migration/runner/output components. Keep test
execution behind an owned disposable target capability. Extend scalar and
exact-SQLSTATE assertions, add bounded scheduling, and retain primary plus
cleanup errors. Use additive JSON results and atomic JUnit files, with
deterministic case order and nonzero outcomes for empty/all-skipped suites or
report failure.

**Rationale**: This provides the requested user workflow without rewriting
production Apply or disguising a test credential as production approval. The
language is bounded and does not add arbitrary shell hooks. Real signed-path
qualification remains separate.

**Alternatives considered**: Build a second migration engine; shell out to an
unrestricted script per test; relax artifact approval for tests; treat skipped
suites as success. None is needed to expose the existing library safely.

## Decision 10: Specify the full recommended sequence, keep epic accounting exact

**Evidence**: Current Beads show `autosql-f2x.3` blocked by `.4`. The `.8`
release-proof Bead depends on `.1` through `.7`, including a source runtime,
exporters, interactive visualization, and whole-CLI improvements. Its acceptance
is broader than the recommended initial release proof.

**Decision**: R1 delivers reliability; R2 delivers project configuration and
factories; R3 delivers the file-based test workflow and a clean-checkout release
proof. R2 can be developed independently once its contracts are reviewed; R3
depends on R2, and the combined candidate depends on R1 qualification. Cover the
recommended `.1`, `.4`, and `.3` work with their remaining criteria explicitly
mapped; contribute a bounded proof to `.8` without closing that broader issue.
Full `.2`, `.5`, `.6`, and `.7` remain later ecosystem work and native
publication stays deferred.

**Rationale**: The user selected the full recommended roadmap, including
configuration and tests. That selection does not imply completing every
unresolved issue in the repository or silently changing the acceptance criteria
of an existing epic.

**Alternatives considered**: Keep the reliability-only spec; claim the whole f2x
epic from a smaller example; expand immediately to every exporter, dialect, and
hosted integration. The first misses the user's selection; the latter two
obscure scope.
