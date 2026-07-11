# Production migration guardrail

`pkg/guardrail` is the production apply boundary. It composes the existing
schema, safety, policy, approval, and precheck packages rather than reimplementing
their controls.

The caller first binds the artifacts:

1. Compute `guardrail.ChangeDigest(changes)`.
2. Set that value on `precheck.Plan.ChangeDigest` and every assertion's
   `ChangeDigest`.
3. Compute `precheck.Digest(plan)` and set every assertion's `PlanDigest`.
4. Compute `guardrail.ApprovalDigest(changeDigest, plan.Digest, environment)`
   and use it as `approval.Request.Plan.Digest`.
5. Supply one safety statement per precheck statement, in the same order, with
   identical SQL and a change ID present in the bound change set. This prevents
   analysis of a harmless statement list followed by execution of another.

The approval digest uses domain-separated, length-prefixed fields, so values
cannot be moved across boundaries or environments. `Guardrail.Apply` recomputes
all digests and rejects any mismatch before analysis, audit, or database work.

On a valid request, apply proceeds in this order:

1. Run independent safety analyzers and enforce the configured unsuppressed
   severity threshold.
2. Evaluate the policy document over the supplied schema and migration
   resources and reject every violation.
3. Derive approval risk from trusted risk configuration and unsuppressed
   diagnostics. Caller-supplied risk is ignored.
4. Ask `approval.Gate` to authorize and durably audit the exact bound plan.
5. Only within the gate's authorized callback, call `precheck.GuardedApply`.
   It begins the transaction, acquires the migration lock, runs live scalar
   checks, and executes statements only after every check passes.

The returned result contains only digests, diagnostics, violations, derived
risk, and bounded precheck counts. Guardrail error strings never include SQL,
query arguments, backend audit messages, or sampled database rows. Typed errors
retain causes for programmatic inspection with `errors.Is` and `errors.As`.
