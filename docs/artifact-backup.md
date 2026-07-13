# Customer-owned artifact backup and fallback

`pkg/backup` keeps a customer-selected object store in the recovery path for
immutable artifacts. `ObjectStore` is intentionally small so an S3-compatible,
GCS, Azure, or on-premise adapter can be supplied without moving ownership of
the data into AutoSQL. `Replicator` copies exact bytes, verifies their
content-addressed digest, and records a manifest entry with creation time,
replication time, byte count, and therefore observable lag.

`Fallback.Read` first uses the primary source. On an outage it reads the
customer backup and verifies both the expected digest and a caller-supplied
signature policy. Missing, stale, digest-mismatched, and signature-invalid
backups return distinct errors; fallback never weakens authorization or
integrity checks. `RecoveryDrill` exercises the same path and reports RPO/RTO
measurements, failing when configured objectives are not met.

The in-memory store and tests are a deterministic reference adapter. Production
adapters should make `Put` durable before acknowledging replication and expose
the manifest through metrics or a control-plane endpoint.
