# Zero-downtime rollback and repair

`pkg/zdm/rollback` withdraws the new version schema and removes compatibility
machinery without undoing a completed or partial table backfill. Shadow data is
retained as valid physical data; rollback succeeds only after proving that the
previous version remains writable.

Before mutation the workflow checks that the migration is active and not
completed, the previous version is writable, and all reported blockers are
clear. Lossy transformations without a reverse expression require a specific
operator acknowledgement. The authorization always includes an operator,
timestamp, and reason. Target-scoped serialization and durable phase state make
an identical retry safe.

Repair uses immutable digest-bound proposals containing the exact observed
state, intended action, and expected state. It refuses if the observation has
changed, records `repair_requested` before mutation, verifies the postcondition,
and appends `repair_completed`. Audit rows are append-only evidence; repair never
updates or deletes prior lifecycle history and never infers a mutation silently.
