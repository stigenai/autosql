# Bounded online backfills

`autosql migrate backfill run|status|pause|resume|cancel` fills one shadow
column from its source column in deterministic unique-key order.

Each transaction selects at most `--batch-size` currently eligible rows with
`FOR UPDATE SKIP LOCKED`, then updates only rows whose destination remains
NULL. It does not use a high-water cursor, so process restarts, retries, and
newly inserted low keys cannot be skipped. Application writes win safely: the
shadow synchronization trigger fills their destination, making those rows
ineligible for later backfill batches.

Progress is durable in a target/environment/artifact/spec-scoped control row.
The state records processed and remaining rows, retry count, aggregate
throughput, progress lag, and a sanitized error code. It never stores or emits
row values.

Load controls include batch size, inter-batch delay, maximum rows per second,
lock timeout, statement timeout, bounded transient retries, and linear retry
backoff. Pause, resume, and cancellation are durable. One session advisory lock
permits only one worker for a job scope.

Backfill transactions set `autosql.zdm.backfill=on`; AutoSQL shadow triggers
then leave the source representation untouched while the destination is filled.
Transformations use the same conservative grammar as synchronization triggers.
