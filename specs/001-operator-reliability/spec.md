# Feature Specification: AutoSQL Reliability and Developer Workflow Roadmap

**Feature**: `001-operator-reliability`

**Created**: 2026-10-29

**Status**: Implementation in progress; tracked in Beads

**Planning bead**: `autosql-b66`

**Input**: Review AutoSQL performance and the repository backlog, then use Spec
Kit to specify and plan the recommended changes. The user selected the **full
recommended roadmap**: reliability first, followed by unified project
configuration, disposable development databases, schema/migration test files and
commands, and a bounded release workflow proof. This expands the same feature;
its directory and existing requirement IDs remain stable.

## Clarifications

### Session 2026-10-30

- Keep current validity checks for approvals and validation attestations. Do not
  substitute their validity at historical execution time. The existing publisher
  expires automation approval before artifact expiry and validation attestations
  at artifact expiry, so its expired releases remain refused by recovery.
- Recovery success must be demonstrated while all independently required
  authorization and evidence lifetimes remain current. Tolerating artifact
  lifetime alone does not promise recovery of an expired published release.

## User Scenarios & Testing

### User Story 1 - Recover proven completion (Priority: P1)

As a database operator, I need a completed change to recover its accurate status
after interrupted bookkeeping, including after the release expires, without
repeating database changes or weakening release authorization.

**Why this priority**: The production review found confirmed execution history
alongside a failed resource with no recorded applied release. Operators cannot
currently distinguish lost completion records from unfinished work. That
observation does not yet establish either full completion or the incident cause.

**Independent Test**: Interrupt completion recording after a disposable database
finishes an approved change. Restart with empty local state while approval and
validation evidence remain current. Prove accurate recovery with zero additional
application-schema changes. Separately prove an expired release from the current
publisher is refused without SQL replay.

**Acceptance Scenarios**:

1. **Given** a fully completed, still-authorized release whose recorded status
   was lost, **When** the operator reconciles it after expiry, **Then** it
   reports Ready only after checking the release identity, complete execution
   evidence, current managed database state, and required audit evidence.
2. **Given** missing, ambiguous, mismatched, or incomplete execution evidence,
   or current database drift, **When** recovery runs, **Then** it leaves the
   resource not Ready, performs no application-schema changes, and states the
   next safe operator action.
3. **Given** a failure recording progress before execution, **When**
   reconciliation runs, **Then** execution does not start. **Given** a failure
   recording completion after execution, **Then** the failure remains visible
   and later reconciliation can recover from durable evidence without executing
   the change twice.
4. **Given** revoked authorization, a mismatched target, or a release dated in
   the future, **When** recovery runs, **Then** it refuses recovery even when
   the database happens to match the desired state.
5. **Given** a suspended resource, **When** any recovery trigger occurs,
   **Then** suspension remains effective and no database inspection or mutation
   occurs.

### User Story 2 - Explain failures and monitor progress (Priority: P1)

As the on-call operator, I need a resource's status and operational signals to
tell me whether work is applying, completed, waiting for recovery, or blocked,
with a specific next action and without exposing credentials.

**Why this priority**: The review found missing controller logs, no AutoSQL
series in the monitoring backend, and an opaque expiry error after 132 retries.

**Independent Test**: Exercise success, expired incomplete execution, recording
failure, suspension, and temporary connectivity failure. Use only documented
status, logs, and monitoring signals to distinguish them.

**Acceptance Scenarios**:

1. **Given** an expired release with unproven completion, **When** an operator
   inspects it, **Then** the reason identifies expiry and the evidence
   preventing recovery, together with an actionable instruction to obtain a
   fresh plan or use the existing authorized adoption workflow.
2. **Given** a temporary dependency outage, **When** retries occur, **Then** the
   cause remains visible, retries are bounded, and successful recovery clears
   the current failure without erasing historical failure counters.
3. **Given** an unchanged release that deterministically cannot proceed,
   **When** it remains blocked, **Then** it does not generate an unbounded
   stream of database work or repeated warning events; a relevant resource or
   credential change can trigger a prompt re-evaluation.
4. **Given** unavailable monitoring, **When** a deployment is qualified,
   **Then** the evidence records the monitoring gap and does not report a
   healthy history from missing data.

### User Story 3 - Refuse a plan invalidated by concurrent changes (Priority: P1)

