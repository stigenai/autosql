# Database safety and test harness

`pkg/precheck` provides `GuardedApply` for data-dependent migration checks. A
plan and each assertion carry the exact plan and change digests. The digest
includes each assertion's name, canonical query, arguments, maximum count,
timeout, and change identity, so approved behavior cannot be edited afterward.
Assertion SQL is parsed from a deliberately small grammar: `SELECT count(*)`
over identifiers with optional placeholder comparisons, NULL checks, and `AND`.
Functions, arbitrary selected values, comments, multiple statements, and other
SQL are rejected before a transaction begins. Guarded apply
opens one transaction, acquires its migration lock, executes scalar count
queries, and only then runs mutation statements. A mismatch, failed check,
timeout, or cancellation rolls the transaction back. Results expose only the
assertion name and counts; assertion queries cannot return sampled rows.

The legacy `pkg/dbtest.Runner` runs every case in an isolated database supplied by a `Factory`.
Cases may set up a blank schema, load fixtures and seeds, apply any number of
named migration versions and plans, and execute scalar SQL assertions. `${name}`
variables are expanded deterministically. Commands and assertions retain file
and line metadata for useful CI failures. Teardown commands run in reverse
order, followed by database close, even when work fails, times out, or is
cancelled. Every teardown and close action gets its own fresh bounded context;
all cleanup errors are retained alongside the primary failure. PostgreSQL setup
also uses fresh contexts to remove a schema and close its connection after
partial setup failure.

## Versioned test files

`pkg/dbtest.ParseTestFile` parses version-1 `*.test.hcl` without filesystem,
credential, process or database access. Parsed files/cases are immutable; their
accessors return detached operation/parameter snapshots. The candidate CLI runs these files with `schema test` and `migrate test`, using
the selected project and an owned development factory. The legacy library
runner remains available with its existing API.

See [schema.test.hcl](../examples/project-testing/tests/schema.test.hcl) and
[upgrade.test.hcl](../examples/project-testing/tests/upgrade.test.hcl). Verify
their syntax with `go test ./examples/project-testing -run TestTestLanguageExamples`.
The example migration directory contains a verified version-1/version-2
snapshot; its upgrade case checks seeded rows after adding a defaulted column.

A file declares `version = 1` and uniquely named `schema_test "name"` or
`migration_test "name"` blocks. Each accepts `skip = "reason"` and
`timeout = "30s"`; a supplied timeout must be positive and at most 24 hours.
Defaults come from the runner/factory. A skip never exempts a case from syntax
validation. Files are bounded to one MiB, 65,536 tokens and 32 nesting levels.
Names are limited to 200 characters, without control characters or `::`.

Operations retain declaration order and exact source ranges:

| Block | Attributes | Behavior |
| --- | --- | --- |
| `setup`, `fixture`, `exec` | `sql`, optional `params` | One SQL statement, bound scalar parameters |
| `assert "name"` | `sql`, optional `params`, `equals` | Exactly one row/column, strict scalar equality |
| `expect_error "name"` | `sql`, optional `params`, `sqlstate`, optional `transaction` | One exact operation error; rollback by default |
| `cleanup` | `sql`, optional `params` | Registered for fresh-context cleanup in reverse order |
| `version "1.0"` | Nested operations | Migration tests only; advance to an existing version |
| `seed` | `sql`, optional `params` | Inside a migration version, after applying that version |

The `version` block also accepts setup, fixture, exec, assertions, expected
errors and cleanup. Versions increase strictly using the existing migration
version rules (`1` and `1.0` name the same version). `BindMigrations` validates
the complete migration manifest and its SQL/artifact bytes, rejects missing or
nonlinear transitions, and captures each forward interval before execution.
Captured statements do not alias the caller's mutable snapshot. This test-only
binding grants no production Apply authority and changes no approval rules.

Values are literals: signed 64-bit integers, strings, booleans and null. `params`
is a literal list of at most 1,024 such values. Functions, variable interpolation,
arithmetic, shell hooks and target/credential fields are rejected. Each SQL
operation is checked as a single statement and the owned-target methods enforce
that limit through PostgreSQL's extended protocol, including zero-parameter SQL.

`DisposableTarget.QueryScalar` accepts PostgreSQL integer, text, boolean and null
results. It refuses empty/multiple rows, multiple columns, numeric/float coercion
and unsupported types. Overflow remains a failure. Parameters are bound, never
substituted into SQL. Existing `Database.QueryCount`, `Assertion.Want` and
legacy `Runner` callers keep their integer-count behavior.

