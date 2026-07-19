# GitOps operator artifacts

`autosql operator artifact publish` is the supported non-interactive path from
committed HCL to the immutable artifact required by `AutoSQLSchema`. It performs
the complete production ceremony in one process: load and normalize HCL,
inspect the current target (or model an empty target with `--bootstrap`), plan,
replay against an isolated development database, run safety and policy gates,
issue a short-lived plan-bound CI approval, generator-attest, release-sign, and
atomically publish an operator bundle.

## One-time keys

Create three role-separated Ed25519 keys. The command writes raw base64 in the
format AutoSQL accepts, uses exclusive file creation, writes private material
with mode `0600`, and never prints key bytes:

```sh
install -d -m 700 .secrets
autosql operator key generate \
  --private-output .secrets/operator-generator.key \
  --public-output .secrets/operator-generator.pub
autosql operator key generate \
  --private-output .secrets/operator-release.key \
  --public-output .secrets/operator-release.pub
autosql operator key generate \
  --private-output .secrets/operator-ci-approval.key \
  --public-output .secrets/operator-ci-approval.pub
```

Put the three private values and both database URLs in the CI secret store.
They must reach AutoSQL only through `env://NAME` or `file://path` references.
The public files are safe to commit or distribute in a trust bundle.

The generator and release roles have distinct key IDs and purposes, which the
publisher enforces. AutoSQL permits the same Ed25519 bytes to back both roles
for constrained installations, but separate keys are strongly recommended:
generator compromise then cannot create a release, and release-key compromise
cannot fabricate generator provenance. The CI approval key is a third role and
must use a distinct key ID.

Rotate by creating a new key ID, setting an overlapping release trust window,
publishing new artifacts, deploying their generated apply configuration, and
only then removing the old key. `SigningNotBefore` and `SigningNotAfter` are the
release key's trust window. `ArtifactLifetime` is the shorter lifetime of one
artifact. `ApprovalTTL` is shorter again and controls how recently CI must have
authorized the exact guardrail bundle.

## Headless publish

Start from [the complete publish configuration](../examples/operator-gitops/publish-config.json)
and [policy](../examples/operator-gitops/policy.json). Values such as identity,
environment, schemas, key IDs, purposes, validity windows, paths, and policy are
trusted literals. `GenerationApprovalAuditPath` is a CI workspace path;
`ApprovalAuditPath` and `LifecycleAuditPath` are operator runtime paths. Every
`*URL` and `*PrivateKeyReference` value is a secret reference; never put the
resolved value in JSON.

```sh
export AUTOSQL_DEV_DATABASE_URL="$CI_AUTOSQL_DEV_DATABASE_URL"
export AUTOSQL_TARGET_DATABASE_URL="$CI_AUTOSQL_TARGET_DATABASE_URL"
export AUTOSQL_OPERATOR_GENERATOR_KEY="$CI_AUTOSQL_OPERATOR_GENERATOR_KEY"
export AUTOSQL_OPERATOR_RELEASE_KEY="$CI_AUTOSQL_OPERATOR_RELEASE_KEY"
export AUTOSQL_OPERATOR_APPROVAL_KEY="$CI_AUTOSQL_OPERATOR_APPROVAL_KEY"

autosql operator artifact publish \
  --file schema/cell.hcl \
  --config examples/operator-gitops/publish-config.json \
  --output-dir dist/cell \
  --source-revision "git:${GITHUB_SHA}" \
  --json
```

For a fresh database, add `--bootstrap`. Bootstrap HCL must contain exactly one
`database` block, and `DatabaseIdentity` must equal its database name. Existing
database transitions may also carry that block; it is treated as the database
contract and excluded from in-database SQL planning.

Bootstrap publish does not resolve or connect to `ProductionURL`. Simulation
and the approval guardrail both execute against disposable databases on
`DevelopmentURL`, prepared to match the declared target: external mode removes
the template-created `public` schema before replay. Owner roles referenced by
the database or schema HCL but not declared as managed `role` resources are
leased on the development cluster as `NOLOGIN` roles when missing. AutoSQL
coordinates concurrent leases, drops only roles it created, and drops them
only after every participating workspace has released its lease; existing
roles are never changed or removed. The development identity therefore needs `CREATEDB`
and, when a referenced owner is absent, `CREATEROLE` (a development superuser
also satisfies both). Auto-created roles are granted to the development
identity for the lease. An existing owner role is never modified, so a
non-superuser development identity must already be its member. These temporary
development roles do not alter the production bootstrap contract: required
external roles must still exist on the real target when its bootstrap is
applied.

The output directory is created atomically and contains:

```text
dist/cell/
├── release.json
├── apply-config.json
└── artifacts/
    └── sha256:<64 lowercase hex>.json
```

Archive the hash-chained file at `GenerationApprovalAuditPath` as a protected
CI artifact alongside the release. It is intentionally not placed inside the
operator bundle, because the operator has separate runtime approval and
lifecycle audit files.