As a release owner, I need planning to reject a schema snapshot invalidated
during the planning window so that approval never relies on known stale
information.

**Why this priority**: The reviewed PostgreSQL 18 qualification run failed the
existing concurrent-change rejection test.

**Independent Test**: Deterministically coordinate planning and a second
database session that changes the relevant schema. Prove rejection when the
change crosses the planning boundary, and success when no conflicting change
exists.

**Acceptance Scenarios**:

1. **Given** a conflicting change has completed within the protected planning
   window or is waiting at its final boundary, **When** planning attempts to
   finish, **Then** it refuses the snapshot with a retryable explanation.
2. **Given** a stable schema and no competing change, **When** planning runs,
   **Then** it succeeds within its existing time limits.
3. **Given** cancellation, timeout, or a failed assertion in the demonstration,
   **When** it finishes, **Then** all sessions and locks it created are
   released.
4. **Given** a valid plan followed by a later schema change, **When**
   application begins, **Then** existing apply-time validation continues to
   reject stale work; successful planning is not a promise that the database
   remains unchanged.

### User Story 4 - Configure a project once (Priority: P2)

As a developer, I need one versioned project definition to select the same named
environment, sources, policies, and test settings locally and in CI without
copying credentials or silently changing which database a command targets.

**Why this priority**: Configuration and environment selection connect the
existing components into a repeatable workflow and are prerequisites for the
testing work.

**Independent Test**: Validate a local/CI/production example, select each
environment without opening a database, and compare the effective configuration
identity across machines. Existing version-1 configuration examples must
continue to work.

**Acceptance Scenarios**:

1. **Given** named or deterministically generated environments, **When** one is
   selected, **Then** exactly one environment is resolved; unknown names,
   duplicate generated names, and ambiguous selection fail with source
   locations.
2. **Given** file, environment, and command overrides, **When** configuration is
   resolved, **Then** their documented precedence and non-secret provenance are
   visible, and changing an execution-relevant input changes its identity.
3. **Given** a project containing production secret references, **When**
   ordinary configuration validation or test discovery runs, **Then** no
   production secret is resolved and no database or external program is
   contacted.
4. **Given** existing configuration and direct command invocations, **When** the
   new version is installed, **Then** those supported paths retain their
   documented behavior; use of newly unsupported or unknown settings fails
   explicitly.

### User Story 5 - Obtain and clean up disposable databases (Priority: P2)

As a developer or CI owner, I need a single command to provision an isolated
test target for a supported database version, run my work, and clean up only the
resources owned by that run, including after partial failure.

**Why this priority**: Schema and migration tests must never rely on a shared
production target or leave interfering state for the next run.

**Independent Test**: Provision two simultaneous runs through both supported
factory modes, prove their isolation, inject readiness failure and cancellation,
and check ownership-scoped cleanup without touching an unrelated database.

**Acceptance Scenarios**:

1. **Given** a configured container factory or an explicitly allowed development
   administration target, **When** a run starts, **Then** it receives a uniquely
   identified, ready target with the requested version and restricted
   credentials.
2. **Given** a production environment/reference, an unapproved administration
   target, or a protected target alias, **When** provisioning or testing is
   requested, **Then** it refuses before any database mutation.
3. **Given** setup failure, test failure, timeout, or graceful cancellation,
   **When** cleanup runs, **Then** all owned resources are removed within the
   cleanup budget or the command fails with a precise remaining-resource record.
4. **Given** an abruptly killed process, **When** an operator uses its recorded
   run identity for cleanup, **Then** only ownership-verified resources are
   removed; unknown or mismatched ownership refuses deletion.

### User Story 6 - Test schemas and migration upgrades from files (Priority: P2)

As a schema author, I need readable test files to set up fixtures, seed an older
version, apply an upgrade, check results or expected errors, and produce useful
local and CI reports.

**Why this priority**: This turns the existing test-library building blocks into
an end-user regression workflow and makes data-preserving upgrades reviewable.

**Independent Test**: Run the same fixture locally and in CI with a successful
upgrade, a deliberately broken upgrade, an expected-error case, and a cleanup
failure. Results must identify the exact case and source location.

**Acceptance Scenarios**:

1. **Given** a schema test, **When** it runs, **Then** setup, schema
   preparation, fixtures, ordered checks, and cleanup occur on its own
   disposable target.
