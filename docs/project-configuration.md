# Project configuration

Version 2 describes named environments in JSON or HCL. Use it to validate source
paths, target references, development factory settings, and test/report defaults
without fetching credentials or opening a database. This page documents the
candidate implementation; it is not evidence of a published release.

```sh
autosql config validate --config examples/project-testing/autosql.hcl --env local --json
```

`--config` selects the file explicitly. The default remains `autosql.json`;
there is no new file discovery. A `.hcl` extension selects HCL, and other paths
select JSON. The executable skips production Apply service initialization for
configuration validation, schema loading, and base planning, even if
`AUTOSQL_APPLY_CONFIG` is set. Trusted plan edit/revalidate/publish commands keep
their production service initialization.

The [executable example](../examples/project-testing/README.md) includes
equivalent local, CI, and production configurations. Run its tests without any
database credentials:

```sh
go test ./examples/project-testing
go test ./pkg/config ./internal/cli
```

## Selection and precedence

The selected environment is the file's `environment`, overridden by
`AUTOSQL_ENV`, then explicit `--env`. Selection must name exactly one existing
environment; a single available environment is not an implicit default.
Version 2 validates all declared environments, including unselected ones.
Duplicate names, unknown fields, wrong types, and undeclared references fail
with file, line, and column. Errors caused by overrides identify `<environment>`
or `<flag>` instead of attributing the value to the project file.

Selected environment fields use file < supported `AUTOSQL_*` < explicit command
flags. Supported environment variables remain `AUTOSQL_TARGET`,
`AUTOSQL_DEV_DATABASE`, `AUTOSQL_SCHEMA_SOURCES`, `AUTOSQL_MIGRATION_DIR`,
`AUTOSQL_REVISION_SCHEMA`, `AUTOSQL_INCLUDE`, `AUTOSQL_EXCLUDE`, and
`AUTOSQL_TIMEOUT`. List overrides replace the entire corresponding list.
Source/filter environment lists use comma-separated values.

`AUTOSQL_TARGET` and `AUTOSQL_DEV_DATABASE` supply runtime secret values; the
effective configuration retains only their `env://AUTOSQL_*` identifiers.
Explicit `--target` and `--dev-database` take secret references. Repeatable
`--schema-source`, `--include`, and `--exclude`, plus `--migration-dir`, retain
their existing meaning. The validation command's `--timeout` bounds the command;
it does not override the environment's runtime timeout.

With `--json`, version 2 adds `data.effective` containing `name`, `values`, the
selected `development_factory` if any, and `provenance`. Provenance keys are JSON
pointers relative to the selected environment; `/environment` identifies the
selection, and `/development_factory/...` identifies factory settings. Each
origin has `kind: file|environment|flag`; file values include a `range` with
`file`, `line`, `column`, `end_line`, and `end_column`. A normalized default has
`default: true`. Replaced lists do not retain stale per-item file origins.

Configuration failures use exit code 3. JSON failures add `error.field` and
`error.location` when available, alongside the existing error envelope. Error
messages omit invalid field values. Variables must contain non-secret values;
only target/admin references represent credentials.

## Version-2 fields

The root contains `version: 2`, optional `environment`, `environments`, optional
`variables` (a non-secret object), and optional named `dev_factories`. JSON writes
already expanded environments. Every environment has:

| Field                | Meaning                                                                                                                    |
| -------------------- | -------------------------------------------------------------------------------------------------------------------------- |
| `target`             | Required `env://NAME` or `file://` secret reference.                                                                       |
| `schema_sources`     | Nonempty ordered list of `{name?, kind, path}`; `kind` is `sql`, `json`, or `hcl`.                                         |
| `migration_dir`      | Required project-relative directory.                                                                                       |
| `classification`     | Optional `development` or `production`; omission grants no development authority.                                          |
| `dev_database`       | Optional legacy development-database secret reference.                                                                     |
| `dev_factory`        | Optional name in `dev_factories`; mutually exclusive with `dev_database`.                                                  |
| `revision_schema`    | Lowercase PostgreSQL identifier; default `autosql_revision`.                                                               |
| `include`, `exclude` | Ordered lists of nonblank filters.                                                                                         |
| `variables`          | Non-secret string map retained for existing runtime consumers.                                                             |
| `timeout`            | Positive Go duration; default `30s`.                                                                                       |
| `tests`              | `{files?, parallel?, timeout?}`; project-relative patterns, parallelism 1–32 (default 1), positive timeout (default `5m`). |
| `policy_files`       | Ordered list of project-relative policy paths.                                                                             |
| `registry`           | `{url?, lock?}`; credential-free HTTPS URL without query/fragment and project-relative lock path.                          |
| `reporting`          | `{json?, junit?}`; JSON preference and optional project-relative JUnit output path.                                        |

