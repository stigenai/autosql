# Validation Quickstart: AutoSQL Reliability and Developer Workflow Roadmap

This is the implementation acceptance guide. The specification change does not
add or run the proposed regression suites, mutate a database, or qualify a
deployment.

## Prerequisites

Use an isolated implementation checkout based on revalidated upstream main, Go
1.25 or the repository's updated requirement, and existing project tooling.
Supply disposable PostgreSQL databases for majors 14–18; the live suite creates
and drops schemas, roles, and other fixtures. Never use production credentials
or data.

Follow `.github/workflows/autosql.yml` for Kubernetes 1.35 envtest assets and
Helm/kind prerequisites. Keep secrets in documented environment or secret
references and do not print their values.

```bash
cd /path/to/isolated/auto-sql
pwd
git status --short
git rev-parse HEAD
go version
```

## Existing regression and repository gates

Set `AUTOSQL_TEST_POSTGRES_URL` through the test environment to a disposable
target. The existing harness requires it and runs packages serially.

```bash
AUTOSQL_LIVE_COUNT=20 scripts/test-live-serial.sh ./pkg/zdm/expandplan \
  -run '^TestLiveConcurrentDDLWaitsAndPlanRefusesFreshSnapshot$' -timeout 15m
AUTOSQL_LIVE_COUNT=20 scripts/test-live-serial.sh ./pkg/zdm/expandplan \
  -run '^TestLivePlanningIsReadOnlyAndFencedToBaseline$' -timeout 15m
scripts/test-live-serial.sh ./pkg/zdm/expandplan -run '^TestLive' -timeout 15m
```

Run on each major 14–18. Record the initial reproducer outcome, then the fixed
outcome. The fixed run must reject conflicting changes, pass the stable control,
honor deadlines, and leave no fixture sessions or locks. A run without its live
environment that reports skipped tests is not evidence.

For the existing operator apply tests, provide `AUTOSQL_OPERATOR_PG_URL` as a
disposable URL as well. Use the same isolated fixture or a separate disposable
database, with serial package execution.

```bash
go test -race -p 1 ./pkg/artifact ./pkg/executor ./pkg/operator \
  ./internal/operatorcontroller ./pkg/zdm/expandplan
go test ./...
go vet ./...
go build ./...
deploy/helm/autosql-operator/tests/render.sh
```

The repository-wide test command includes feature-catalog validation. Run the
existing envtest and kind install/upgrade workflows as additional required
gates; unit tests are not substitutes for them.

## New completion and diagnostic scenarios

Implementation will add suites prefixed `TestCompletionObservation` and
`TestOperatorReliability` in the existing artifact, executor, operator, and
controller packages as appropriate. These are proposed test names, not existing
tests. First list tests and verify both families are actually present before
running:

```bash
go test -list 'Test(CompletionObservation|OperatorReliability)' \
  ./pkg/artifact ./pkg/executor ./pkg/operator ./internal/operatorcontroller
go test -race -p 1 -count=1 \
  -run '^Test(CompletionObservation|OperatorReliability)' \
  ./pkg/artifact ./pkg/executor ./pkg/operator ./internal/operatorcontroller
```

The catalog must point to real suite names and paths once implemented. Use real
signed artifacts and the production adapter for integration/live evidence; test
doubles alone qualify only the contract level.

| Scenario                                                            | Expected result and evidence                                                                           |
| ------------------------------------------------------------------- | ------------------------------------------------------------------------------------------------------ |
| SQL and required audit completed; local completion save fails       | Visible persistence failure, no optimistic cache; later Ready within two minutes without another Apply |
| Status update fails or conflicts after checkpoint                   | Retry publication only; correct generation/digest, no duplicate execution                              |
| Replacement leader has empty local state but retained durable audit | Exact history/state/audit observation recovers; Ready reason `CompletionRecovered`                     |
| Local state and required audit evidence both lost                   | Refuse recovery with audit guidance; never infer audit from SQL history                                |
| Expired artifact; all other authority valid                         | Observation may recover; expired artifact still cannot authorize SQL                                   |
| Revoked key, expired approval, future creation, wrong target        | Not Ready; no SQL; distinct authorization reason                                                       |
| Missing, duplicate, tampered, incomplete, or ambiguous history      | Not Ready; no history creation/repair; evidence-specific guidance                                      |
| Current managed fingerprint differs                                 | Not Ready with drift reason                                                                            |
| Audit read/append failure                                           | No recovered Ready; repair-audit guidance                                                              |
| New generation/reference/target arrives during observation          | Discard stale result and evaluate current inputs                                                       |
| Suspended, bootstrap, versioned, or empty/topology release          | No unsupported recovery; suspension causes no database access                                          |
| Temporary dependency outage                                         | Bounded retry and recovery without operator restart                                                    |
| Unchanged deterministic failure                                     | At most one periodic recheck/warning per five minutes; relevant input change triggers evaluation       |
| Two replicas and leader handoff                                     | No duplicate SQL; standby remains healthy; metrics interpreted per process                             |
| Secret-bearing error input                                          | No secret, URL credential, SQL, or artifact payload in status, logs, events, or labels                 |

Inject failures only in disposable fixtures. Capture execution-call counts,
history before/after, managed-state fingerprints, audit event identity, status,
and elapsed recovery time. Use a counting application hook and an unchanged
history comparison to prove observation did not execute SQL or create
bookkeeping.

## Monitoring and runbook acceptance

Deploy the candidate into the existing disposable kind qualification
environment. Scrape the internal metrics Service using the documented collector
configuration. Then exercise success, expiry with incomplete evidence,
persistence failure, authorization failure, and temporary dependency failure.
The operator following the runbook must identify each cause and next action
within five minutes.

Example queries after the collector is configured:

