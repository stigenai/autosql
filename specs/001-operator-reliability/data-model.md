# Data Model: AutoSQL Reliability and Developer Workflow Roadmap

The R1 completion model extends existing artifact, execution, and resource
models without adding a history table or changing the signed artifact format.
R2/R3 add the project, disposable-target, and test models below. Project-aware
attestation binding reuses existing configuration-digest fields with explicit
compatibility tests.

## Completion observation input

An internal immutable input binds the resource UID, namespace/name, metadata
generation, artifact digest, target identity, desired-state fingerprint, and
resolved reference versions to a verified observation-only artifact. It is
transient and contains no persistable credentials.

Validation requires current trust, signature, approval, policy, and target
checks. Only the artifact lifetime may have elapsed. Future-created releases and
invalid approval/key lifetimes remain invalid. This input is not convertible to
a verified execution artifact through an exported constructor.

## Execution evidence

Reuse existing `autosql_migration_history` records and binding checks:

| Field group                                | Required invariant                                                 |
| ------------------------------------------ | ------------------------------------------------------------------ |
| Artifact, execution, plan, bundle digests  | Match the verified release exactly                                 |
| Target identity and environment            | Match both the release and resolved target                         |
| Attempt                                    | Exactly one eligible completed attempt; ambiguity refuses recovery |
| Step identity/hash and phase identity/mode | Match every expected executable step exactly                       |
| State                                      | All expected steps confirmed; no unresolved intended step          |
| Managed fingerprint                        | Fresh, read-only inspection matches the signed result              |
| Audit evidence                             | Required lifecycle records are durably present and attributable    |

Unknown or duplicate rows, missing binding columns, empty/topology plans, and
bootstrap/versioned executions produce an ineligible result. Read failures are
distinguished from invalid evidence. No history initialization or repair occurs.

## Completion observation result

Use a discriminated internal result rather than a boolean:

| Result     | Meaning                                                    | Mutation authority                      |
| ---------- | ---------------------------------------------------------- | --------------------------------------- |
| Proven     | All identity, history, state, and audit predicates passed  | May persist observation and status only |
| Ineligible | Resource kind or evidence shape is unsupported             | None                                    |
| Refused    | Evidence, trust, target, or current state fails validation | None                                    |
| Retryable  | A temporary dependency prevents the decision               | None                                    |

`Proven` carries artifact/plan/target identities, confirmed step count,
fingerprint, attempt, observation time, and an audit event identity. Derive the
audit identity from resource UID, generation, artifact digest, attempt, and
event kind. Repeated delivery is deduplicated where the sink supports it;
otherwise identical event identity denotes the same observation, not another
execution. Failure to establish required durable audit evidence prevents Ready.

## Public resource status

Reuse `conditions`, `observedGeneration`, `appliedDigest`, `planDigest`,
`targetIdentity`, `executionID`, `recoveryState`, `recoveryGuidance`,
`appliedSteps`, `retryCount`, and `appliedFingerprint`. Add optional RFC3339
`completionObservedAt`; it means the time completion was verified later, not
when the SQL originally finished. Do not add a guessed historical completion
time.

Keep existing recovery-state values. Successful recovery sets `recoveryState` to
`none`, Ready=True with reason `CompletionRecovered`, clears current failure and
guidance, and publishes the matching generation/digest/fingerprint together. An
old applied digest may remain historical while a newer generation fails;
consumers must check Ready and observed generation, not just digest presence.

Kubernetes metadata generation is authoritative when present. The existing spec
generation fallback remains for non-Kubernetes callers. Conditions recording a
failure identify the generation evaluated but must not advance the top-level
successful-completion checkpoint.

## State transitions

| From                           | Evidence/action                                         | To                                                |
| ------------------------------ | ------------------------------------------------------- | ------------------------------------------------- |
| Any                            | Resource suspended                                      | Suspended; no database reads or changes           |
| Already converged              | Existing validated hold checks pass                     | Ready, existing reason preserved                  |
| Missing completion             | Eligible artifact verified for observation              | Inspecting evidence; not Ready                    |
| Inspecting evidence            | Evidence/ref authorization fails                        | Failed with reason and safe guidance              |
| Inspecting evidence            | Temporary dependency failure                            | Failed, bounded retry                             |
| Inspecting evidence            | Completion and audit proven                             | Persist observation checkpoint                    |
| Persist observation checkpoint | Save fails                                              | Failed; no optimistic in-memory success           |
| Persist observation checkpoint | Save succeeds, current resource still matches           | Publish Ready/CompletionRecovered                 |
| Publishing status              | Conflict but same resource identity/bindings            | Retry publication only                            |
| Publishing status              | UID, generation, target, release, or references changed | Discard old result and reconcile current resource |

