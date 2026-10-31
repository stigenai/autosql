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

`schema.hcl`, `schema.sql`, and the native AutoSQL `schema.json` describe the
same desired schema. Each can be selected explicitly without resolving the
project target:

```sh
autosql schema load --config examples/project-testing/autosql.hcl --env local --source json:schema.json --json
autosql plan --config examples/project-testing/autosql.hcl --env local --from sql:current.sql --to sql:schema.sql --json
```

The qualification driver checks that all three formats produce the same target
fingerprint. A JSON project configuration and a native JSON schema are different
file contracts; `autosql.json` is the configuration, and `schema.json` is a source.

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

## Packaged binary qualification

From a clean checkout at the published version tag, use
[`scripts/test-project-workflow.sh`](../../scripts/test-project-workflow.sh).
It needs Go 1.25 or later, a C compiler, Git, jq, Docker CLI, a native packaged
binary, an approved daemon and a disposable development administrator server.
It runs the binary as a separate process; Go only drives the checks and the
separate real-signature integration scenario.

Set these inputs through your development environment or secret provider:

- `AUTOSQL_DBTEST_DOCKER_ENDPOINT`: explicitly approved Docker endpoint.
- `AUTOSQL_DBTEST_POSTGRES_IMAGE`: matching PostgreSQL image pinned by digest.
- `AUTOSQL_DBTEST_POSTGRES_MAJOR`: one major from 14 through 18.
- `AUTOSQL_DBTEST_ADMIN_DSN`: approved disposable development administrator URL.

Download the selected published binary and checksum outside the clean checkout,
verify the archive, then supply the binary's SHA-256 and exact version:

```sh
version="$AUTOSQL_RELEASE_VERSION"
name="autosql-${version}-darwin-arm64"
assets="$(mktemp -d)"
gh release download "$version" --repo stigenai/autosql \
  --pattern "${name}.tar.gz" --pattern "${name}.sha256" --dir "$assets"
(cd "$assets" && shasum -a 256 --check "${name}.sha256" && tar -xzf "${name}.tar.gz")
expected="$(shasum -a 256 "$assets/$name" | cut -d ' ' -f 1)"
scripts/test-project-workflow.sh "$assets/$name" "$expected" "$version" "$assets/evidence"
```

Use `linux-arm64` or `linux-amd64` for the corresponding native Linux binary.
The driver refuses an unsupported or emulated platform, mismatched binary
checksum/version, dirty checkout, wrong release tag, missing required case or
skipped required case. The evidence directory must be new. It validates local,
CI and production configuration without resolving those target credentials;
only the selected development factories can allocate resources.

Each tuple executes 16 cases through both factories, including schema and upgrade
success at parallelism 1 and 2, the intentional regression, a database-error
redaction probe, report-write refusal and human output. It also refuses empty,
all-skipped, production and changed-migration selections before case execution.
JSON and JUnit results are compared, and the JSON cases' released resources are
checked independently through catalog queries or Docker inventory. The suite
tests separately qualify ten concurrent same-named cases and cancellation.
PostgreSQL 17 additionally runs the existing signed generation/apply integration
and changed-input refusal with real ephemeral keys. That is separate integration
evidence; the file-based fixture runner does not grant production authority.

Keep each platform/major tuple's evidence. Compare a macOS run with a Linux run:

```sh
scripts/compare-project-workflow.sh /absolute/macos-evidence /absolute/linux-evidence
```

Comparison requires successful runs, identical required case semantics, matching
reference-only configuration identities and identical shared input bytes.
Runtime factory URLs and image digests differ across tuples, so their complete
configuration and test-input digests stay in the raw reports instead of being
misrepresented as equal. Timing, run IDs and resource names are excluded only
from the separate semantic comparison files.

The manual **released project workflow** GitHub Actions workflow accepts a
published version and runs both factories on Linux amd64/arm64 and PostgreSQL
14–18. macOS arm64 runs use the same driver locally with an approved Docker
daemon; hosted macOS execution is not claimed. All tuples and the separate
signature evidence are required for the bounded release gate.

During development, `--candidate` permits an uncommitted checkout and labels its
evidence `local-candidate`. It also requires
`AUTOSQL_PROJECT_TEST_SOURCE_MANIFEST_SHA256` for the source snapshot used to
build that binary. Candidate results do not satisfy the published-version or
clean-checkout gates. v0.1.53 predates these commands and has no macOS asset.
