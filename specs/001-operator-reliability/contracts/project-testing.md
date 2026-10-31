# Project and Testing Contracts

These are proposed contracts for R2/R3. They extend existing commands and
packages; they are not descriptions of functionality already shipped.

## Project configuration

Keep `autosql config validate --config PATH --env NAME [--preflight] [--json]`.
Keep version-1 `autosql.json`, its existing environment precedence, and direct
command flags. Add explicit version-2 JSON and HCL project inputs that normalize
to one internal model; HCL files are selected by `--config`, with no ambiguous
new automatic discovery. An old binary rejects version 2 rather than ignoring
it.

Version 2 adds `tests`, named `dev_factories`, policy/registry references, and
reporting defaults to existing environment settings. The schema source kinds are
local SQL, canonical JSON, and author HCL. Config-only registry references do
not authorize network access or implement the broader data-source runtime.

HCL uses `version = 2`, `environment "name" { ... }`, and optional
`environment_set "name" { for_each = ... }` blocks. A set expands names as
`set-name/key`; explicit and expanded names must be unique. `for_each` accepts a
finite map supplied in the project, with at most 256 expanded environments.
Evaluate only declared non-secret variables and `each.key`/`each.value`; no
shell evaluation, remote data lookup, implicit environment-variable function, or
unrestricted function library. JSON expresses already expanded environments.
Unknown blocks/attributes, types, or references fail with file/line/column.

The concrete version-2 field and default contract is documented in
[`docs/project-configuration.md`](../../../docs/project-configuration.md), with
equivalent local/CI/production inputs under `examples/project-testing/`.
Root `variables` declares non-secret values; `tests`, `registry`, and `reporting`
are typed attributes, while `dev_factory` declares a named factory block.
The implementation bounds project files and expanded models to 4 MiB and
structure to 32 levels/100,000 nodes or tokens. Empty sets still validate their
template's types, fields, and references. These parsing limits grant no runtime
factory or production-target authority.

Selection is file default < `AUTOSQL_ENV` < explicit `--env`. Field overrides
remain file < supported `AUTOSQL_*` < explicit command flags. Return the
selected name, effective non-secret values, and field provenance (`file`,
`environment`, `flag`), including source ranges for file values. Do not silently
fall back to a different environment when a name is absent or invalid.

Normal validation parses configuration and secret references only. Existing
`--preflight` may resolve references and inspect local paths but must not open a
database. Test discovery validates file locations and syntax without resolving a
production target. Test runtime resolves only the selected development factory's
references; it must not call the existing all-target `Config.Preflight` blindly.

Extend `schema load` and `plan` to accept `--config PATH --env NAME` through the
same resolver, while preserving existing explicit source flags. Explicit list
overrides replace the corresponding configured list rather than appending hidden
inputs. Keep the existing `migrate apply`/`status` config entry points. A
project-aware release invocation must use the attestation binding below; a
legacy direct invocation retains its documented legacy behavior.

### Effective input identity

Compute a domain-separated SHA-256 digest of the normalized selected
environment, execution-relevant settings, secret reference identifiers, override
provenance, project-relative source paths and content digests, migration
snapshot digest, test file digests, and referenced policy/lock digests. Include
reporting settings in the report configuration digest; output file location need
not invalidate SQL approval. Exclude resolved secret values, timestamps,
machine-specific absolute checkout paths, temporary resource names, and
measured run durations. Configured timeouts remain execution settings and are
included. Preserve semantically significant source/migration ordering; sort
unordered maps.

Distinguish a configuration digest available after parse from an effective input
digest available after input-byte inspection. Reports name both. For
project-based release generation, compose the effective input digest into
existing stage `ConfigDigest` attestation inputs and recompute it during
verification. Include the same digest in the guardrail approval bundle: an
input-only edit must invalidate old approvals even when the generated SQL is
unchanged. Omit that additive bundle field for legacy inputs to preserve their
existing digest representation. Do not misuse source revision or trust an unsigned report as an
approval binding. New project-aware releases without matching attestations
refuse application. Existing legacy releases retain their existing validation
path and make no new project binding claim. The artifact wire version need not
change if existing digest slots can express this composition; prove that with
tamper and compatibility tests.

