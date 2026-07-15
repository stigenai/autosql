# PostgreSQL default expressions

AutoSQL can adopt an inspected PostgreSQL schema, provision the same schema in
an empty database, and make unrelated incremental changes without treating
database defaults as arbitrary SQL. Defaults are parsed into a PostgreSQL AST
and accepted only when their structure, target column type, and declared schema
dependencies match this document.

The complete executable HCL catalog is
[`examples/hcl-postgres/defaults.hcl`](../examples/hcl-postgres/defaults.hcl).
Its test loads and normalizes the file, builds a fresh PostgreSQL plan, and
checks the emitted defaults and dependency order.

## Supported scalar defaults

| Column family | Accepted examples | Boundary |
| --- | --- | --- |
| Integer | `0`, `-42`, `'12'::integer` | Must fit `smallint`, `integer`, or `bigint`; noncanonical forms such as `-0` are rejected. |
| Decimal | `0.00`, `'12.30'::numeric(4,2)` | Must fit the target precision and scale and be finite. |
| Boolean | `true`, `false`, `'true'::boolean` | Only boolean literals are accepted. |
| Text | `'pending'`, `'pending'::text` | `character(n)` and `character varying(n)` values must fit their declared length. |
| Bit string | `'1010'::bit(4)` | Values contain only `0` and `1` and must fit the declared fixed or varying length. |
| UUID | `'550e8400-e29b-41d4-a716-446655440000'::uuid` | Must be a complete UUID literal. |
| JSON/JSONB | `'{}'::jsonb`, `'[]'::jsonb` | The string must contain valid JSON and the cast must match the column family. |
| Date/time | `'2026-07-15'::date`, `'12:30:00'::time`, timestamp literals | The value must parse exactly as its target temporal type. |
| Interval | `'1 day 00:05:00'::interval` | Supports a signed day/time form with valid minute and second fields and up to six fractional digits. |

Literal casts may use a compatible narrower core type, such as an integer cast
for a bigint column or a shorter character cast for a wider character column.
AutoSQL checks both the cast type and the destination type before rendering.

## Supported generated and temporal defaults

The generated-function allowlist is intentionally small:

- `CURRENT_TIMESTAMP` and `CURRENT_TIMESTAMP(0..6)` for timestamp columns;
- `CURRENT_DATE` for date columns;
- `CURRENT_TIME(0..6)` for time-with-time-zone columns;
- `LOCALTIME(0..6)` for time-without-time-zone columns;
- `LOCALTIMESTAMP(0..6)` for timestamp-without-time-zone columns;
- `gen_random_uuid()` for UUID columns; and
- `timezone('utc'::text, now())` for a UTC timestamp without time zone.

Inspection aliases are normalized before comparison and rendering. In
particular, `now()` and `transaction_timestamp()` become
`CURRENT_TIMESTAMP`, `gen_random_uuid()` becomes
`pg_catalog.gen_random_uuid()`, and the UTC timezone form becomes
`pg_catalog.timezone('utc'::text, CURRENT_TIMESTAMP)`. This makes an inspected
schema and its formatted HCL converge to a no-op.

## Arrays

One-dimensional arrays of supported core scalar types are accepted in either
of these forms:

```sql
'{}'::text[]
'{1,2}'::integer[]
ARRAY['a'::text, 'b'::text]
ARRAY[true, false]
ARRAY[]::text[]
```

The array cast must match the column type, and every element is validated with
the same rules as a scalar default. Nested arrays, explicit dimensions, `NULL`
elements, malformed quoting, untyped empty `ARRAY[]`, and function calls inside
array elements are rejected.

## Enum and domain defaults

A column using an enum or domain must carry exactly one `uses` dependency for
that type. AutoSQL checks the dependency rather than trusting the text alone.

Enum defaults must be a string literal cast to the declared type, and the label
must occur in the enum's modeled `values` list:

```hcl
spec_json = jsonencode({
  type     = "defaults_demo.job_status"
  default  = "'pending'::defaults_demo.job_status"
  not_null = true
  ordinal  = 9
})
deps_json = jsonencode([
  contains(table_id("defaults_demo", "widgets")),
  uses(resource_id("enum", "defaults_demo", schema_id("defaults_demo"), "job_status")),
])
```

Domain defaults may be a compatible core literal or an exact cast to the
declared domain. Validation uses the modeled `base_type`; PostgreSQL still
enforces the actual domain constraints when the statement runs. Missing,
mismatched, or ambiguous type dependencies fail closed.

## Sequence-backed defaults

`nextval` is supported only for integer-family columns and only in this exact
shape:

```sql
nextval('defaults_demo.widget_id_seq'::regclass)
```

The sequence name must be schema-qualified and must match exactly one
`references` dependency from the column to a modeled sequence. Inspection
derives this edge from `pg_depend`, including sequences created by PostgreSQL
for `serial` columns. The dependency orders sequence creation before the
column. Unqualified, renamed, composed, missing, mismatched, or ambiguous
sequence references are rejected.

## Safety and diagnostics

AutoSQL does not provide an arbitrary-expression escape hatch for defaults.
It rejects unknown functions, operators, column references, subqueries,
multiple statements, comments, dollar quoting, escape-prefixed strings,
variadic calls, malformed casts, incompatible types, and expressions outside
the bounded AST grammar. An unchanged legacy default does not block an
unrelated column change, but a changed default or a default in the changed
type/sequence dependency closure is validated.

Planning errors identify the affected resource, normalized type, and rejected
expression class, for example `function lower is not allowlisted`. They do not
repeat the default's literal value, and the normal CLI redaction layer still
removes recognized secret material.

If a required expression is not listed here, model the database object through
an explicitly supported feature or keep that migration as reviewed SQL. Do not
weaken the classifier or interpolate an expression from untrusted input.