2. **Given** a migration test with an initial version, seed data, and an upgrade
   version, **When** it runs, **Then** it proves both upgrade behavior and
   preservation of the asserted data; unsupported or out-of-order versions fail
   explicitly.
3. **Given** scalar assertions, expected database errors, filters, or reasoned
   skips, **When** tests run, **Then** human and CI output consistently
   distinguish passed, failed, skipped, timed-out, and cleanup-failed cases.
4. **Given** syntax errors, zero selected/runnable cases, or a production
   reference, **When** testing is requested, **Then** it fails before fixture
   execution and never reports a misleading green run.
5. **Given** concurrent cases, **When** any case fails, **Then** others retain
   their isolation, and all started cases still attempt bounded cleanup.

### User Story 7 - Reproduce the released workflow (Priority: P3)

As a release owner, I need one versioned example that proves the recommended
project-and-testing workflow using a released binary from a clean checkout.

**Why this priority**: Package tests alone cannot demonstrate that users can run
this workflow through the documented commands.

**Independent Test**: A clean local checkout and CI run select the same project,
provision a disposable target, run schema and upgrade tests, and produce
comparable configuration/test identities and reports without leaking secrets.

**Acceptance Scenarios**:

1. **Given** a release candidate and the versioned example, **When** its
   documented workflow runs locally and in CI, **Then** all required cases
   execute, results are attributable to the same source/configuration inputs,
   and cleanup is verified.
2. **Given** changed configuration, migration bytes, missing required cases,
   leaked secrets, or an unsupported platform, **When** qualification evaluates
   the run, **Then** it rejects or explicitly reports unsupported evidence.
3. **Given** this bounded workflow passes, **When** backlog progress is
   reported, **Then** it does not claim completion of the broader
   export/visualization/source ecosystem work still required by the full
   developer-experience epic.

### Edge Cases

- SQL completes but audit delivery, local persistence, or public status
  recording fails independently; recovery must not conceal an unresolved audit
  failure.
- A replacement leader has no local state, or a stale worker tries to publish
  completion for a resource whose generation or target has changed.
- Execution history is absent, duplicated, tampered with, from a different
  attempt, or contains an intended step without confirmation.
- Desired state matches by coincidence but the expected execution cannot be
  proven.
- A release expires during reconciliation, a signing key is revoked, or current
  authorization expires independently of the release.
- A healthy resource encounters expiry after completion was already recorded;
  preserve the existing validated behavior for that case.
- Bootstrap and versioned migration resources reach expiry; their existing
  recovery rules remain in force, with no automatic inference from the
  declarative execution history.
- Two leaders overlap briefly or a status write conflicts with a resource
  update.
- A blocked resource shares an operator with healthy resources; its failure must
  not make the operator restart continuously.

- Configuration uses a target alias that resolves to a protected production
  endpoint, or a CI environment variable silently overrides the project file.
- Concurrent tests use identical case names, explicit schema names, or migration
  histories; isolation must include those names, not only a changed search path.
- A fixture emits credential-like database error text or fails during cleanup.
- A process is killed after provisioning but before receiving a ready result;
  its durable ownership record must still allow a bounded cleanup procedure.
- A filter selects no tests, every test is skipped, an expected error is too
  broad, or a report cannot be written; none may silently qualify a release.

## Requirements

### Functional Requirements

- **FR-001**: Recover missing completion status only for ordinary declarative
  releases with independently proven completion; exclude bootstrap and versioned
  migration recovery from this milestone.
- **FR-002**: Validate the release's integrity, target, desired state, and
  current trust and authorization rules before accepting recovery evidence.
  Permit elapsed release lifetime solely for observing completion, never for
  authorizing execution.
- **FR-003**: Require exact correspondence between the expected change and its
  confirmed execution history, with no unexplained or uncertain steps, and
  require the current managed database state to match the approved result.
- **FR-004**: Recovery MUST NOT execute application SQL, create or repair
  history tables, or infer completion from the number of history rows alone.
- **FR-005**: Required audit evidence must be durable before publishing
  recovered completion. Record that completion was observed later, without
  inventing the original completion time or hiding an unresolved execution
  uncertainty.
- **FR-006**: Propagate progress and completion persistence failures. Failed
  writes must not become successful in-memory checkpoints or Ready status.
- **FR-007**: Recovery must be repeatable after restart or leader replacement,
  and completion updates must refer to the current resource generation and
  release.
