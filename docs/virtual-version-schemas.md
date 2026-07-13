# Virtual version schemas

`autosql migrate virtual-schema-apply` installs two application-facing
PostgreSQL schemas in one transaction. Each logical table is a simple view over
an explicitly schema-qualified physical table. PostgreSQL therefore provides
native updatability for supported direct column mappings, including
`INSERT`, `UPDATE`, `DELETE`, defaults, stored generated columns, and
`RETURNING`.

The specification is deterministic and digest-bound. Schema and view comments
carry ownership markers; installation refuses unmarked collisions. `CREATE` is
revoked from `PUBLIC` on version schemas. Status reports both safe connection
paths:

```
<previous_schema>, pg_catalog
<current_schema>, pg_catalog
```

Views never depend on the caller's `search_path`; physical relations are fully
qualified at creation. PostgreSQL does not copy base-table grants, ownership,
security labels, or comments to a view. Status therefore always emits an
explicit `grants_ownership_review` diagnostic. Operators must grant schema
`USAGE` and the required view DML privileges to application roles after review.
This is intentional: silently copying ACLs can expand access when old and new
logical column sets differ.

Expression projections, joins, aggregates, and arbitrary `INSTEAD OF`
triggers are not accepted by this task. Transformation and bidirectional shadow
column synchronization belong to `autosql-cmz.5`.
