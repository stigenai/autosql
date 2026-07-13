# Migration start and recovery

`pkg/zdm/start` coordinates the expand lifecycle as a durable phase machine:

1. validate the artifact and every bound sub-specification;
2. record intent before application objects are changed;
3. execute the verified expand plan;
4. install shadow synchronization;
5. run every durable bounded backfill;
6. publish the previous and new virtual schemas.

The `(target, environment)` pair owns one PostgreSQL session advisory lock, so
two starts cannot mutate the same logical schema concurrently. Durable state is
bound to the artifact, versions, and operation digest. A different request is
rejected; an identical request resumes at the last completed phase.

Every phase action must be idempotent. The supplied `Pipeline` adapter uses the
idempotent virtual-schema, shadow-sync, and backfill packages. Its expand action
must execute a previously verified immutable expand plan and must verify planned
postconditions before treating an already-created object as complete.
Use `RunPipeline` so database, target, and environment bindings are checked
across every subcomponent before intent is recorded.

`StatusOf` reports the active phase, previous and new versions, completion
percentage, blockers, and ordered recovery actions. An interruption is safe to
retry with the identical specification. During backfill, operators should first
inspect the individual backfill status, resume or cancel it as appropriate, and
then retry start. Status and errors contain no database row values.

The control schema and operations table carry exact ownership markers. AutoSQL
refuses to use a pre-existing unmarked namespace or table.
