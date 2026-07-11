# Production migration guardrail

`pkg/guardrail` is the production apply boundary. It composes the existing
schema, safety, policy, approval, and precheck packages rather than reimplementing
their controls.

The caller first binds the artifacts:

1. Compute `guardrail.ChangeDigest(changes)`.
2. Set that value on `precheck.Plan.ChangeDigest` and every assertion's
   `ChangeDigest`.
3. Compute `precheck.Digest(plan)` and set every assertion's `PlanDigest`.
4. Supply a stable, non-empty policy identity and one safety statement per
   precheck statement, in the same order, with
   identical SQL and a change ID present in the bound change set. This prevents
   analysis of a harmless statement list followed by execution of another.
5. Build canonical statement bindings with `guardrail.BuildStatementBindings`.
   Each entry binds SQL and change ID to a canonical hash of the exact referenced
   `schema.Change`.
6. Compute `Guardrail.BundleDigest(input)` and use it as
   `approval.Request.Plan.Digest`.

The bundle digest is domain-separated and binds the exact changes, precheck
plan, environment, author, requester, policy identity/document/resources,
safety threshold and risk mapping, target metadata and thresholds, sorted
analyzer attestations, exact suppressions, policy evaluator limits, the complete
approval environment policy, and canonical statement bindings.
The approval request's plan expiry is represented explicitly as either unset or
an RFC3339Nano UTC instant. Emergency override presence, identity, and reason
are also bound, so neither normal nor emergency authorization can replay after
expiry or override changes. Individual approval proofs remain dynamic evidence;
their verified claims already bind this bundle digest and environment.
`Guardrail.Apply` recomputes it and rejects any mismatch before analysis, audit,
or database work. Analyzer identities must be non-empty, unique, stable,
concrete package/type identities with a versioned configuration attestation.
Closures and `safety.AnalyzerFunc` are development-only and rejected. The policy
must have a stable identity and at least one rule.

Production uses the system clock. Non-nil `Safety.Now`, `Policy.Now`, or
`Approval.Now` hooks are rejected so time behavior cannot be changed after an
approval was issued.

On a valid request, apply proceeds in this order:

1. Run independent safety analyzers and enforce the configured unsuppressed
   severity threshold.
2. Evaluate the policy document over the supplied schema and migration
   resources and reject every violation.
3. Derive approval risk from trusted risk configuration and all diagnostics.
   Suppression can prevent blocking/reporting but never lowers approval risk;
   caller-supplied risk is ignored.
4. Ask `approval.Gate` to authorize and durably audit the exact bound plan.
5. Only within the gate's authorized callback, call `precheck.GuardedApply`.
   It begins the transaction, acquires the migration lock, runs live scalar
   checks, and executes statements only after every check passes.

The returned result contains only digests, diagnostics, violations, derived
risk, and bounded precheck counts. Guardrail error strings never include SQL,
query arguments, backend audit messages, or sampled database rows. Typed stage
errors intentionally discard raw backend causes; `errors.Is` exposes only safe
guardrail stage sentinels and cancellation/deadline classifications.