- **FR-008**: Preserve suspension, validated already-converged behavior,
  existing approval controls, and safe behavior for resources outside recovery's
  scope.
- **FR-009**: Expose stable, distinct reasons for expiry, invalid evidence,
  drift, persistence failure, authorization failure, and temporary dependency
  failure. Include safe next actions in human-readable diagnostics.
- **FR-010**: Provide structured operational logs and measurable reconciliation
  outcomes, durations, and recovery outcomes. Do not emit credentials,
  connection strings, full SQL, or unrestricted user-controlled monitoring
  labels.
- **FR-011**: Bound retries and repeated warnings for unchanged deterministic
  failures, while re-evaluating relevant changes and recovering from temporary
  outages without manual operator restarts.
- **FR-012**: Ship reproducible monitoring setup and demonstrate both local
  signal emission and backend ingestion before claiming production
  observability.
- **FR-013**: Reject planning snapshots invalidated by conflicting schema
  changes within the protected planning window, including a change waiting at
  its final boundary; preserve stable-schema success and apply-time validation.
- **FR-014**: Prove concurrent-change behavior on every currently supported
  PostgreSQL major version, with deterministic synchronization, bounded runtime,
  and reliable cleanup. Do not weaken a rejection assertion to make a test pass.
- **FR-015**: Deliver documentation, runnable supported and rejected scenarios,
  feature-catalog coverage, and compatibility/release notes with implementation.
- **FR-016**: Distinguish implemented, tested, published, deployed, and
  operationally qualified states. Stop promotion when required evidence is
  missing or fails.

- **FR-017**: Provide a versioned project definition with named and bounded,
  deterministic generated environments, source inputs, target/development
  references, migration directory, tests, policy, registry references, and
  reporting settings.
- **FR-018**: Select exactly one environment; retain documented override
  precedence, report source locations and override provenance, and bind
  execution-relevant effective configuration and input bytes to a reproducible
  identity.
- **FR-019**: Retain supported version-1 configurations and direct CLI
  invocations. Validate unknown or unsupported settings explicitly. Ordinary
  validation and test discovery must not resolve production secrets, run
  programs, or open databases.
- **FR-020**: Provision disposable PostgreSQL 14–18 targets through container
  and approved existing-admin factories, with unique ownership, version
  verification, bounded readiness, and separate restricted execution
  credentials.
- **FR-021**: Refuse production environments/references and protected target
  aliases before mutation. Only explicitly authorized development factories may
  provision; a name containing "test" alone is not authorization.
- **FR-022**: Clean owned resources on success and ordinary
  failure/cancellation; preserve primary and cleanup failures. Abrupt
  termination must leave a durable, credential-free ownership record and an
  explicit, ownership-checked cleanup path.
- **FR-023**: Support file-based schema and migration tests with setup,
  fixtures, seed-before-upgrade, ordered version transitions, execution steps,
  typed scalar assertions, exact expected errors, reasoned skips, filters, and
  cleanup.
- **FR-024**: Run each started case in an isolated target with bounded
  parallelism. Failures must retain file/line/column and stage, without secrets
  or uncontrolled database error text. Test execution must not weaken production
  apply authority.
- **FR-025**: Provide stable human, JSON, and JUnit reports, deterministic case
  ordering, and documented exit behavior. Empty selections, all-skipped suites,
  infrastructure failures, and report/cleanup failures must not appear as
  success.
- **FR-026**: Provide help and executable examples for every added command/flag,
  using existing CLI output/error conventions. Do not require a new hosted
  service.
- **FR-027**: Qualify a versioned clean-checkout project/testing example through
  released commands locally and in CI, recording input identities, binary
  identity, supported platform/version, actual case counts, reports, redaction,
  and cleanup.
- **FR-028**: Keep bounded roadmap delivery distinct from the broader existing
  source/export/visualization/CLI epic; do not close a Bead whose remaining
  acceptance criteria or dependencies are not met.

### Key Entities

- **Release**: An approved change bound to a target, desired result, identity,
  and validity period.
- **Execution evidence**: Durable records identifying which exact steps
  completed for a particular release and attempt, including uncertainty and
  audit evidence.
- **Completion observation**: A later, read-only determination that a release
  completed and its approved result still exists.
- **Resource status**: The current generation's progress, readiness, recovery
  outcome, and actionable failure reason.
