# Declarative PostgreSQL HCL

AutoSQL HCL is an author-first PostgreSQL schema language that lowers into the
same versioned canonical graph used by inspection, planning, policy, signing,
and execution. Authors use typed blocks and symbolic references; automation can
use the lossless canonical `resource` form. Both forms can coexist in one file.

## Native resources

The author form covers every PostgreSQL resource kind managed by AutoSQL:

| Area | Blocks |
| --- | --- |
| Namespaces and types | `schema`, `extension`, `enum`, `domain`, `composite`, `sequence` |
| Relations | `table`, nested `column`, `primary_key`, `unique`, `check`, `foreign_key`, `index`, partition declarations, `view`, `materialized_view` |
| Server behavior | `function`, `procedure`, `trigger` |
| Security | `policy`, `role`, `grant`, `membership`, `default_privilege`, ownership attributes |
| Bootstrap | `database` plus the separately authorized bootstrap contract |

Containment and references are inferred from nesting and typed attributes:

```hcl
schema "app" {}

enum "account_state" {
  schema = schema.app
  values = ["pending", "active", "disabled"]
}

table "accounts" {
  schema  = schema.app
  comment = "Tenant accounts"

  column "id" {
    type = "bigint"
    identity { mode = "always" }
  }
  column "state" {
    type    = enum.account_state
    null    = false
    default = enum_value(enum.account_state, "pending")
  }

  primary_key { columns = [column.id] }
  index "accounts_active_idx" {
    columns = [column.state]
    where   = sql("state = 'active'::app.account_state")
  }
}
```

Symbolic addresses are stable and schema-aware. Examples include
`schema.app`, `table["app"]["accounts"]`,
`table["app"]["accounts"]["column"]["id"]`, and
`function["app"]["normalize_email(text)"]`. Short forms such as
`table.accounts` and `column.id` are available only when the name is
unambiguous in the current scope. Ambiguous or missing references fail closed.

Explicit dependency constructors—`contains`, `references`, `uses`, and
`owns`—accept symbolic references. They are needed only when the relationship
cannot be inferred from a typed attribute or parsed PostgreSQL expression.

## Bounded SQL expressions

HCL never turns an arbitrary string into executable SQL implicitly. Use typed
constructors to state intent:

- `literal(value)` for a quoted PostgreSQL literal;
- `sql(expression)` for the bounded, parser-validated expression grammar;
- `cast(value, type)` for an explicit cast;
- `enum_value(enum.account_state, "pending")` for an enum literal and exact
  type dependency; and
- `sql_array(values, type)` for a one-dimensional typed array.

Routine and view bodies use readable heredocs. Routine execution remains bound
to independently reviewed source digests; declaring a body in HCL does not
authorize it. Extension packages likewise require an external allowlist,
version pin, schema policy, and server readiness.

## Variables, locals, and deterministic expansion

Variables have types, defaults, and aggregated validation diagnostics. Locals
are pure and cycle-checked. `for_each` accepts only maps/objects with stable
string keys, expands in sorted-key order, and records the key in source
provenance:

```hcl
variable "environment" {
  type    = string
  default = "development"
  validation {
    condition     = var.environment == "development" || var.environment == "production"
    error_message = "unsupported environment"
  }
}

table "regional_queue" {
  for_each = { east = "us-east-1", west = "us-west-2" }
  name     = "queue_${each.key}"
  schema   = "app"

  column "region" {
    type    = "text"
    default = literal(each.value)
  }
}
```

Evaluation exposes no environment, filesystem, network, process-execution, or
secret functions. Credentials remain runtime `env://` or `file://` references
outside the schema document.

## Imports and typed modules

`import` composes trusted files in the caller scope. `module` creates an
isolated scope with an explicit input contract and declared typed outputs:

```hcl
module "accounts" {
  source = "accounts"
  inputs = { schema_name = "app" }
}

table "audit_log" {
  schema = module.accounts.schema_name
}
```

Module inputs must match variables declared by the module; root values never
leak into a module implicitly. A module source may be a file or directory.
Directory files are loaded in lexical order, while graph normalization makes
the result independent of composition order. Relative paths, source
locations, cycle detection, and the depth limit are preserved. Duplicate
resource identities report both declaration locations.

See the executable
[`author-modules`](../examples/hcl-postgres/author-modules/main.hcl) example.

## Reviewed rename intent

AutoSQL never guesses that a drop/create pair is a rename. Declare intent on a
resource or with a `moved` block:

```hcl
table "accounts" {
  schema       = schema.app
  renamed_from = table["app"]["customer_accounts"]
}

moved {
  from = table["app"]["customer_accounts"]["column"]["customer_id"]
  to   = table["app"]["accounts"]["column"]["id"]
}
```

Hints are normalized, conflict-checked, and stored as planning intent rather
than desired database state. Signed plans include a digest of the exact hint
set. Stale, duplicate, cross-kind, cross-parent, or conflicting moves fail
closed. Formatting preserves the intent. The complete example is
[`rename-intent.hcl`](../examples/hcl-postgres/rename-intent.hcl).

## Author and canonical formatting

`schema inspect` exposes two explicit HCL styles:

```sh
autosql schema inspect --url env://AUTOSQL_DATABASE_URL \
  --schema app --format hcl --hcl-style author

autosql schema inspect --url env://AUTOSQL_DATABASE_URL \
  --schema app --format hcl --hcl-style canonical
```

Author style emits native nesting, symbolic references, comments, and
heredocs. If a resource contains extension data that has no lossless native
representation, only that resource falls back to canonical form. Canonical
style emits deterministic `resource "kind" "name"` blocks with exact JSON
fields. Every `Document`, `Graph`, `Resource`, `Name`, dependency, annotation,
source-location, and extension field survives the mixed-form round trip.

The production-scale integration gate formats and reloads a 1,007-resource
catalog, performs a signed fresh-database bootstrap, reinspects it, and proves
zero-change convergence on PostgreSQL 14, 15, 16, 17, and 18.

For complete runnable workflows and the resource-by-resource catalog, see the
[HCL-managed PostgreSQL example](../examples/hcl-postgres/README.md). The
[default-expression reference](postgresql-default-expressions.md) documents
the bounded grammar and its rejection boundaries.