Durations and relative paths are normalized in the effective output. Source,
migration, test, policy, lock, and report paths must stay relative to the project;
absolute paths, parent escapes, URI schemes, and backslashes are refused in
version 2. Secret references have their own syntax and are not source paths.
Ordinary validation checks path syntax without requiring the files to exist.

Local inspection checks selected schema files and the migration directory,
including symlink containment. Inspection does not grant a future file-open
authorization: a consumer must check containment again when reading inputs.
Policy/registry/test/report references do not cause file reads, network fetches,
external programs, or SQL during ordinary validation.

## HCL environments and finite expansion

HCL uses attributes for typed nested settings, and named blocks for environments
and factories:

```hcl
version     = 2
environment = "tenant/blue"
variables   = { tenants = { blue = "BLUE_TARGET", red = "RED_TARGET" } }

environment_set "tenant" {
  for_each       = var.tenants
  target         = "env://${each.value}"
  schema_sources = [{ kind = "hcl", path = "schema.hcl" }]
  migration_dir  = "migrations/${each.key}"
  tests          = { parallel = 1, timeout = "5m" }
}
```

Use `environment "name" { ... }` for an explicit environment and
`dev_factory "name" { ... }` for a factory. Set expansion names environments
`set-name/key`. Keys are sorted, names must be unique across explicit and expanded
environments, and the total cannot exceed 256. Names start with an ASCII letter
or digit and otherwise contain letters, digits, `_`, `.`, or `-`; an environment
may contain one `/` separator. Factory/set names and individual set keys cannot
contain `/`.

Expressions support literals, declared `var` values, and `each.key`/`each.value`
within a set. Function calls, comprehensions, arithmetic, conditionals, remote
lookups, and implicit environment-variable functions are unsupported. Unknown
references and malformed templates fail even when a set is empty; an empty set
does not provide an environment for selection.

Project files are limited to 4 MiB, nesting to 32 levels, and parsing to 100,000
JSON nodes or HCL tokens. HCL interpolation also has a conservative 4 MiB value
evaluation budget, and its expanded JSON model must fit within 4 MiB. These
limits prevent small templates from allocating unbounded expanded projects.

## Development factory declarations

Both modes require PostgreSQL major 14–18 and a nonempty `allowlist`.
`protected_targets` optionally lists credential-free PostgreSQL endpoint/database
identities. `isolation` is `database` (default) or `schema`. Positive durations
default to `startup_timeout: 2m`, `case_timeout: 5m`, and `cleanup_timeout: 10s`.

| Mode             | Additional fields                                                                                                                                                                                                 |
| ---------------- | ----------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `existing_admin` | `admin` secret reference, `admin_classification: development`, and an allowlist of credential-free `postgres://host[:port]/database` identities. No container endpoint or image.                                  |
| `container`      | Explicit `endpoint` (`unix:///absolute/socket` or `tcp://host:port`), digest-pinned `image` ending in `@sha256:` plus 64 lowercase hex digits, and an allowlist of explicit daemon endpoints. No admin reference. |

Parsing validates these declarations; it neither verifies a live endpoint's
identity nor grants permission to create or delete a database. Factory runtime
must verify positive allowlisting, protected aliases, actual PostgreSQL version,
and ownership before any mutation. A development-sounding environment name does
not supply that authority. The candidate `database dev create` command now
executes both modes with fresh database isolation; schema isolation remains
refused by that command. See [database testing](database-testing.md)
for the lifecycle and the roadmap's [factory contract](../specs/001-operator-reliability/contracts/project-testing.md).

## Compatibility and implementation boundary

Version-1 JSON, selection/override precedence, direct command flags, and its
normal validation result shape remain supported. Optional version-1 null fields
retain their previous zero-value behavior. New fields and HCL projects require
version 2; older binaries reject the new version. Version 1 retains its previous
path rules and SQL/JSON source kinds. Version 2 rejects null in typed fields.

`--preflight` remains an explicit request to resolve the selected target and
legacy development-database references and inspect their local paths, without
opening a database. It does not provision a named factory. Library consumers can
use `InspectLocal` without a resolver, or explicitly call `ResolveTarget` for
only the selected target. Test discovery must use the former.

