# Bounded reference data as code

`pkg/reference` manages application-owned rows only when a declaration names a
schema, table, stable primary-key columns, and an explicit managed-column set.
Declarations have row and serialized-byte limits, reject duplicate or null
keys, and can exclude non-deterministic columns from diffs without excluding
those columns from the managed boundary.

Each table chooses one reconciliation mode:

- `insert` creates missing keys and never updates or deletes rows.
- `upsert` creates missing keys and updates changed managed values; it never
  deletes rows.
- `sync` also plans exact deletes, reports the delete count, and requires an
  explicit destructive-data approval before applying.

Plans are deterministic by primary key. The `Store` adapter applies actions in
a transaction and rolls back on the first error. Production adapters should
refuse declarations for unbounded or non-owned tables before opening a write
transaction.
