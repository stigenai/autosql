# Project configuration example

This example validates equivalent version-2 JSON and HCL projects with local,
CI, and production environments. It requires Go or a candidate `autosql` binary
that supports version 2. No database, container daemon, credentials, or network
registry are needed for validation. The `.invalid` production endpoint is an
illustrative protected identity, never a test target.

From the repository root:

```sh
go run ./cmd/autosql config validate --config examples/project-testing/autosql.hcl --env local --json
go run ./cmd/autosql config validate --config examples/project-testing/autosql.json --env ci/postgres --json
go run ./cmd/autosql config validate --config examples/project-testing/autosql.hcl --env production --json
go run ./cmd/autosql schema load --config examples/project-testing/autosql.hcl --env local --json
go run ./cmd/autosql plan --config examples/project-testing/autosql.hcl --env local --from sql:current.sql --json
go test ./examples/project-testing
```

Each successful validation reports `valid: true`, the exact environment name, and
effective values with field provenance. The HCL `environment_set "ci"` expands
the declared map into `ci/postgres`; the JSON file writes that environment
explicitly. Source and migration paths resolve relative to the project file.

Schema loading reports the composed schema, configuration identity, and source
byte records. The local plan compares `current.sql` with the desired HCL schema
and proposes the accounts table without resolving the target credential. Omitting
`--from` would request a live read of that selected target instead. The tests run
both project commands for local, CI, and production selections using local
current-state input; they never connect to the illustrative production endpoint.

The automated example also loads the local HCL schema and refuses an unknown
environment, a mistyped nested field, and an inline database credential. To see
the selection failure directly:

```sh
go run ./cmd/autosql config validate --config examples/project-testing/autosql.hcl --env missing --json
```

The CLI returns configuration exit code 3 with a located error. `go run` itself
returns its wrapper exit status; use a built binary when asserting CLI codes.

The `tests/` directory contains executable schema and seed-before-upgrade cases.
The migration snapshot includes versions 1 and 2, and the parser example verifies
its binding without connecting to a database. Configuration validation alone does
not prove that credentials or services exist. Do not add `--preflight` unless you
intend to resolve target and legacy development-database references.

After selecting an approved factory, run:

```sh
autosql schema test --config examples/project-testing/container.hcl --env local --parallel 2 --json --junit reports/schema.xml
autosql migrate test --config examples/project-testing/container.hcl --env local --filter 'tests/*.test.hcl::seed survives upgrade' --json --junit reports/upgrade.xml
```

Substitute `development.hcl` for the existing-admin factory. Report paths resolve
beside the project file. `--json=false` selects human output; `--junit=` disables
a configured report. `schema test --help` and `migrate test --help` describe the
flags. A missing filter match or all-skipped selection returns 5 before allocation;
`--env production` returns 3. These commands use the candidate binary. Released
binary qualification remains a separate acceptance gate.

See [project configuration](../../docs/project-configuration.md) for the field
contract, defaults, override rules, and compatibility boundaries.

## Interactive development database

`development.hcl` is an executable existing-admin factory example for a separately
approved PostgreSQL 17 development server at `127.0.0.1:55432/postgres`. Set
`AUTOSQL_EXAMPLE_LOCAL_ADMIN` through your secret provider to its administrator
URL, or edit the example's administrator reference and credential-free allowlist
to match your development server. Creation resolves that reference only; neither
the local target nor production target credentials are needed.

```sh
autosql database dev create --config examples/project-testing/development.hcl --env local --postgres-version 17 --json
```

The result contains a private credential-file reference for the restricted
fixture login and the exact lease path. After using the database, pass that path
to cleanup:

```sh
autosql database dev cleanup --lease /absolute/path/from/create/lease.json --json
```

Selecting `--env production` in the create command returns configuration exit
code 3 before resolving any credential or mutating a database. A wrong requested
major, unlisted endpoint, corrupt lease or foreign ownership marker also refuses
operation. The [database testing guide](../../docs/database-testing.md) describes
required privileges, ownership checks, cleanup recovery, and optional live tests
of this example. The same factory can run the project test commands above.

## Container development database

`container.hcl` provisions a fresh container through an explicitly approved
Docker-compatible daemon, then creates the restricted fixture login/database.
It pins a Linux/arm64 PostgreSQL 17 image and uses `unix:///var/run/docker.sock`;
replace the image digest and the endpoint/allowlist together to match your
approved daemon and architecture. A mutable image tag is not accepted. No
administrator or project-target credential is required.

```sh
autosql database dev create --config examples/project-testing/container.hcl --env local --postgres-version 17 --json
autosql database dev cleanup --lease /absolute/path/from/create/lease.json --json
```

Removing `--json` gives the human lifecycle output. Both forms return the exact
lease path and private restricted-credential reference. Production selection is
refused before any daemon call. A missing daemon, changed daemon identity,
replaced container, or mismatched image/major fails explicitly. PostgreSQL data
uses fresh temporary storage; only image bytes are cached. The
[factory guide](../../docs/database-testing.md#container-factories) covers endpoint
requirements and ownership recovery, and its matrix script verifies both modes
on PostgreSQL 14–18.

## Intentional regression

The container project's `regression` environment uses `failures/*.test.hcl`.
It deliberately expects two rows after seeding one and also fails its cleanup
statement. Run it only as an expected failure:

```sh
autosql migrate test --config examples/project-testing/container.hcl --env regression --json --junit reports/regression.xml
```

Expected exit: 5. JSON retains both the located assertion and cleanup failure;
JUnit contains a failure and an error for the same case. The owned database and
container still close. The normal `local` selection excludes this regression.
The live example test verifies schema success, upgrade success and this failure
through the same CLI entrypoint on a copied fixture.
