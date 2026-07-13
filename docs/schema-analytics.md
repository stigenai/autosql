# Schema analytics and governance

`pkg/analytics` records bounded, read-only schema statistics as immutable
snapshots. Every snapshot carries the target, artifact digest, schema digest,
and observation timestamp, so a safety decision can be tied to the exact
upgrade and schema that produced it.

Snapshots include table size and row estimates, index usage and validity,
constraint counts, complexity scoring, and a permission summary. Collectors
must use a read-only catalog adapter and return aggregate metadata only; row
contents, SQL text, connection strings, and credentials are rejected. Limits
on tables, indexes, and constraints prevent unbounded collection.

`Evaluate` compares a current snapshot with a prior snapshot only when target
and schema digests match. It emits deterministic, machine-readable findings
for storage, dead rows, unused indexes, complexity, and growth thresholds.
`Store` retains snapshots by target with configurable age and count limits,
keeping historical data queryable after target or schema drift.

The resulting findings are suitable inputs to safety policy gates and review
dashboards without turning AutoSQL into a general observability platform.