`DisposableTarget.ExpectError` matches one exact SQLSTATE from the operation,
never backend message text. Success, warning, connection, cancellation,
operator-intervention and system-error classes cannot be expected successes.
`transaction = "required"` is the default: a disposable transaction is rolled
back even if the operation unexpectedly succeeds. `transaction = "forbidden"`
uses a fresh, identity-checked restricted connection and closes it afterward.
Any rollback/close error remains a failure. Deadline, cancellation, transport and
joined cleanup failures never become a passing expectation. Diagnostics exclude
SQL, parameter/observed values and unrestricted backend messages.

## Project test commands

```sh
autosql schema test --config examples/project-testing/container.hcl --env local --parallel 2 --json --junit reports/schema.xml
autosql migrate test --config examples/project-testing/container.hcl --env local --filter 'tests/*.test.hcl::seed survives upgrade' --json --junit reports/upgrade.xml
```

The container example requires an explicitly approved daemon and matching pinned
image. The existing-admin examples require their approved administrator reference
and allowlist. Tests never resolve the project's target or construct production
Apply services. A version-1 project, production environment, or missing factory
is refused. `--config` is required; `--env` selects a named environment.

Discovery uses `tests.files`, with project-relative component globs (`*`, `?`,
`[...]`, no `**`). All discovered files must end in `.test.hcl` and parse before
filtering. Files are sorted by relative path; cases keep declaration order.
`--filter` matches `relative/file.test.hcl::case name` with slash-aware glob
semantics. Zero selected cases and all-skipped selections return exit 5 before
provisioning. Syntax-invalid skipped or filtered-out cases are still errors.

`--parallel 1..32` overrides `tests.parallel`. `--json[=false]` overrides
`reporting.json`; `--junit PATH` overrides `reporting.junit`, and `--junit=`
disables it. Relative report paths resolve beside the project file. Reports
cannot replace configuration, captured inputs, migration directories, declared
schema/policy/lock files or file credentials. JUnit uses a private temporary file,
fsync and atomic replacement; symlink, hard-link and non-file destinations are
refused. A report error is never success.

Each schema case runs its leading `setup` blocks on a blank owned target, applies
the captured schema plan, then runs fixtures and checks in declaration order.
Schema setup after a fixture/check is a syntax error. Migration cases execute
ordered version intervals from one verified snapshot and run nested seeds/checks
after each interval. `auto` and `required` migration files use a transaction;
`forbidden` runs without an outer transaction. Explicit transaction-control SQL
in `auto`/`required` files is refused during binding, before provisioning. These are disposable test runs,
not production revision recording, signed artifact Apply or release qualification.
The configuration and consumed source/test bytes are checked again before each
allocation; migration bytes and manifest are checked before each version too.
A changed capture is refused, never silently refreshed.

The case budget starts after provisioning: case `timeout`, then `tests.timeout`
(default five minutes). The factory's `case_timeout` lease deadline is an
additional hard cap; a longer case timeout cannot extend it. Provisioning uses
`startup_timeout`. Fixture cleanup runs in reverse order, then resource cleanup;
every action receives a fresh `cleanup_timeout` budget, even after cancellation.
Cancellation stops new starts and waits for all started cases to attempt cleanup.
Abrupt process death uses the recorded exact-lease recovery command.

Human, JSON and JUnit come from one ordered result model. JSON retains suite data
on failure: selected/executed/passed/failed/skipped/not-run counts; case source
ranges, stages, versions, assertion counts, actual major, primary and cleanup
failures, durations and lease state. It includes binary version, selected
settings, project identity and an input digest binding consumed bytes, selected
cases and parallelism. Measured durations and random lease identities naturally
vary; result ordering does not. SQL, parameters, observed scalar values,
unrestricted backend errors, secrets and PostgreSQL connection URLs are excluded.

JUnit represents assertion failures as `failure` and infrastructure/cleanup
failures as `error`, retaining both when they coexist. Selection/command failures
get a suite error case so an all-skipped selection cannot look green. The first
failed case in discovery order chooses the exit code; command cancellation or
deadline takes precedence. Cleanup never replaces a primary failure: cleanup-only
uses 7, report-only uses 1, assertions/selection use 5, configuration 3, secret
resolution 4, connection 6, ownership 8, timeout 124 and cancellation 130.
Case-specific `assertion` and `cleanup` kinds map to the existing envelope kinds
`validation` and `migration`. Legacy runner/count-assertion APIs are unchanged.

Run the regression and live acceptance checks with:

```sh
go test -race ./pkg/config ./pkg/source ./pkg/dbtest ./internal/cli
go test -race ./pkg/dbtest ./internal/cli -run '^(TestSuiteLive|TestProjectTestLive)' -count=1 -v
```

