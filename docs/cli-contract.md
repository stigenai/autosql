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

`autosql.json` has version `1`, an `environment` name, and an `environments` object. Each named environment defines a `target` secret reference, optional `dev_database` secret reference, `schema_sources`, `migration_dir`, optional lowercase PostgreSQL `revision_schema` (default `autosql_revision`), include/exclude filters, variables, and an optional Go-style duration such as `30s`.

Secret references are `env://VARIABLE` or `file:///path`. Secret values are resolved only by `config validate --preflight` and later runtime commands; the values are never included in config or JSON output. The normal validation command parses and validates references without resolving secrets or connecting to any database. `--preflight` additionally resolves secrets and verifies local schema and migration paths, but still never opens a production connection.

Precedence is deterministic: configuration file, then `AUTOSQL_*` environment variables, then explicit command flags. Environment overrides are `AUTOSQL_ENV`, `AUTOSQL_TARGET`, `AUTOSQL_DEV_DATABASE`, `AUTOSQL_SCHEMA_SOURCES`, `AUTOSQL_MIGRATION_DIR`, `AUTOSQL_REVISION_SCHEMA`, `AUTOSQL_INCLUDE`, `AUTOSQL_EXCLUDE`, and `AUTOSQL_TIMEOUT`.

## Migration revision status

`migrate status --config autosql.json [--env NAME]` or
`migrate status --url env://DATABASE --migration-dir PATH [--revision-schema NAME]`
first verifies the canonical migration manifest, then performs read-only queries
against the versioned PostgreSQL revision store. It never initializes, upgrades,
repairs, or reinterprets incomplete records. Output distinguishes pending,
applied, failed, partial, baseline, checkpoint, unknown, dirty, and drifted
entries. Executor rows in an incomplete state add reconciliation guidance but
never promote a revision to applied. JSON uses the standard output envelope and
contains `manifest_digest`, ordered `entries`, `counts`, `dirty`, and `drift`.

`migrate apply` and `migrate baseline` acquire one target advisory lock on one
pinned PostgreSQL session, then reload the directory snapshot, revision rows,
and exact artifact bytes under that lock. `--count`, `--from`, and inclusive
`--to` select only a contiguous pending prefix; gaps, unknown revisions, drift,
dirty/partial state, missing raw-byte bindings, or any untrusted signed artifact
refuse before pending writes. Dry run performs the same locked trust and
selection checks without mutation. Baseline verifies every artifact and records
the prefix atomically with distinct `baseline_recorded` events and zero SQL.
Per-file transactional execution commits DDL, revision state, statement
evidence, executor history, and events together. `--transaction=all` places all
selected transaction-safe files and evidence in one database transaction.
Nontransactional statements durably record intended and confirmed evidence;
an ambiguous execution or confirmation boundary is reported as uncertain and
cannot be retried until reconciled. Only signed artifact approval is accepted.
Output includes ordered file results, exact file/statement/line/column failure
position, durations, final version, backend session, and recovery guidance.

## Diagnose and repair

`migrate diagnose --url env://DATABASE --migration-dir PATH` acquires the same
canonical target lock as apply and compares the verified manifest and signed
artifacts with revision rows, target-scoped executor history, and the canonical
live schema fingerprint. It reports only the first divergence and a root cause:
dirty/partial evidence, an unknown revision, checksum drift, or manual schema
drift. Suggested commands retain secret references and never contain resolved
credentials.

`migrate repair mark|remove|reconcile` requires a byte-immutable
`autosql.repair-proposal/v1`, a trusted operator Ed25519 signature, bounded
reason, expected before digest/state, exact action and expected after state,
manifest/guardrail/approval bindings, and an unexpired target environment and
database identity. Repairs run under the canonical lock with a database CAS.
Requested and applied audit records must be durable before mutation; an audit
failure leaves revision state unchanged. Remove requires a distinct destructive
approval and appends a reversal tombstone that supersedes the original row—it
never deletes history. Stale or tampered proposals are refused and audited.
Repair authorization uses a separately configured, digest-bound repair policy
and distinct normal/destructive approval evidence. Schema-plan safety analyzers
are intentionally not rerun: mark/reconcile/tombstone actions execute no user
DDL or data SQL. The production authorization callback instead fail-closes on
the signed action, target/environment, before-state CAS, manifest and guardrail
bindings, repair-policy digest, and approval digest; all of that evidence is
copied into requested/applied/refused audit records.

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
Schema/include/exclude filters apply only when both inputs are live reads; local
or provider inputs reject selectors. An omitted `--max-changes` is unlimited,
while an explicit zero permits only a no-op. Diff, plan, and `apply --dry-run`
receive no mutation capability.

Interactive apply prints the exact plan digest and requires it to be typed on a
TTY. Noninteractive apply requires `--approve-digest DIGEST`, which must exactly
match the computed plan. `--dry-run`, `--approve-digest`, and `--artifact` are
mutually exclusive. Artifact execution is available only through the cs5.7
verified-artifact service: trusted signature and binding verification, guardrail
authorization/audit, locked stale-state checks, live prechecks, and the
phase-aware PostgreSQL executor. Missing configuration remains fail-closed.
Mutation is delegated only to the injected `ApplyService`; the default binary
returns `refused` until the guarded executor is wired. Stable statuses are
`planned`, `dry_run`, `no_op`, `success`, `refused`, and `partial_failure`.
`partial_failure` is reported only when the executor returns structured evidence
of partial mutation; a generic pre-mutation error has no fabricated status.
Successful executor messages are sanitized before human or JSON serialization.

## JSON envelope

The normative schema is [`json-output.schema.json`](json-output.schema.json). Consumers must branch on `ok`; failed commands include `error.kind`, `error.message`, and the numeric `error.exit_code`. Unknown additive fields must be ignored.
