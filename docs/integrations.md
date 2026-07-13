# Integration contracts

## ORM reference adapters

The adapters in `pkg/integrations/orm` consume exported SQL or canonical
AutoSQL JSON, so their release versions are independent from the core engine.

| Adapter | Version | Supported baseline | Explicitly unsupported examples |
| --- | --- | --- | --- |
| Go GORM | 0.1.0 | models, relations, indexes | raw SQL, database triggers |
| Python Django | 0.1.0 | models, relations, indexes | `managed=False`, database triggers |
| JavaScript/TypeScript Prisma | 0.1.0 | models, relations, indexes | unsupported native types, triggers |
| Java Hibernate | 0.1.0 | models, relations, indexes | formulas, triggers |

An export listing an unsupported construct fails by default. Set
`unsupported_mode=warn` to preserve the canonical state and attach an explicit
`adapter_warning/*` annotation. Each adapter is a `plugin.SourceProvider` and
is covered by equivalent SQL/native fixture tests.

## Terraform and generic deployment

`pkg/integrations/deploy` exposes a secret-free `Request`, `Store.Plan`, and
`Store.Apply` contract. Only artifact and target IDs are retained; connection
secrets are rejected rather than persisted. Re-planning an existing deployment
with a different artifact digest returns a conflict, making digest changes
explicit. Destroy requires an explicit approval flag. `Generic` defines start,
observe, and cancel webhook events, carrying correlation IDs and artifact
digests without SQL or credentials.

## IDE and local developer workflow

`pkg/integrations/devflow` exposes format, validate, semantic preview, and
generate operations without an IDE plugin. It delegates to `pkg/source` and
`pkg/schema`, preserving source URI/line diagnostics and CI-compatible parsing.
`LocalHelper` returns a non-production local connection reference and rejects
production environments or targets.