The live command requires `AUTOSQL_DBTEST_ADMIN_DSN` for an authorized disposable
server, plus `AUTOSQL_DBTEST_DOCKER_ENDPOINT`, `AUTOSQL_DBTEST_POSTGRES_IMAGE`
(digest pinned), and `AUTOSQL_DBTEST_POSTGRES_MAJOR` for container cases. Missing
live settings skip those cases; a skipped invocation is not qualification evidence.

## Owned development databases

The candidate CLI and `dbtest.AdminFactory` support `existing_admin` and `container` factories
from version-2 projects. Every case gets a fresh database from `template0` and a
random restricted login. The existing `PostgresFactory` schema-only library API
is unchanged. Configuration validation alone does not execute test files;
use the explicit test commands below.

```sh
autosql database dev create --config examples/project-testing/development.hcl --env local --postgres-version 17 --json
autosql database dev cleanup --lease /absolute/path/from/create/lease.json --json
```

Before creation, set the example's `AUTOSQL_EXAMPLE_LOCAL_ADMIN` secret reference
to an authorized development administrator URL, and maintain the factory's
credential-free allowlist for that actual administration database. The example
allows `127.0.0.1:55432/postgres`; it supplies no administrator credentials and
does not start a server. Change the allowlist and requested major together when
using another development server. The selected environment and administrator
must both be explicitly classified `development`. The actual connected major
must match the configured/requested PostgreSQL 14–18 major. An existing server
is never upgraded by this command.

The administrator needs database and role creation privileges, ownership of the
administration database, and permission to read `pg_catalog.pg_control_system()`
for cluster identity. A dedicated non-superuser with `CREATEDB`, `CREATEROLE`,
and an explicit execute grant is supported. Provisioning creates an owner-only
`autosql_development_leases.resources` registry in that database; creation
records remain after cleanup. Fixtures cannot read or modify that registry.
The administrator owns the fresh database; the restricted login has access to
its database and permission to create fixture objects. It has no superuser,
role/database creation, replication, bypass-RLS, or inherited role powers.

The CLI resolves only the selected administration reference. It does not resolve
project targets, inspect schema/migration files, or initialize production Apply
services. A production selection or a reference shared with a production
configuration is refused. Reference spelling is normalized before comparison.
A positive endpoint/database allowlist is required even with no protected
identities. DNS aliases and the actual connected peer are checked; a protected
endpoint protects every database on that endpoint. An unresolvable configured
protected hostname fails closed. This boundary depends on the operator's
allowlist and protected-target inventory; it cannot discover unlisted production
servers. PostgreSQL URLs with hidden host or session-option overrides are refused.

### Container factories

Use the [container project example](../examples/project-testing/container.hcl):

```sh
autosql database dev create --config examples/project-testing/container.hcl --env local --postgres-version 17 --json
```

Set its `endpoint` and matching `allowlist` to your explicitly approved
Docker-compatible daemon. The command ignores inherited `DOCKER_HOST` and
`DOCKER_CONTEXT`; it does not silently select your current context. Unix sockets
and TCP endpoints are supported. TCP hostnames are resolved to a pinned address
used for daemon calls and PostgreSQL port binding. That address must be reachable
from the client and bindable on the daemon host; TLS configuration, when used,
must authenticate that address. Unix daemons publish PostgreSQL on loopback only.

The example pins a verified Linux/arm64 PostgreSQL 17 image. Select an approved
`repository@sha256:digest` for your daemon's architecture and requested major;
mutable tags are refused. A cached image is used without pulling again. A missing
image is fetched only by that exact digest. The actual PostgreSQL major is
checked before restricted role/database provisioning or fixture execution.
Missing CLI/daemon access produces a bounded runtime-unavailable error before
allocation.

