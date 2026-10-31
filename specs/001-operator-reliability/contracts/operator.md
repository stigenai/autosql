# Operator Reliability Contracts

## Compatibility boundary

Keep the existing AutoSQLSchema API version, spec fields, artifact format,
history schema, and secret-reference resolution. Add the optional status field
`completionObservedAt` to both standalone and Helm CRD schemas. Older objects
that omit it remain valid. Preserve existing `Applied`/`Adopted` behavior and
existing recovery-state values. Old clients must tolerate additional condition
reasons.

Apply CRD changes before upgrading the operator. Downgrade requires stopping use
of the new recovery behavior and checking old-controller compatibility; a
downgrade must not be claimed safe merely because the new field is optional.

## Readiness and failure contract

Recovered success sets Ready=True, reason `CompletionRecovered`, and the exact
observed generation, applied digest, plan digest, target, step count,
fingerprint, and observation time. Clear Failed and transient applying
conditions consistently.

| Condition reason              | Trigger                                                                                    | Operator guidance                                                        |
| ----------------------------- | ------------------------------------------------------------------------------------------ | ------------------------------------------------------------------------ |
| `ArtifactExpired`             | Release elapsed and completion cannot enter supported recovery                             | Create a fresh approved plan or explicitly use documented adoption       |
| `RecoveryEvidenceInvalid`     | Missing, ambiguous, mismatched, or incomplete history                                      | Inspect the named evidence category; use the existing recovery procedure |
| `RecoveryStateDrifted`        | Current managed state differs from the signed result                                       | Investigate drift and generate a fresh approved plan                     |
| `RecoveryAuditUnavailable`    | Required audit evidence absent or not durably accessible                                   | Repair the audit path; do not replay SQL to repair bookkeeping           |
| `StatePersistenceFailed`      | Required local state write fails                                                           | Restore writable state storage; retry observation                        |
| `ReleaseAuthorizationInvalid` | Trust, approval, target binding, or time validation fails beyond elapsed artifact lifetime | Correct authorization or publish a freshly approved release              |
| `DependencyUnavailable`       | Temporary API, database, reference, or audit connectivity failure                          | Restore the indicated dependency; bounded retries continue               |

Condition messages and `recoveryGuidance` name safe categories, not raw
connection errors that may embed secrets. Preserve more specific existing
bootstrap reasons. Status API failure is reported in logs/metrics even when
status itself cannot be updated. A logger failure must not claim a successful
status update.

## Retry and event contract

- Transient failures use exponential retry from one second to a 60-second cap,
  with bounded jitter and cancellation. The two-minute recovery objective
  includes this cap in the qualification fixture.
- Deterministic evidence, expiry, authorization, or drift failures recheck no
  more often than every five minutes without a relevant input change. They do
  not increment execution-attempt counts or invoke application SQL.
- Resource generation, relevant annotations, and watched reference changes
  enqueue immediate re-evaluation. Suspension is checked first.
- Emit a warning on reason/evidence change; suppress identical warning emission
  until the five-minute recheck. Preserve cumulative failure counters without
  interpreting them as the number of SQL executions.

## Structured logs

Initialize the controller logger before manager construction. Emit start/result
records with resource namespace/name, generation, release digest, bounded
outcome, reason code, duration, and recovery result where available. Include
event identity for a completion observation. Digests and resource identities
belong in logs, not metric labels. Never log secret values, URLs containing
credentials, complete SQL, artifact payloads, or user-controlled raw error
messages without sanitization.

## Metrics and health endpoints

Use controller-runtime's registry; retain its standard controller metrics.

| Metric                                              | Type      | Allowed labels                                                        |
| --------------------------------------------------- | --------- | --------------------------------------------------------------------- |
| `autosql_operator_reconciliations_total`            | Counter   | `outcome`: success, failure, suspended; `reason`: bounded reason code |
| `autosql_operator_reconcile_duration_seconds`       | Histogram | `outcome`: success, failure, suspended                                |
| `autosql_operator_completion_observations_total`    | Counter   | `outcome`: recovered, ineligible, refused, retryable                  |
| `autosql_operator_state_persistence_failures_total` | Counter   | `stage`: pre_apply, post_apply, recovery                              |

Unknown reason codes map to `other`. Initialize documented label combinations so
a quiet but scraped operator is distinguishable from an absent target. Counts
are process-local and reset on restart; use rates/increases and target
availability, not raw totals as historical SLO proof. Do not add namespace,
resource, digest, target, error string, or SQL as labels.

Provide `--metrics-bind-address` (default `:8080`, `0` disables) and
`--health-probe-bind-address` (default `:8081`, `0` disables), threaded through
the existing operator command. Expose `/metrics`, `/healthz`, and `/readyz`
internally. Readiness checks manager/cache readiness, not whether every schema
resource is Ready; standby replicas must not restart because they are not
leader. No public Ingress, new ClusterRole, or mandatory ServiceMonitor CRD is
required.

The chart offers an enabled-by-default internal metrics Service and documented
scrape annotations; standalone examples expose equivalent ports/probes.
Production owners choose their collector configuration and network policy.
Two-replica tests must demonstrate which process serves leadership-scoped work
and that duplicate scrapes are not interpreted as duplicate database executions.

## Planning refusal contract

Retain the existing error category for an unsafe planning snapshot. Return an
actionable bounded message distinguishing a changed fingerprint, queued
conflicting relation change, and inspection timeout. Do not include another
session's query text. The contract covers detected interference through the
final validation boundary, not a guarantee against changes after validation;
application still performs its existing preflight checks.
