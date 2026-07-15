# Declarative HCL authoring

`pkg/source` accepts an AutoSQL HCL dialect in addition to SQL and canonical
JSON. Ergonomic blocks such as `schema "public" {}`, `table "users" {}`,
`role`, `permission`, `policy`, `data`, and nested object blocks lower to the
same versioned canonical graph. The `resource "kind" "name"` form is a
lossless escape hatch used by the deterministic formatter for kinds with
dialect-specific fields.

Attributes support literals and approved variables. Imports/modules are loaded
through `HCLLoader`, which resolves relative paths in sorted order, rejects
cycles, enforces a depth limit, and preserves each block's URI/line/column.
Unknown block kinds fail closed; vendor extensions must be represented through
capability-gated canonical resource fields rather than silently discarded.

Authoring helpers keep lossless `resource` blocks readable:

- `jsonencode(value)` creates canonical `spec_json` and `deps_json` strings;
- `schema_id(name)`, `table_id(schema, table)`, and
  `column_id(schema, table, column)` calculate stable canonical identities;
- `resource_id(kind, schema, parent, name)` handles other canonical kinds; and
- `contains(id)`, `references(id)`, `uses(id)`, and `owns(id)` create typed
  dependency objects for `jsonencode([...])`.

All helpers are pure. HCL evaluation cannot read environment variables, files,
network resources, or secrets; database credentials remain secret references
resolved outside the schema document.

`FormatHCL` emits stable resource-form HCL ordered by canonical resource ID.
Parsing, formatting, and validation therefore have a deterministic round-trip
that can be used by CI and artifact digest generation.

For a complete HCL-to-plan workflow, live PostgreSQL upgrade, and
PostgreSQL-to-HCL round-trip, see the
[HCL-managed PostgreSQL example](../examples/hcl-postgres/README.md). Its
advanced catalog is automatically checked against every resource kind exposed
by the PostgreSQL driver, including constraints, types, routines, triggers,
row-level security, roles, grants, memberships, and default privileges.