Versioned generation binds its pre-generation migration snapshot. Verification
reconstructs that exact prefix from a fully verified current generation, so the
artifact does not depend on its own serialized bytes. Appending a later valid
migration does not rewrite the earlier release's input snapshot; modifying or
removing an input does. Current project sources, tests, policies, lock files,
and execution options must still match. Edited project releases fail explicitly
until their project input revalidation path is implemented.

The implemented generation adapter uses `migrate generate --config PATH --env
NAME --generation-config TRUSTED`. Its trusted execution context binds version,
label, canonical rename hints, PostgreSQL major, and policy file/identity. Apply's
per-artifact trusted `Project` entry supplies the local path and overrides;
neither paths nor expected context come from artifact metadata. Generation pins
the confined migration directory through publication. Verification requires the
exact artifact in the current published snapshot and recomputes current inputs
even when an artifact verification policy was cached. Project execution also
checks the connected cluster/database identity against the simulation identity.
The existing signed automation approval provider can supply fresh bundle-bound
approvals; their original signed expiry remains authoritative.

Current generation boundaries are explicit: one project policy file, unfiltered
schema, local desired inputs, and an explicitly configured existing development
server. Factory provisioning and test-language execution remain the subsequent
scopes below. Version-1 and direct command compatibility stays intact.

## Disposable development factories

Lifecycle entry points (both factory modes implemented and qualified locally):

```text
autosql database dev create --config PATH --env NAME --postgres-version MAJOR [--lease-dir PATH] --json
autosql database dev cleanup --lease PATH --json
```

Test commands own create/cleanup automatically; the explicit commands support
interactive development and recovery of an interrupted run. No `--all` cleanup
or arbitrary database-name deletion is introduced.

Supported factory modes are `container` and `existing_admin`. The first uses an
explicit Docker-compatible daemon and digest-pinned PostgreSQL image. The second
requires a separately authorized development administration reference and target
allowlist; it verifies the actual major version rather than claiming to install
another version on that server. Images may be cached by digest. No writable
database state is reused across runs.

Provision a fresh database per case by default. A schema-only factory is opt-in
for schema-local cases proven not to use fixed schemas, database-level DDL, or
versioned history; ordinary migration tests always use a separate database. Test
SQL runs with a run-specific restricted role, never the provisioning
administrator. The administrator credential is not exposed to fixture execution
or child reports.

Before mutation, reject production-classified environments/references, protected
target identities, and normalized endpoint/database aliases that match
configured protected targets. Require a positive development-factory allowlist;
a missing production denylist or a user naming the environment `dev` is not
sufficient. Identity comparisons use resolved connection identity without
logging credentials. There is no universal discovery of every production server:
the trusted factory allowlist is an explicit operator-maintained boundary,
tested against aliases.

Each acquisition creates a random run/case identity and a durable local lease
before the first resource mutation. Record ownership incrementally. Containers
carry ownership labels; database/role ownership has a matching creation record
and a verified marker in the dedicated development target. Cleanup compares both
the lease and actual resource ownership. A name prefix alone never authorizes
deletion. Missing or inconsistent ownership refuses cleanup and reports the
residual safely.

Defaults are a two-minute startup deadline, five-minute case deadline, and ten
seconds per cleanup action, with explicit positive configuration overrides.
Cancellation stops new cases and gives each started case a fresh bounded cleanup
context. Failure to clean remains a failure with the lease path/resource
identity. A force-killed process may leave resources; the explicit cleanup
command recovers only its recorded lease. Credentials stored for an interactive
run must be in an owner-only runtime file, referenced by the output, and removed
with the lease; they must never be embedded in the lease, stdout, or a committed
project file.

Both factory modes retain a nonsecret cleaned lease after removing
credentials/resources, and rechecks live absence on repeated cleanup. The private
runtime root defaults to the user cache's `autosql/development` directory.
JSON failures retain available lease state and bounded cleanup categories in
`data`; an output failure during create cleans the allocation. Runtime and
recovery details are in [`docs/database-testing.md`](../../../docs/database-testing.md).

## Test file language

