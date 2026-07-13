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