`apply-config.json` is the minimal complete `AUTOSQL_APPLY_CONFIG`; do not
manually reconstruct it. It contains no private key or database credential. It
contains the release and generator public keys, exact plan/check/guardrail and
approval bindings, all three validation attestations, the policy inputs needed
to reproduce the guardrail bundle, the per-artifact schema scope, key status
and validity window, and literal audit/artifact paths. Point the operator at it:

```sh
export AUTOSQL_APPLY_CONFIG=/release/apply-config.json
export AUTOSQL_OPERATOR_ARTIFACT_DIR=/release/artifacts
```

### CI approval without a human click

The publishing configuration names a CI identity, its policy roles, and a
private approval-key reference. After AutoSQL computes the exact guardrail
bundle, it signs claims containing that digest, environment, CI identity,
approval time, and expiry. The regular approval gate verifies those claims and
durably appends its decision before generator or release signing. A proof for a
different bundle, environment, key ID, identity, or time window is rejected.
This replaces the circular `VerifiedApprovals` setup; there is no production
mode that disables approval freshness.

Use a protected CI environment or workload identity to authorize access to the
approval key. Keep `Author`, `Requester`, and `AutomationApprovalIdentity`
distinct. The checked-in policy should require the automation identity's role,
as the example requires `release-automation`.

## OCI and Flux distribution

The operator consumes a mounted directory; it deliberately does not pull OCI
content or accept registry credentials in a CR. OCI is a transport for the
three-file release bundle. Publish the exact directory with an immutable tag:

```sh
DIGEST="$(jq -r .artifact_digest dist/cell/release.json)"
TAG="${DIGEST/:/-}"
(
  cd dist/cell
  oras push "ghcr.io/acme/autosql-cell:${TAG}" \
    release.json:application/vnd.autosql.operator-release.v1+json \
    apply-config.json:application/vnd.autosql.apply-config.v1+json \
    "artifacts/${DIGEST}.json":application/vnd.autosql.artifact.v1+json
)
```

Use a Flux-managed Deployment with an init container (or an equivalent CSI or
volume-population controller) to pull that immutable OCI tag into a shared
read-only volume before the operator starts. The init container output must
preserve `artifacts/sha256:<hex>.json`; the operator performs an exact digest
and signature check before resolving database Secrets. See
[the deployment patch](../examples/operator-gitops/operator-deployment-patch.yaml).

There are two different digest namespaces:

- `artifactDigest` is the AutoSQL artifact's canonical SHA-256 digest and maps
  exactly to `${AUTOSQL_OPERATOR_ARTIFACT_DIR}/${artifactDigest}.json`.
- `source.registryDigest`, when selected, is an AutoSQL provenance selector and
  must equal `artifactDigest` by CRD policy. It is **not** the OCI manifest
  digest. `release.json.oci_tag` converts it to the portable `sha256-<hex>` tag.

For a declarative ConfigMap/Secret/inline HCL source, keep that source selector
and set only `artifactDigest`; the controller replans the resolved HCL and
requires its plan digest to match the mounted artifact. For an immutable
registry source, set both fields to the same AutoSQL artifact digest.

## Per-database conventions

Publish one artifact and trust entry per PostgreSQL database:

| HCL | `DatabaseIdentity` | `Environment` | `SourceRevision` | `Schemas` |
| --- | --- | --- | --- | --- |
| `schema/cell.hcl` | `cell` | `production` | `git:<full commit SHA>` | all schemas managed in cell |
| `schema/global.hcl` | `global` | `production` | `git:<full commit SHA>` | all schemas managed in global |
| `schema/auth.hcl` | `auth` | `production` | `git:<full commit SHA>` | all schemas managed in auth |

`DatabaseIdentity` is stable and case-sensitive. For whole-database bootstrap
it must be the exact database block name. `Environment` is a policy boundary,
not a cluster nickname; use the same value only when the same approval policy
applies. `SourceRevision` must be immutable—prefer `git:<40-character SHA>`,
never a branch, tag that can move, build number alone, or `latest`.

Set the CR's `postgresVersion` and `concurrentIndexes` to the values recorded in
`release.json`. They are plan inputs: changing either value changes rendered
SQL and must produce a new artifact. The checked-in example uses PostgreSQL 16
and concurrent indexes, matching the CRD's default for `concurrentIndexes`.
See [the complete `AutoSQLSchema` example](../examples/operator-gitops/autosqlschema.yaml).

The generated trust entry carries its own `Schemas`, so one apply configuration
can safely contain releases whose inspection scopes differ. When combining
cell/global/auth into one operator trust file, merge `TrustedMigrations` entries
only when release/generator keys, issuer, signer, key windows, author/requester,
PostgreSQL version, and approval policy are identical; otherwise deploy a
separate operator configuration for that trust domain.
