# Cloud discovery and continuous monitoring

`pkg/monitor` defines a provider-neutral contract for AWS, GCP, Azure, and
static target discovery. Discovery applies environment/label/ID filters,
rejects duplicate identities, skips revoked targets, enforces a maximum target
count and timeout, masks connection references, and binds the sorted result to
a snapshot digest.

`Monitor.RunOnce` performs bounded concurrent read-only inspections. Every
target has resumable health state (`healthy`, `drifted`, `failed`, `stale`, or
`revoked`). Drift findings include severity, evidence/remediation text, and a
change-plan digest. Each proposal is approval-required; monitoring never
mutates a database.

Alerts are delivered through an injected sink/webhook and contain only target
identity, health, proposal ID, and safe summary text. Credentials, SQL,
results, and connection URLs are not persisted. Operators can use checkpoints
and stale detection to distinguish a real healthy observation from a monitor
that stopped checking.
