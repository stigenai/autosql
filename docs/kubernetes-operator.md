# Kubernetes operator contract

`deploy/operator/crd.yaml` declares `AutoSQLSchema` resources. A resource may
be declarative or versioned and may select exactly one inline, Secret, ConfigMap,
URL, or immutable registry-digest source. Database URLs are always Secret key
references. The controller writes only conditions, generation, retry count, and
artifact digest to status; it never copies secret values.

The reconciliation core in `pkg/operator` persists an idempotency key and
requires a leader lease before applying. Conditions move through Planning,
ApprovalRequired (when configured), Applying, Ready, or Failed. Persisting the
applied key means a restart or leader change cannot duplicate a successful
apply; retries are safe because the same key is reused.
