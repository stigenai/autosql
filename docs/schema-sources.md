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

Inline primary, unique, foreign-key and check constraints retain their column
metadata, explicit names and PostgreSQL definitions. Primary keys also make the
column non-null. Deferrability, initially-deferred timing and referential actions
are preserved. Explicit referenced columns and an unambiguous referenced key in
the same SQL source are bound before rendering. Unresolved or ambiguous reference
graphs remain subject to the renderer's existing refusal checks.

Compatibility: corrected inline definitions and column metadata can change the
canonical graph, plan and input fingerprints compared with the earlier incomplete
parser output. Regenerate affected plans and approvals; old digests are not
reinterpreted. Existing table-level declarations keep their source representation.