This milestone implements parsing, validation, normalization, provenance,
configuration identities, purpose-specific resolution, and project-aware schema
loading and planning. The library also captures effective input manifests and
provides approval/attestation binding primitives. Release command integration
and current-input revalidation, owned database factories, and the test
runner remain subsequent roadmap work. A configuration validation result is not
an approval, test pass, release, or deployment claim.

## Configuration identity

Version-2 validation adds `data.identity.config_digest` and
`data.identity.execution_config_digest`, both domain-separated SHA-256 values.
They bind the exact selected environment, normalized settings and selected
factory, secret reference identifiers, and environment/flag override origins.
Field ranges, formatting, and the absolute location of the project checkout do
not enter either digest. Equivalent JSON/HCL settings have the same identities.
Ordered source lists remain ordered. Configured timeout settings are included;
measured runtime durations are not inputs.

`config_digest` also covers reporting preferences and destinations.
`execution_config_digest` excludes them, so moving a report does not change the
configuration identity used for future SQL approval binding. Rotating the value
behind an unchanged secret reference does not change either digest; runtime must
still verify the resolved target's identity and authorization independently.

These configuration hashes do not read or bind source, migration, policy, or
test file bytes. Input-byte inspection and release verification are required
before claiming those bindings. Version 1 retains its previous output and makes no project identity
claim. Run `go test ./pkg/config -run TestProjectIdentity` for the executable
identity and compatibility contract.

The explicit library input reader, `Config.OpenInputFiles`, captures immutable
local file bytes and project-relative content records. It supports the existing
HCL module loader through `ReadFile`/`ReadDir`, confines reads during file open,
and refuses non-regular inputs such as named pipes. Relative symlinks must stay
inside the project; absolute symlinks are refused by this reader. Limits are
32 MiB per file, 128 MiB total, and 4096 files or entries in a directory.
`go test ./pkg/config -run TestProjectInputFiles` exercises these boundaries.
Content records alone are not a complete project input manifest or signed
release binding; the attestation integration must assemble
and verify those before execution.

`Config.CaptureInputManifest` completes a source loader's immutable capture with
the selected test, policy, registry-lock, and verified migration inputs. Tests
use finite component globs, with sorted results and no recursive `**` extension;
configured patterns must match files. This step binds test bytes without
claiming that the test language was validated or executed. It resolves no secret
or network registry. The command supplies a digest of its non-secret execution
options; a verifier must recompute that context independently.

The resulting `autosql.project-inputs/v1` manifest contains the configuration
identities, captured file records, migration snapshot digest and ordered logical
migration paths, execution context digest, and `effective_input_digest`. Report
destinations affect the configuration digest but not the effective input digest.
The complete inventory is limited to 4096 files and 128 MiB. Migration files
retain the migration core's individual size, ownership, and integrity checks.

Migration inspection uses an already opened, confined directory and does not
create a lock or modify any files. Release generation binds the preceding
migration snapshot. Verification can reconstruct that prefix from a verified
current generation, avoiding self-reference through the newly generated signed
artifact. The entire current snapshot is checked before extracting its prefix.
Untracked or modified migration files still refuse inspection.

The generation library accepts the captured input digest and expected migration
snapshot, refuses a changed snapshot before database replay, and carries the
input digest through approval and signing. The artifact library composes it into all three generated
validation stages using the existing signed metadata and digest fields. Such an
artifact refuses verification without a matching independently recomputed input
digest. The guardrail library also includes that digest in the approval bundle,
so unchanged SQL with changed project inputs requires fresh approval. Legacy
artifacts/bundles omit these fields and retain their existing verification path.
Edited project releases are currently unsupported. Project-aware generation and
Apply use these bindings through the command workflow below.

## Project schema loading and planning

```sh
autosql schema load --config project.hcl --env local --json
autosql schema load --config project.hcl --source hcl:replacement.hcl --json
autosql plan --config project.hcl --env local --from sql:current.sql --json
autosql plan --config project.hcl --env local --json
```

`schema load` composes the selected environment's ordered sources. Repeatable
`--source sql:PATH|json:PATH|hcl:PATH` replaces that whole list; explicit formats
do not depend on file extensions. HCL imports/modules use the same bounded
input reader, and the environment's non-secret `variables` supplies HCL inputs.
The command uses the environment timeout unless `--timeout` overrides it; this
override participates in the configuration identity. No target, development
database, or factory credential is resolved.

