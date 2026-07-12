# AutoSQL CLI contract

The stable workflow commands include `schema inspect`, `schema diff`, `plan`,
and `apply` in addition to the existing version/config/load commands.
Success and ordinary human-readable results go to stdout. Diagnostics and human-readable errors go to stderr. With `--json`, stdout contains exactly one JSON envelope and stderr is reserved for diagnostics; both successful and failed envelopes use `autosql.output/v1`.

## Exit codes and error kinds

| Code | Kind | Meaning |
|---:|---|---|
| 0 | — | Success |
| 1 | `internal` | Unexpected internal failure |
| 2 | `usage` | Invalid command or arguments |
| 3 | `config` | Configuration decoding or semantic error |
| 4 | `secret` | Secret provider or resolution error |
| 5 | `validation` | Local preflight or input validation failure |
| 5 | `permission` | Database inspection permission failure |
| 6 | `connection` | Database connection failure |
| 7 | `migration` | Migration planning or execution failure |
| 8 | `conflict` | Concurrent state or revision conflict |
| 124 | `timeout` | Command deadline exceeded |
| 130 | `canceled` | Interrupt or caller cancellation |

Future commands must use these categories rather than inventing command-specific codes. Cancellation is cooperative and commands must finish or abandon an atomic operation before returning.

## Configuration

`autosql.json` has version `1`, an `environment` name, and an `environments` object. Each named environment defines a `target` secret reference, optional `dev_database` secret reference, `schema_sources`, `migration_dir`, include/exclude filters, variables, and an optional Go-style duration such as `30s`.

Secret references are `env://VARIABLE` or `file:///path`. Secret values are resolved only by `config validate --preflight` and later runtime commands; the values are never included in config or JSON output. The normal validation command parses and validates references without resolving secrets or connecting to any database. `--preflight` additionally resolves secrets and verifies local schema and migration paths, but still never opens a production connection.

Precedence is deterministic: configuration file, then `AUTOSQL_*` environment variables, then explicit command flags. Environment overrides are `AUTOSQL_ENV`, `AUTOSQL_TARGET`, `AUTOSQL_DEV_DATABASE`, `AUTOSQL_SCHEMA_SOURCES`, `AUTOSQL_MIGRATION_DIR`, `AUTOSQL_INCLUDE`, `AUTOSQL_EXCLUDE`, and `AUTOSQL_TIMEOUT`.

## Schema commands

`schema load` accepts repeatable `--source sql:path` and `--source json:path`
arguments. It parses and composes desired state without opening a database.
The default output is the canonical schema document; `--json` wraps that
document in the standard output envelope.

`schema inspect` accepts a database URL only through an `env://` or `file://`
secret reference, plus repeatable schema/include/exclude filters. It inspects
PostgreSQL catalog state and emits the same canonical schema document. The
`--advanced` flag additionally inspects roles and grants. Resolved URLs and
credentials are registered with the command redactor and never serialized.
`--format native|sql|json` controls human output without changing the standard
`--json` envelope.

## Diff, plan, and apply

`--from` and `--to` use the same source syntax: `sql:path`, `native:path`,
`live:env://NAME` (or `file://`), and registered `provider:name:reference`.
Schema/include/exclude filters apply to live reads. `--max-changes` is enforced
before apply. Diff, plan, and `apply --dry-run` receive no mutation capability.

Interactive apply prints the exact plan digest and requires it to be typed on a
TTY. Noninteractive apply requires `--auto-approve` or `--artifact path`.
Mutation is delegated only to the injected `ApplyService`; the default binary
returns `refused` until the guarded executor is wired. Stable statuses are
`no_op`, `success`, `refused`, and `partial_failure`.

## JSON envelope

The normative schema is [`json-output.schema.json`](json-output.schema.json). Consumers must branch on `ok`; failed commands include `error.kind`, `error.message`, and the numeric `error.exit_code`. Unknown additive fields must be ignored.