Use versioned `*.test.hcl` files, parsed into located immutable cases. A file
declares `version = 1` and uniquely named `schema_test` or `migration_test`
blocks. Both can contain `setup`, `fixture`, `exec`, `assert`, `expect_error`,
and `cleanup` blocks with source locations. `skip` requires a nonempty
explanation; `timeout` is a positive bounded duration.

Schema tests prepare the selected desired schema on the isolated target before
fixtures and checks. Migration tests add an ordered `version` sequence: apply
the initial version, run its `seed` operations, then apply the later version and
its checks. Missing versions, backwards transitions, or modified migration
manifests fail before executing the affected step. Sources come from the
selected project; arbitrary shell hooks or embedded production credentials are
not language features.

`assert` contains a SQL query, optional bound parameters, and a typed `equals`
value (integer, string, boolean, or null). The query must return exactly one row
and one column; type mismatch, multiple rows/columns, and numeric overflow fail.
Do not interpolate arbitrary assertion values into SQL. Preserve existing
library callers of integer-count assertions through a compatibility adapter.

`expect_error` names one execution operation and one exact SQLSTATE. A different
error, no error, or a connection/cancellation/cleanup failure fails the case.
Never accept substring matching against raw backend messages as success. The
operation uses a disposable transaction/savepoint where supported so an expected
error does not poison later steps; explicitly nontransactional cases use a fresh
connection and remain subject to case cleanup.

Test-only fixture execution is authorized solely by an owned disposable lease.
Reuse migration snapshot parsing, ordering, and rendering. Do not route tests
through production Apply with trust checks disabled or add a production
`--skip-approval` flag. Qualification of the signed release path is a separate
existing real-signature integration scenario.

## Test commands and output

```text
autosql schema test --config PATH --env NAME [--filter GLOB] [--parallel N]
                    [--json] [--junit PATH]
autosql migrate test --config PATH --env NAME [--filter GLOB] [--parallel N]
                     [--json] [--junit PATH]
```

Discover files from the selected project's test patterns in stable relative-path
order. Default parallelism is one; allow 1–32 bounded concurrent cases on
separately owned databases. A filter applies to fully qualified
`relative-file::case-name`. Validate all discovered syntax before provisioning;
no selected runnable cases is a validation failure, even when all selected cases
are skipped.

Use the existing `autosql.output/v1` envelope: one JSON object on stdout with
`--json`, diagnostics on stderr. Suite data includes
selected/passed/failed/skipped counts, ordered cases, project/input/migration
digests, binary version, database major, outcome, primary failure, cleanup
failures, and source locations. Timing and run identities are explicitly
nondeterministic; case ordering and semantic outcomes are stable. Extend the
error envelope additively to preserve structured suite data on a failing run; do
not discard case outcomes just because `ok=false`.

JUnit writes atomically to the requested local path and represents assertion
failures, infrastructure/cleanup errors, and skips distinctly. No report
includes credentials, connection strings, or unrestricted backend error text. A
report-write failure produces nonzero exit status even after passing tests.

Reuse existing exit codes: 2 usage, 3 configuration/language decode, 4 secret
resolution, 5 assertion/selection/validation, 6 connectivity, 7 migration
execution, 8 ownership/concurrency conflict, 124 timeout, and 130 cancellation.
Cleanup failure alone uses 7; report I/O failure alone uses 1. Preserve an
original nonzero code when cleanup/reporting also fails, and represent all
failures in available output. Across multiple cases, choose the first failed
case in deterministic discovery order, unless command-level cancellation/timeout
or setup validation already determines the result. Zero requires runnable cases,
successful execution, cleanup, and reports.

## Bounded release proof

Ship `examples/project-testing/` with local/CI/production configuration
examples, local SQL/JSON/HCL sources, two migration versions with seed data,
schema and upgrade tests, and human/JSON/JUnit runs. Production configuration is
validated but never used as a test target. The release proof includes the
existing signed-artifact integration path; passing a fixture test alone does not
prove release authorization.

Use a released binary (candidate builds clearly labeled during development),
record its immutable identity, and compare semantic results and input digests
across local and CI runs. Record actual case counts and cleanup, not only
process exit codes. These capabilities contribute to `autosql-f2x.8` but do not
satisfy its full locked external-source, every-export-format, ERD, or
whole-CLI-completion requirements.
