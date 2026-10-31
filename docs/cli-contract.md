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

Version `2` additionally supports explicitly selected JSON/HCL project files,
finite generated environments, local HCL sources, and test/factory/policy/registry
settings. `config validate --config PATH --env NAME --json` returns normalized
effective values and field provenance for version 2; version 1 retains its
existing result shape. See [project configuration](project-configuration.md)
for the supported fields, defaults, located errors, and compatibility rules.

Secret references are `env://VARIABLE` or `file:///path`. Secret values are resolved only by `config validate --preflight` and later runtime commands; the values are never included in config or JSON output. The normal validation command parses and validates references without resolving secrets or connecting to any database. `--preflight` additionally resolves secrets and verifies local schema and migration paths, but still never opens a production connection.

Precedence is deterministic: configuration file, then `AUTOSQL_*` environment variables, then explicit command flags. Environment overrides are `AUTOSQL_ENV`, `AUTOSQL_TARGET`, `AUTOSQL_DEV_DATABASE`, `AUTOSQL_SCHEMA_SOURCES`, `AUTOSQL_MIGRATION_DIR`, `AUTOSQL_REVISION_SCHEMA`, `AUTOSQL_INCLUDE`, `AUTOSQL_EXCLUDE`, and `AUTOSQL_TIMEOUT`.

## Disposable schema and migration tests

`schema test` and `migrate test` require `--config PATH` and a version-2
development environment with a named factory. They accept `--env`, `--filter`,
`--parallel 1..32`, `--json[=false]` and `--junit PATH`; relative JUnit paths are
project-relative. `--junit=` disables a configured report. Each command's
`--help` succeeds without loading configuration or initializing Apply services.

Results use the existing envelope on both success and failure. Failure
`data` retains the ordered suite, identities and all case/cleanup/report outcomes.
Case-specific assertion/cleanup kinds remain in the data; envelope kinds stay
`validation` (5) and `migration` (7). Selection errors use 5; report-only errors
use `internal` (1). A primary test failure keeps its code if cleanup/reporting
also fails. Cancellation/deadline use 130/124 and still attempt bounded cleanup.
See [database testing](database-testing.md#project-test-commands) for discovery,
input verification, source locations, redaction and JUnit semantics.

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

Project generation uses `migrate generate --config PATH --env NAME
--generation-config TRUSTED --version VERSION --label LABEL`. It returns the
generation result, captured input manifest, trusted-verification settings, and
release evidence in the standard JSON envelope. Current project inputs are
recomputed before Apply and reauthorization; explicit source/directory overrides
are bound. See [project release generation](project-configuration.md#project-release-generation-and-verification).
Legacy direct generation keeps its output shape. The command owns its generation
configuration and ignores unrelated Apply initialization.

## Schema commands

`schema load` accepts repeatable `--source sql:path`, `--source json:path`, and
`--source hcl:path` arguments. It parses and composes desired state without opening a database.
The default output is the canonical schema document; `--json` wraps that
document in the standard output envelope.

`schema load --config PATH --env NAME` reads the selected project's sources;
explicit `--source` flags replace the configured list. Project JSON output uses
`data.schema`, `data.environment`, and version-2 configuration identities and
captured source records. It resolves no database credentials. See
[project commands](project-configuration.md#project-schema-loading-and-planning).

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

`plan --config PATH --env NAME` uses the project's desired sources and inspects
only its selected target when `--from` is omitted. A local `--from` permits
planning without database access; `--to` replaces the desired-source list.
Project include/exclude filters select both graphs, including local sources,
before fingerprints are computed. `--schema` retains the direct-live-only rule.
These project outputs are planning evidence, not release authorization.

`plan` takes an optional flag: `--postgres-version version`. `version` must
be `<major>[.<minor>]`, with `major` from 14 to 18. This is the same range
as `database bootstrap prepare --postgres-version`. The flag sets the
render option that a version-gated feature checks. For example, a
membership's `INHERIT` or `SET` option needs PostgreSQL 16 or newer. If a
caller does not set the flag, `plan` keeps its old behavior: it passes no
`postgres_version` render option, so a version-gated feature still
refuses. The flag changes only this refusal. It never changes the rendered
SQL or the plan digest of a feature with no version gate. `apply` has no
`--postgres-version` flag.

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

## Checkpoints

`migrate checkpoint create --dir PATH --version VERSION --generation-config FILE`
replays the verified directory (or its latest checkpoint plus suffix) in a
distinct development database, inspects the canonical schema, renders the full
schema from empty, and proves the rendered SQL reaches the identical semantic
fingerprint in another empty database. Publication uses the migration
directory's manifest compare-and-swap, so concurrent creators have one winner
and failures before publication leave no visible checkpoint.

Every checkpoint manifest entry and generator/release-signed artifact bind the
covered first and last revision, covered-head chain digest, exact schema
fingerprint, and data policy. `schema_only` refuses covered INSERT, COPY, UPDATE,
or DELETE migrations. `declared_replay` requires every such revision through
repeatable `--declare-replay VERSION` and matching `CheckpointDataPolicy`,
`CheckpointDeclaredReplay`, and `CheckpointPolicyApproved` values in the
trusted generation configuration; checkpoints never silently claim that schema rendering
preserves data or seed rows.

`migrate checkpoint verify --dir PATH --generation-config FILE [--json]` is
read-only and verifies generator and release signatures, environment/database,
expiry, approval, typed validation attestations, and the
immutable generation plus checkpoint range, head, artifact, fingerprint, and
data-policy bindings. A fresh database applies only the latest valid checkpoint
and its suffix. A database with any recorded revision in a checkpoint's covered
range ignores that checkpoint and continues with the suffix, preventing schema
re-creation on incremental targets.

## Owned development databases

`database dev create --config PATH --env NAME --postgres-version MAJOR
[--lease-dir PATH] [--json]` provisions one fresh database and restricted login
through an explicitly allowlisted version-2 `existing_admin` or `container` factory.
`database dev cleanup --lease PATH [--json]` recovers only that recorded lease.
These commands initialize their own development services and never resolve
production Apply configuration or project target credentials.

Success data includes the lease path, run/case IDs, state, actual PostgreSQL
major, deadline, and resource identities/states. Create additionally returns a
private `credential_ref`, never a connection string. Failed operations preserve
available lease data and cleanup categories with `ok: false` and recovery
guidance. Exit status and lifecycle details are in
[database testing](database-testing.md). Test-language commands remain separate work.
