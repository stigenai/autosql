# Schema sources

AutoSQL loads desired database state locally; source parsing never connects to
the target database. Multiple sources are evaluated in their configured order
and merged by canonical resource identity. Identical definitions are
deduplicated. Contradictory definitions fail with both source locations.

## Native format

The native format is the versioned `autosql.schema/v1` JSON document implemented
by the public `pkg/schema` wire contract. Unknown fields are retained during round trips.
Unknown document versions and resource kinds are rejected because applying a
partially understood schema is unsafe.

## PostgreSQL SQL format

The SQL loader accepts semicolon-delimited PostgreSQL DDL, including quoted
identifiers, strings, nested comments, and dollar-quoted function bodies. It
currently models:

- schemas, tables, columns, and inline or table-level constraints;
- indexes, views, materialized views, sequences, domains, and extensions;
- overloaded functions, procedures, triggers, roles, and users.

Unsupported statements fail with a source-located diagnostic. They are never
silently ignored. Resource-specific database features that are not yet
structurally interpreted remain in the resource's normalized `definition` or
`options` field so the source stays reviewable and deterministic.
