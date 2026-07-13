# Target inventory and deployment events

`pkg/inventory` is the control-plane model for a database target. A target is
identified by project, environment, and an external target ID; it never stores
a connection string. Observations update the target's current and expected
artifact versions, sync status, and last observation time. Every observation
must carry a stable report ID, so retries are idempotent and a reused ID with
different content is rejected.

Deployment events are append-only and carry an event ID, fleet run ID,
artifact digest, overall status, timestamp, and safe per-target status details.
The model supports `passed`, `failed`, `no-op`, `dry-run`, `canceled`, and
`partially-successful` outcomes. Event queries can filter by project,
environment, target, artifact, status, and time range.

The package rejects credentials, connection strings, SQL/query text, and raw
result-like fields at the boundary. Callers should provide only redacted,
structured facts (for example duration, error class, or retry count). The
in-memory store is a reference implementation; a durable control-plane adapter
can preserve the same idempotency and append-only contracts.