Every case creates a unique container with an ownership label and a one-GiB
tmpfs for PostgreSQL data. No host directory or persistent volume is mounted,
reused, or deleted. The factory handles the official images' data mount at
`/var/lib/postgresql/data` for 14–17 and `/var/lib/postgresql` for 18. See the
[official PostgreSQL image definition](https://github.com/docker-library/postgres/blob/master/18/bookworm/Dockerfile).
The provisioning password is random and remains private to the process and owned
daemon. Fixtures receive the same restricted login used by the existing-admin
factory, and cannot administer roles/databases.

The lease records the approved daemon identity, requested image digest, actual
image ID, unique name and acquired container ID. It exists before container
creation, so recovery can check the exact label and image even when creation was
acknowledged after the client stopped. Cleanup verifies the current daemon and
container identity, labels, image and temporary-storage configuration. It removes
by full verified container ID; a replacement with the same name/label is refused.
Removing that owned container removes its fresh role/database scope. Recovery
needs the recorded daemon, not an administrator secret or project target. Library
callers use `CleanupLease` for either mode; `CleanupAdminLease` remains available
and accepts only existing-admin leases.

### Lease and credential lifecycle

Creation writes a durable, owner-only lease before resource mutation and records
each role/database acquisition. The default directory is
`<user cache directory>/autosql/development`; `--lease-dir PATH` chooses another
private runtime directory. Existing unsafe permissions, links, corrupt records,
and concurrent lease locks are refused. No credential is stored in the lease.

Both modes return `lease_path`, `run_id`, `case_id`, `state`, `postgres_major`,
`deadline`, resources, and a `credential_ref` pointing to a mode-0600
`connection.url` file. That file contains the generated restricted login URL.
Use the reference with tools that support secret references; do not copy its
contents into project files or reports. Human output contains the same recovery
path/reference, never the URL. Interactive create leaves the allocation ready
for explicit cleanup. Library users call `Close`; `Detach` deliberately leaves
an interactive allocation for later `CleanupLease`.

Startup defaults to two minutes, case execution to five minutes, and each
cleanup action to ten seconds. Positive factory overrides are supported. The
library enforces the lease deadline during fixture execution. The generated
password also expires at that deadline; explicit cleanup is still required.
Cancellation uses fresh bounded cleanup contexts. A killed process can be
recovered with its exact lease path; there is no cleanup-all command.

Existing-admin cleanup checks the lease, current cluster/database identity, protected creation
registry, resource OIDs, ownership markers, and restricted role attributes before
dropping anything. A name prefix never authorizes deletion. Changed ownership,
corrupt leases, and redirected administrator references refuse deletion and
retain recovery metadata. An interrupted `CREATE DATABASE` without an
acknowledged ownership marker may need operator investigation; the command will
report the residual rather than guess ownership. It never repairs a foreign
marker automatically.

A successful cleanup removes the owned database/role or container and private credential,
retaining a nonsecret `cleaned` lease as evidence. Repeating cleanup checks that
the recorded resources are still absent. On failure, JSON `ok: false` preserves
available lease state, resource identities and bounded cleanup categories in
`data`, plus recovery guidance. No backend error text or administrator URL is
included. Exit codes are 2 usage, 3 configuration, 4 secret resolution,
6 connection, 7 execution/cleanup failure, 8 ownership/lease conflict, 124 timeout,
and 130 cancellation. Failure to write create output cleans the allocation and
returns 1. An existing primary failure remains authoritative when cleanup also
fails.

### Verification

Unit tests remain hermetic. Live tests require an explicitly disposable
PostgreSQL administrator URL in `AUTOSQL_DBTEST_ADMIN_DSN`:

```sh
go test -race ./pkg/dbtest ./internal/cli ./examples/project-testing
go test -race ./pkg/dbtest -run '^TestAdminFactoryLive' -count=1 -v
go test -race ./internal/cli -run '^TestDatabaseDevLive' -count=1 -v
```

Live coverage includes restricted-role execution, protected aliases and version
refusals before mutation, seven partial-setup/cancellation boundaries, ten
concurrent same-named cases, a force-killed child process, changed ownership,
recreated database names, foreign credential files, corrupt leases, redirected
administrator references, query deadlines, a non-superuser provisioner, and CLI output/recovery failures. The CLI live test
loads the public HCL example with only its endpoint and actual major adapted to
the disposable server.

Both factory modes, lifecycle commands, file-based test commands and the public
schema/upgrade/regression examples have passed on PostgreSQL 14–18 using
macOS/arm64 and Linux/arm64 clients. Suite coverage includes parallel isolation,
seeded upgrades, input-change refusal, timeouts, multi-case cancellation, combined
assertion/cleanup failures, filters, and report-write failures. Container coverage includes
partial creation, eight cancellation boundaries, timeout, ten simultaneous
cases, killed-process recovery, copied-label replacement refusal, daemon identity
mismatch, cached images, TCP aliases, and actual major mismatch. These are local
candidate results, not released-binary qualification.

Reproduce the matrix with an explicitly approved daemon and five ascending
`major repository@sha256:digest` lines in `images.tsv`:

```sh
AUTOSQL_DBTEST_DOCKER_ENDPOINT=unix:///absolute/docker.sock \
AUTOSQL_DBTEST_MATRIX_DIR=/absolute/results \
  scripts/test-dbtest-postgres-matrix.sh images.tsv
```

The script compiles each test binary once, records its SHA-256 and Go/platform
identity, runs both factory modes plus language, runner, CLI and public example
tests for each major, and checks that
administration databases/roles are gone. Each factory test also verifies its
recorded cleanup. Full case logs and the selected image digests stay in the
results directory. The isolated Linux client was tested with Go 1.25; macOS used
Go 1.26. Unit permission tests also pass under an ordinary Linux user; container
root bypasses chmod-based write denial and is not a valid context for that one
unit check. No amd64, remote-daemon or remote GitHub CI qualification is claimed
by this local run.