`plan` uses the same configuration resolver and desired-source loader. Omitted
`--from` inspects only the selected target through its secret reference; an
explicit local `--from sql:PATH|json:PATH|hcl:PATH` avoids database access.
`--from live:env://NAME` or a file reference explicitly chooses another current
source. `--to` replaces the configured desired-source list with one local source.
Project paths resolve relative to the project file. Local desired inputs are
validated before live inspection. Providers and live desired sources remain
available through the existing direct command mode.

Configured `include`/`exclude` and their explicit flag replacements select both
graphs before computing plan fingerprints. Project planning supports these
filters for local sources; `--schema` remains limited to two direct live sources.
`--max-changes` and `--postgres-version` retain their existing behavior. Planning
uses the selected environment's timeout and receives no mutation capability.

Project JSON output contains `data.environment`, `data.schema` or `data.plan`,
and, for version 2, `data.identity` and `data.source_inputs`. Source records name
the project-relative files actually consumed, their byte digests, and sizes;
planning includes local current-state inputs too. The configuration identity
does not cover command-only planning options, live catalog state, or file bytes.
The plan digest covers the resulting plan. Neither output is a signed release
authorization or a complete effective-input attestation. Unrelated migration,
test, policy, and lock files are not read by these commands.

Version-1 project commands retain SQL/JSON and their existing path rules but
make no new identity claim. Direct `schema load --source ...` still returns the
schema document directly in `data`; direct planning retains its output shape
and selector rules. `--env` requires `--config` for these two commands.

## Project release generation and verification

```sh
autosql migrate generate --config project.hcl --env ci \
  --version 1 --label create_accounts --generation-config generation.json --json
```

The project selects local desired inputs and an already initialized migration
directory. The trusted generation configuration supplies the development/target
secret references, signing and approval authority, policy identity, and release
lifetime. Its environment and target reference must match the selected project.
The generation command owns this configuration and does not initialize unrelated
`AUTOSQL_APPLY_CONFIG` services. Legacy direct generation remains available with
`--dir` and `--to`; its JSON result shape is unchanged.

Project generation accepts `--to sql:PATH|json:PATH|hcl:PATH` as a complete desired
source replacement and `--dir PATH` as a migration-directory replacement. Both
are relative to the project and their flag provenance is bound. The command
replays migration history; project mode refuses `--from`. It also refuses schema
filters, multiple policy files, and version-1 project files. The selected project
must name exactly one `policy_files` entry matching `generation.json`'s
`PolicyFile`. Nonlocal desired sources remain limited to direct mode. A named
development factory is bound as configuration but is not provisioned by this
command: the trusted `DevelopmentURL` still selects the existing development
server. A legacy `dev_database` reference, when configured, must match it.

All referenced project files are captured before secret resolution. The migration
directory remains pinned through replay and atomic publication, and a changed
snapshot refuses generation. Relative secret-file references resolve from the
project root in this mode. The project timeout covers the command; measured
durations and resolved secret values never enter its identities.

Project JSON output contains `data.generation` (the existing generation result),
`environment`, `input_manifest`, `project_verification`, and `release_evidence`.
The last item includes approval evidence and policy resource inputs needed by a
trusted release process. A no-op returns no release evidence and publishes no new
artifact. The signed artifact contains the canonical input manifest in metadata
`autosql.project.manifest`; `autosql.project.inputs` composes its digest into all
three validation stages. The command's output is evidence to review, not an
authorization to install a release.

For Apply, the trusted `TrustedMigrations[artifactDigest].Project` entry supplies
the project path, exact overrides, execution settings, and pre-generation
`migration_prefix` count. These are the fields returned in
`project_verification`. A relative project path resolves from the Apply
configuration's directory. The release owner installs this entry with the
existing independently trusted artifact bindings, attestations, policy,
approver identities, and original approval evidence. Artifact metadata never
selects a project path or supplies trusted overrides.

Every verification and execution reauthorization reloads those current project
inputs, reconstructs the migration prefix, and requires the exact signed
artifact at the expected published migration entry. Changing source, test,
policy, lock, migration, or execution inputs refuses Apply, including when a
verification policy was cached. A later valid migration may be appended; removing
the release or tampering with the current snapshot refuses it. Report destinations
may move. A secret reference may rotate only to the same validated database:
Apply checks the actual execution connection's cluster/database identity against
the signed simulation attestation. That check requires permission to read
`pg_control_system()` and does not open a second connection.

The optional signed automation-approval settings and the disposable PostgreSQL
proof are documented in [migration generation](migration-generation.md). Original
approval lifetimes, release/attestation lifetimes, and current signing-key checks
remain enforced; an expired published release is still refused.
