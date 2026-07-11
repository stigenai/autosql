# Database safety and test harness

`pkg/precheck` provides `GuardedApply` for data-dependent migration checks. A
plan and each assertion carry the exact plan and change digests. The digest
includes each assertion's name, canonical query, arguments, maximum count,
timeout, and change identity, so approved behavior cannot be edited afterward.
Assertion SQL is parsed from a deliberately small grammar: `SELECT count(*)`
over identifiers with optional placeholder comparisons, NULL checks, and `AND`.
Functions, arbitrary selected values, comments, multiple statements, and other
SQL are rejected before a transaction begins. Guarded apply
opens one transaction, acquires its migration lock, executes scalar count
queries, and only then runs mutation statements. A mismatch, failed check,
timeout, or cancellation rolls the transaction back. Results expose only the
assertion name and counts; assertion queries cannot return sampled rows.

`pkg/dbtest` runs every case in an isolated database supplied by a `Factory`.
Cases may set up a blank schema, load fixtures and seeds, apply any number of
named migration versions and plans, and execute scalar SQL assertions. `${name}`
variables are expanded deterministically. Commands and assertions retain file
and line metadata for useful CI failures. Teardown commands run in reverse
order, followed by database close, even when work fails, times out, or is
cancelled. Every teardown and close action gets its own fresh bounded context;
all cleanup errors are retained alongside the primary failure. PostgreSQL setup
also uses fresh contexts to remove a schema and close its connection after
partial setup failure.

For PostgreSQL CI, implement the small `Factory` and `Database` interfaces with
a privileged administrative connection that creates a uniquely named database
or schema per case. Gate the real-database test with an explicit environment
variable (for example `AUTOSQL_POSTGRES_TEST_DSN`) so ordinary unit tests remain
fast and hermetic. The harness itself has no dependency on a particular driver.
