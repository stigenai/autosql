# PostgreSQL executor and recovery

The executor accepts only an `artifact.VerifiedArtifact` and is exposed to the
guardrail as an authorized mutation callback. The callback is invoked only after
safety analysis, policy evaluation, approval, and durable approval audit.

It acquires a session advisory lock derived from the signed database identity
and environment before checking time or live state. A competing apply receives
a typed busy error. The lock is held through prechecks, all phases, and durable
history writes. A target fingerprint match is a no-op; any other mismatch is a
stale refusal before migration SQL.

Required phases execute DDL and confirmed history records in one transaction,
so both roll back atomically. Prohibited phases durably record intent before
each statement and confirmation afterward. History records bind artifact,
phase, step ID, step hash, attempt, state, timestamps, last confirmed step, and
recovery guidance. An intended-but-unconfirmed step refuses retry: an operator
must reconcile and explicitly skip it, or create a new signed plan.

## Production CLI configuration

The shipped binary never uses nil apply services. Without configuration it uses
an explicit refusing service. To enable apply, set `AUTOSQL_APPLY_CONFIG` to a
strict JSON file containing the database URL secret reference, environment,
database identity, source revision, trusted Ed25519 key metadata, distinct
author/requester identities, PostgreSQL version, managed schemas, durable
approval/lifecycle audit paths, and an artifact directory. Artifact mode verifies
the supplied file. Digest mode resolves `<artifact_directory>/<digest>.json` and
enters the identical signature, binding, guardrail, lock, and executor boundary.
The configuration is also the trusted release manifest: it must provide the
expected plan, checks, guardrail, and approval-identity bindings plus immutable
key status, validity interval, issuer, and purpose. These values are never
derived from the artifact and are revalidated after the execution lock is held.

## Completion evidence contract

`executor.ObserveCompletion` evaluates an observation-only artifact against
existing execution history, managed state, and durable lifecycle audit evidence.
It returns `proven`, `ineligible`, `refused`, or `retryable` with a bounded reason.
Only a proven result carries the exact release, plan, target, fingerprint,
attempt, confirmed step count, and later observation time.

The reader selects all history states and attempts for the artifact, then
requires one complete attempt with exact step/hash/phase/execution/target/plan/
guardrail bindings. Unknown, duplicate, intended, incomplete, and conflicting
records refuse recovery. Empty and topology plans, bootstrap/adoption artifacts,
and versioned migration artifacts are outside this contract. The caller must
also enforce the declarative resource kind and current external authorization.

`FileAudit.ReadExecution` opens an existing file, validates the entire hash
chain under a bounded shared lock, syncs it, and returns only the matching
execution events. It creates no missing file and appends no records. Missing
commit/confirmation/completion evidence, uncertainty, invalid bindings, or
drift prevents proof; a matching schema or history-row count alone is
insufficient. Managed state is checked again after reading the audit.

The `CompletionReader` interface has no execution or history-repair method.
`ObservePostgreSQLCompletion` supplies the concrete reader, using the executor's
target advisory lock and a read-only, read-committed transaction. Existing
selected relations and history receive access-share locks. Managed catalog state
and complete history are read again after the audit, so an intervening drift or
manual history edit refuses proof. No history table is created or repaired.

The caller supplies the resolved URL, logical database identity, environment,
revision, schema scope, and managed resources from trusted configuration and
resource references. Conflicting release bindings refuse before connection.
These identities must not be copied from an untrusted artifact to manufacture
a matching configuration. Resource authorization and controller publication
remain separate; this addition alone does not enable automatic operator recovery.

Observation is capped at 45 seconds, statements at ten seconds, relation-lock
waits at one second, and each connection/transaction cleanup at five seconds.
Cancellation releases the owned transaction and connection. Contention returns
`retryable/target_busy`. These checks describe the validation window; they do
not promise the database remains unchanged after locks are released.

`CheckPostgreSQLExecutionStart` distinguishes an original-state, unstarted
release from one requiring completion observation. It uses the same bounded
read-only transaction and target coordination, checks every history state and
attempt, validates an existing audit chain, and repeats state/history reads.
An absent audit is allowed only for an original-state target without history;
a damaged audit is refused. Only events known to precede execution may be
present for an unstarted result. After a possible attempt is recorded, an
existing audit with at least one valid pre-execution event is required; missing
or empty audit evidence cannot clear that uncertainty. Any transaction-start, intent, confirmation,
completion, or ambiguous event requires observation. A desired-state match is
never treated as a fresh start, even if history and audit disappeared.

This is routing information, not execution authorization. Normal apply must
still authenticate the current release and pass approval, safety, policy,
prechecks, target locking, and source-state validation. The start check creates
no history table or audit file and never repairs evidence.

`FileAudit.AppendCompletionObservation` accepts a proven observation and resource
UID/generation, opens only an existing audit, and revalidates the original
execution chain under a cancellable exclusive lock. Its ten-second lock budget
prevents recovery from waiting indefinitely on a writer. A stable event ID makes
retries idempotent; even a duplicate is synced before success. Missing, emptied,
or damaged audit evidence cannot be replaced by a new completion record.

Compatibility: history and audit formats are unchanged. Readers and writers now
reject an unterminated final audit record, including a complete JSON object
whose terminating newline was lost during an interrupted append.

Run the supported and rejected contract scenarios without a database:

```sh
go test ./pkg/executor -run 'Test(CompletionObservation|CompletionAudit|FileAuditReadExecution|ExecutionStartAudit)'
```

These scenarios use generated signatures, explicit history fixtures, and
temporary durable audit files. They cover exact completion, binding tampering,
missing/ambiguous evidence, drift, chain corruption, and cancellation while an
audit writer holds the lock. They make no application database changes.

For live evidence, set `AUTOSQL_TEST_POSTGRES_URL` to a disposable PostgreSQL
14–18 administration database, then run:

```sh
scripts/test-live-serial.sh ./pkg/executor \
  -run '^Test(CompletionObservationLive|ExecutionStartLive)' -timeout 3m
```

The live fixtures create uniquely named databases, apply real signed changes
through the executor, and observe them after artifact expiry. They compare
schema fingerprints, exact history contents, and audit bytes before/after
observation, trace the read-only transaction, inject drift and history edits
during audit, and check lock/session cleanup after cancellation. Each owned
database is dropped even after failure. The suite needs database-creation
privileges and must never point at production.
