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
