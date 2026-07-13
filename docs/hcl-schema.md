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

`FormatHCL` emits stable resource-form HCL ordered by canonical resource ID.
Parsing, formatting, and validation therefore have a deterministic round-trip
that can be used by CI and artifact digest generation.
