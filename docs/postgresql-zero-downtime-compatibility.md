# PostgreSQL zero-downtime compatibility

AutoSQL's expand/contract lifecycle is continuously tested on PostgreSQL 14,
15, 16, 17, and 18 by `.github/workflows/zdm-postgres-matrix.yml`. Run the same
matrix locally with `scripts/test-zdm-postgres-matrix.sh`; each version writes a
test and benchmark report under `artifacts/zdm-matrix/`.

The matrix covers metadata initialization and upgrades, planning, virtual
schemas, old/new DML, shadow triggers, bounded backfill, start recovery,
contract completion, rollback, repair, and CLI behavior. The workload suite
adds:

- concurrent ORM-style parameterized reads and writes through both version
  schemas, followed by physical equivalence checks;
- a repeatable-read long transaction while clients continue writing;
- cancellation and restart after every start lifecycle phase;
- backfill throughput of at least 50 rows/second and a maximum two-second batch
  duration in the disposable CI environment;
- trigger-write and version-view-read benchmarks, with an asserted maximum ten
  seconds for 500 serial triggered writes;
- a managed-service profile using a login with `CREATE` on the database and
  ownership of its application schema, but without superuser, `CREATEDB`, or
  `CREATEROLE`.

## Planning during concurrent schema changes

Planning holds shared advisory and `ACCESS SHARE` relation locks in a read-only
repeatable-read transaction. Before releasing those locks, it rejects an
`ACCESS EXCLUSIVE` request queued against an inspected relation in the same
database. It also compares a separate, fresh catalog snapshot before release
and retains the existing comparison after release. A queued schema change
therefore cannot be accepted merely because it has not committed yet.

The lock check uses relation identity, database identity, lock mode, and process
identity from [`pg_locks`](https://www.postgresql.org/docs/14/view-pg-locks.html).
It does not read another session's SQL text or require monitoring privileges.
PostgreSQL's [lock compatibility rules](https://www.postgresql.org/docs/14/explicit-locking.html)
allow ordinary data writes alongside these planning locks; unrelated schema
activity is outside the inspected relation set.

Queued conflicts return the existing `ErrRefused` category with guidance to
retry after schema changes finish. A changed fingerprint asks for a refreshed
trusted baseline. An unavailable or timed-out inspection refuses the plan.
Connection rollback and closure are bounded even after caller cancellation.
These checks cover interference detected through the final validation boundary;
they cannot prevent later schema changes. Existing apply-time preflight checks
remain required.

On each PostgreSQL major 14–18, the CI matrix runs 20 coordinated conflict trials
and stable controls. Reproduce that qualification against a disposable database:

```sh
AUTOSQL_LIVE_COUNT=20 scripts/test-live-serial.sh ./pkg/zdm/expandplan \
  -run '^TestLive(ConcurrentDDLWaitsAndPlanRefusesFreshSnapshot|PlanningIsReadOnlyAndFencedToBaseline)$' \
  -timeout 3m
scripts/test-live-serial.sh ./pkg/zdm/expandplan -timeout 3m
```

Set `AUTOSQL_TEST_POSTGRES_URL` as described in the live-test setup. Fixtures
create isolated schemas, tables, and a restricted test role, then remove them.
They wait for the exact relation lock to queue and register worker cleanup
before assertions. Additional cases cover committed catalog changes, unrelated
activity, minimum-role permissions, cancellation, lock/hold deadlines, and
session cleanup. No test changes production data or weakens the stale-plan
refusal assertion.

## Version notes

PostgreSQL 14 qualifies columns when deparsing simple views differently from
later versions. AutoSQL compares a narrowly normalized simple-view AST while
preserving output aliases and physical-column bindings. PostgreSQL 17 permits a
null catalog representation for the default statistics target; AutoSQL treats
that as the documented default. PostgreSQL 18 also represents `NOT NULL` in
`pg_constraint`; nullability remains checked through `pg_attribute`, while
constraint-manifest comparison excludes that redundant representation.

RDS and Aurora PostgreSQL do not require a superuser for this workflow. Grant
the operator `CONNECT` and `CREATE` on the target database, and make it the owner
of managed application relations (or grant the equivalent `ALTER`, DML, and
schema privileges). AutoSQL does not create roles, databases, extensions, or
server-wide objects. Environments that prohibit database-level `CREATE` must
pre-create the marked control/version schemas under the operator's ownership or
delegate initialization to a more privileged deployment identity.
