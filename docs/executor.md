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