No transition from observation grants execution authority. A later fresh release
uses the existing execution path, including its normal approval and preflight
gates.

## Project definition and effective environment

A `ProjectDefinition` contains its format version, declared non-secret
variables, named environments, bounded environment sets, development factories,
tests, and policy/registry/reporting references. Each parsed field has a source
range stored separately from the canonical value. Version-1 JSON converts into
the existing compatible model; version-2 JSON/HCL converts into the extended
model.

An `EffectiveEnvironment` contains exactly one selected name, its normalized
values, per-field origin, a configuration digest, and later an effective-input
digest. Target/development credentials remain `secret.Reference` values until an
operation requires them. Test discovery has no capability to resolve the
production target.

The input manifest lists project-relative paths and content digests for local
schema, migration, test, and policy inputs, preserving ordered source semantics.
Hashes exclude absolute checkout paths, temporary leases, timestamps, and secret
values. Resolved target identity is separately verified for authorization; a
matching configuration digest does not prove that a secret reference still
resolves to the same authorized target.

## Factory definition and disposable lease

A `DevelopmentFactory` names its mode, supported/requested major version,
positive target allowlist, protected identities, image digest or admin
reference, isolation mode, readiness deadline, and cleanup budget. Configuration
describes intent; only successful ownership verification produces a usable lease
capability.

| Lease field                    | Meaning and validation                                                     |
| ------------------------------ | -------------------------------------------------------------------------- |
| `version`, `run_id`, `case_id` | Versioned record and random unique ownership identities                    |
| `factory`, `postgres_major`    | Selected factory and observed server version; mismatch refuses execution   |
| `resources`                    | Incremental container/database/role identifiers and verified owner markers |
| `state`                        | Allocating, ready, running, cleaning, cleaned, or cleanup_failed           |
| `credential_ref`               | Owner-only transient runtime-file reference; no credential values          |
| `created_at`, `deadline`       | Observed lifecycle timestamps; not approval identity                       |
| `cleanup_errors`               | Sanitized category, resource identity, and next action                     |

Write the lease before provisioning and after each acquired resource. A returned
`DisposableTarget` includes a private verified ownership capability and
transient restricted credentials. It is not interchangeable with the production
target type. Case execution never receives the provisioning administrator
credential.

Lease transitions are allocating → ready → running → cleaning → cleaned.
Failures from any allocated state attempt cleaning; cleanup failure persists
`cleanup_failed`. An explicit retry verifies actual ownership again before
deletion. Crash recovery uses the last durable lease and live ownership markers;
unknown ownership is a refusal, never a best-effort prefix deletion. Separate
processes with the same case name must have different ownership identities.

## Located test suite

A `TestSuite` has language version, selected project/input digest, files,
ordered cases, and execution/report settings. Discovery sorts normalized
relative file paths; cases retain declaration order. Duplicate fully qualified
case names fail validation.

A `TestCase` has kind (schema or migration), source range, skip reason, timeout,
ordered operations, and cleanup operations. Operation kinds are setup, fixture,
seed, version transition, exec, typed assertion, and expected error. Each has
its own source range. A version transition references the immutable migration
snapshot and requires monotonic, existing versions. Seed operations occur
between the stated initial version and upgrade, not after the upgrade by
accident.

Typed assertions compare exactly one returned scalar against
integer/string/boolean/ null values with strict type checking. Expected-error
operations match exactly one SQLSTATE from the requested operation, never
arbitrary raw error text. Preserve existing integer-count library callers
through an adapter.

## Case and suite results

Cases transition discovered → skipped or pending → provisioning → running →
cleaning → passed/failed. Cancellation prevents new starts and still cleans
started cases. A primary assertion success cannot override failed cleanup.

Results retain separate primary failure, cleanup failures, report errors, source
range, stage, semantic outcome, and duration. JSON and JUnit derive from the
same immutable ordered result model. Suite output binds input digests, binary
identity, observed database major, selected/executed/skipped counts, and
sanitized outcomes. Stable identity fields are comparable across CI/local runs;
duration, temporary resources, and run IDs are explicitly excluded from
determinism claims.

Report publication is prepared → atomically published, or failed. A failed
required report changes the command outcome even when every case passed. Failure
envelopes carry the available result summary instead of discarding successful or
failed cases.