- **Planning window**: The bounded period in which the system observes and
  validates the database state used to produce a plan.

- **Project environment**: A named effective configuration with source
  locations, secret references, explicit overrides, and a reproducible input
  identity.
- **Disposable target lease**: A run-owned resource identity, factory, verified
  version, lifetime, and cleanup state; credentials remain transient.
- **Test suite and case**: Located, ordered setup/upgrade/assertion/cleanup
  operations plus selection, skip, and timeout rules.
- **Test report**: Ordered outcomes bound to project/test/migration and binary
  identities, with separate primary and cleanup failures.

## Success Criteria

### Measurable Outcomes

- **SC-001**: Every supported completion-loss scenario restores accurate
  readiness within two minutes after dependencies become healthy, with zero
  repeated application-schema changes. Measure in the disposable qualification
  fixture.
- **SC-002**: Every invalid, incomplete, unauthorized, wrong-target, or drifted
  recovery case stays not Ready and performs zero application-schema changes.
- **SC-003**: An operator following the runbook can identify the cause and next
  action for all five demonstrated failure classes within five minutes, without
  inspecting source code or seeing secret material.
- **SC-004**: Every supported database version passes 20 consecutive coordinated
  concurrent-change trials and the corresponding stable-schema controls; no
  trial leaves a session or lock behind after its deadline.
- **SC-005**: A 30-minute deployment qualification records reconciliation and
  recovery outcomes in the monitoring backend with no gap longer than two
  configured collection intervals. Missing data fails this criterion.
- **SC-006**: Each delivered behavior has catalog-linked runnable evidence for
  its supported path and material rejection boundaries before release
  qualification.

- **SC-007**: Equivalent projects produce identical effective input identities
  locally and in CI; every execution-relevant override or input-byte change in
  the qualification matrix changes that identity. All legacy compatibility
  fixtures pass.
- **SC-008**: Both factory modes pass success, partial-startup, cancellation,
  timeout, and ownership-mismatch trials on supported platforms and database
  majors 14–18. Ten simultaneous same-named cases neither share writable state
  nor delete another run's resources. Every residual resource is reported with
  its owner after failure.
- **SC-009**: The released example passes the same required schema and upgrade
  cases locally and in CI; a seeded broken upgrade fails at the intended source
  location. No empty/all-skipped run, cleanup failure, or report failure
  qualifies as success.
- **SC-010**: Qualification fixtures expose zero resolved secrets in
  configuration, desired state, reports, logs, errors, or cleanup records,
  including injected secret-bearing database errors and rejected production
  targets.

## Assumptions

- The user selected the full recommended roadmap. Reliability is milestone R1;
  project configuration (`autosql-f2x.1`) and disposable factories
  (`autosql-f2x.4`) are R2; test DSL/CLI (`autosql-f2x.3`) and a bounded release
  proof are R3. Delivery may be incremental, but this specification covers all
  three milestones.
- The bounded release proof contributes to `autosql-f2x.8`; it cannot close that
  broader Bead without its source runtime, export, visualization, and
  command-surface dependencies. Full `autosql-f2x.2`, `.5`, `.6`, and `.7` scope
  remains later work.
- The initial development-factory support is Linux and macOS with a
  Docker-compatible daemon or an explicitly configured development PostgreSQL
  administrator. Other operating systems are unsupported until independently
  qualified; production Kubernetes support is unchanged.
- Container image caching may reuse pinned images, never writable test
  databases. Process crashes cannot guarantee immediate cleanup; residual
  ownership and safe recovery must be observable. No background cleanup service
  is introduced.
- Current execution history and managed-state inspection remain the evidence
  sources. Recovery does not introduce a new control plane or database.
- Existing authorized re-plan and adoption workflows remain the escape route
  when completion cannot be proven. No automatic production repair is authorized
  by writing this specification.
- The incident's missing-status cause remains unproven. Implementation must
  first reproduce the relevant interruption boundaries; the observed history
  count alone does not establish that the production resource is safe to
  recover.
- Deployment integration in `stigen-flux` and release gating in `infra-blocks`
  have their own owners and approval paths. This repository can supply
  compatible deployment examples and evidence but cannot certify another
  repository's rollout.
- Native integration publication remains explicitly deferred (`autosql-45s`).
  New dialects, hosted services, a broad diagnostic CLI, and general
  schema-feature expansion are outside this milestone.
