# Fleet deployment and upgrade orchestration

`pkg/fleet` provides the control-plane contracts for staged database upgrades:

* `Target`, discovery providers, filters, and deterministic `Snapshot` values
  normalize target identity while masking connection references.
* `BuildRollout` binds a verified artifact and immutable snapshot to dependency-
  ordered groups, canaries, bounded batches, concurrency, delays, and fail
  policy. `DryRun` is stable JSON-ready output.
* `Executor` persists per-target attempts and events, enforces one active
  target/artifact execution, retries only errors that implement `Transient`,
  and resumes successful work without reapplying it.
* `GatePolicy` binds checks and approvals to rollout, stage, artifact, and
  snapshot. Emergency bypasses require policy permission and a reason.
* `RunHook` bounds pre-stage/per-target/post-stage hooks, retries and cleanup,
  and redacts scoped secrets from output.
* `Status` and `Recovery` expose target state and auditable retry, skip, and
  rollback operations without silently rewriting rollout history.

Adapters can connect `ApplyFunc`, discovery, hooks, and audit sinks to the
deployment environment while preserving these deterministic boundaries.
