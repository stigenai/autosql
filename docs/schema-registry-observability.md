# Schema registry and observability control plane

The AV7 packages provide the control-plane boundaries around the canonical
schema and signed migration artifacts:

* `pkg/artifact.ManagedRegistry` stores verified immutable bytes by digest,
  validates integrity manifests and required attestations, and keeps mutable
  promotion tags separate from artifact identity. Read, push, and promotion
  authorization are independent capabilities.
* `pkg/inventory` records redacted target identity, expected/current versions,
  synchronization state, and idempotent fleet deployment events. Query filters
  cover project, environment, target, artifact, status, and time; connection
  strings, SQL, credentials, and query results are rejected.
* `pkg/backup` replicates exact artifact bytes to a customer-owned object store,
  exposes replication lag, and verifies digest plus caller-supplied signature
  policy on fallback. `RecoveryDrill` reports and enforces RPO/RTO objectives.
* `pkg/schemadoc` binds searchable HTML, Mermaid ER diagrams, portable JSON,
  and semantic version comparison to an artifact digest and observation time.
* `pkg/drift` performs bounded, read-only inspections, produces semantic
  remediation changes without applying them, classifies ignored changes,
  deduplicates incidents by target and change fingerprint, and resolves them
  after reconciliation or an explicit accepted baseline.
* `pkg/auditlog` writes actor/time/action/subject/result/correlation records in
  a tamper-evident chain, supports search/export/retention, and retries and
  deduplicates notifications.

Adapters can persist these interfaces in a service or database without
changing the integrity, authorization, redaction, and recovery contracts.
