# Contract completion

`pkg/zdm/contract` withdraws the previous application schema and removes
compatibility state only through a separately previewed and approved contract
plan. The plan digest covers every destructive statement, its durable
postcondition query, transaction mode, operator summary, and recovery action.

Immediately before every step, completion rechecks that:

- no sessions are reported against the previous version;
- every backfill is complete;
- compatibility and correctness checks passed;
- the active migration objects have not drifted;
- the approval names an approver and reason, is current, and binds the exact
  contract digest.

Any failed gate produces a blocker list and ordered recovery actions without
running SQL. A target-scoped advisory lock serializes completion. Durable step
checkpoints and approved postcondition queries make retries safe even when a
process stops after PostgreSQL committed a step but before AutoSQL recorded it.
Transactional and non-transactional execution is supplied by the caller and is
declared in the preview.

After every step, the required final verifier compares a fresh inspection with
the desired canonical schema. Completion is not recorded while obsolete version
schemas, triggers, shadow objects, mappings, privilege differences, or other
drift remains.