```promql
sum by (outcome, reason) (increase(autosql_operator_reconciliations_total[30m]))
sum by (outcome) (increase(autosql_operator_completion_observations_total[30m]))
sum by (stage) (increase(autosql_operator_state_persistence_failures_total[30m]))
```

Use the collector's actual target selector to inspect `up` and collection gaps.
Record a 30-minute window with no gap longer than two configured collection
intervals. An empty query is missing evidence, not a zero-failure result.
Confirm the controller logger initialization warning is absent and both
replicas' health behavior is correct. See
[contracts/operator.md](contracts/operator.md) for labels, ports, and status
semantics.

## Release and production handoff

Attach the source SHA, required CI results, supported-major runs, catalog
evidence, release version/image digest, and compatibility notes to the release
review. Production owners must separately apply and qualify collector/deployment
changes in `stigen-flux`; this guide does not authorize that rollout.

For an authorized rollout, capture readiness, generations, image identity,
resource status, and backend ingestion. Inspect the current cell resource
read-only. If it cannot prove completion under current trust and audit rules,
record the refusal and owner action instead of forcing Ready. No production
fault injection or expired SQL replay is part of qualification.

## R2: Project and factory qualification

The following suites and commands are proposed acceptance surfaces to add during
implementation. They must not be reported as existing or passing merely because
this guide lists them. First run the existing focused tests:

```bash
go test ./pkg/config ./pkg/dbtest ./internal/cli
```

Add suites with prefixes `TestProjectWorkflow`, `TestDisposableFactory`, and
`TestFileTestWorkflow`, then list them and require actual matches before
execution:

```bash
go test -list 'Test(ProjectWorkflow|DisposableFactory|FileTestWorkflow)' \
  ./pkg/config ./pkg/dbtest ./internal/cli
go test -race -p 1 -count=1 \
  -run '^Test(ProjectWorkflow|DisposableFactory|FileTestWorkflow)' \
  ./pkg/config ./pkg/dbtest ./internal/cli
```

Run factory integration suites using explicit disposable admin references or an
explicit Docker-compatible daemon. Qualify both modes on Linux/macOS and
PostgreSQL 14–18, using separate fixtures per matrix job. Never reuse production
URLs to make a live suite runnable.

| Scenario                                        | Required evidence                                                                           |
| ----------------------------------------------- | ------------------------------------------------------------------------------------------- |
| Version-1 JSON and direct flags                 | Existing fixtures keep the same selection, precedence, and output                           |
| Equivalent version-2 JSON/HCL                   | Same normalized values and configuration digest; duplicate/generated names refuse           |
| Local versus CI checkout path                   | Same effective-input digest despite different absolute directories                          |
| Override/source/migration/policy change         | Corresponding effective identity changes and old project-bound approval refuses             |
| Validation and test discovery                   | No database, external program, network source, or production-secret access                  |
| Production name, reference, or endpoint alias   | Refusal before mutation, with sanitized output                                              |
| Container and admin factory                     | Requested actual version, positive authorization, restricted fixture role, unique ownership |
| Ten concurrent same-named cases                 | No shared database/history, no cross-case changes or cleanup                                |
| Partial create, readiness timeout, canceled run | Cleanup or explicit failed lease; no silent orphan                                          |
| Force-killed process                            | Recorded lease allows explicit ownership-verified cleanup                                   |
| Altered lease or unrelated prefixed resource    | Cleanup refuses mismatched ownership and preserves unrelated resources                      |

## R3: Released project/testing example

Implementation must supply `examples/project-testing/` and its documented
fixtures. The commands below are the target user workflow; they are not runnable
in the current planning-only checkout. Provide any required development
references securely, then use the released binary selected by the qualification
environment:

```bash
autosql version --json
autosql config validate \
  --config examples/project-testing/autosql.hcl --env local --json
autosql config validate \
  --config examples/project-testing/autosql.hcl --env production --json
autosql schema test \
  --config examples/project-testing/autosql.hcl --env local \
  --json --junit /tmp/autosql-schema-results.xml
autosql migrate test \
  --config examples/project-testing/autosql.hcl --env local \
  --json --junit /tmp/autosql-migration-results.xml
```

Repeat with `--env ci` on the CI runner. Production appears only in validation,
never as a test destination. Compare effective inputs for equivalent
environments; local and CI examples must deliberately use identical semantic
settings and reference identifiers when equal digests are expected. Different
factory identities/settings may legitimately produce different digests and must
be explicitly recorded.

The versioned example includes a seed-before-upgrade case, typed scalar
assertion, exact SQLSTATE expectation, reasoned skip, filtered run, intentional
assertion failure, cleanup failure, and report-write failure. Verify
file/line/column, ordering, exit categories, and matching JSON/JUnit semantics.
Prove that zero selected cases and all-skipped suites are failures. Run the case
set with parallelism one and ten; only durations/run identities may vary for
otherwise identical inputs/outcomes.

Use the explicit create/cleanup commands in the contract for a separate
interrupted- provisioning demonstration. Record ownership and remaining-resource
evidence. Do not include secret values in screenshots, reports, fixtures, logs,
or lease files. Include an injected secret-bearing backend error to exercise
redaction.

Run the existing real-signature operator/release integration workflow in
addition to fixture tests. A test-only disposable execution success is not proof
that a production signed artifact would pass authorization.

Required release evidence comprises the binary digest/version, example commit,
configuration/effective-input digests, migration/test identities, platform and
observed database major, selected/executed/skipped counts, stable semantic
results, JSON/JUnit artifacts, no-secret scan, and cleanup verification. Missing
or skipped required cases stop qualification. Do not close `autosql-f2x.8` from
this bounded proof: its external-source/export/ERD/full-command-surface
dependencies remain.
