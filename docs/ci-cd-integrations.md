# Native CI/CD integrations

Every first-party adapter calls one shared entrypoint:

```text
autosql integration verify|run --contract CONTRACT.json \
  --contract-digest sha256:... --json
```

It verifies the contract, execution image, artifact bytes, approval bytes,
policy, target snapshot, and deployment OIDC requirement before the apply
service sees a database reference. `verify` is for untrusted reviews; `run` is
for protected environments and invokes the signed-artifact apply path.

| Platform | Native package | Deployment boundary |
|---|---|---|
| GitHub Actions | `integrations/github/action.yml` plus reusable workflow | protected environment + OIDC |
| GitLab | CI/CD Catalog component under `templates/autosql` | ID token with AutoSQL audience |
| CircleCI | `stigenai/autosql` orb | restricted context/OIDC |
| Bitbucket | AutoSQL Pipe | deployment environment + step OIDC |
| Azure DevOps | `AutoSQL@0` extension task | protected environment/workload federation |

Release assets contain checksummed bundles. Examples live in `deploy/ci`.
Images are multi-architecture and digest-pinned. Credential URLs, remote
material, stale digests, missing approval evidence, and deploy contracts
without OIDC fail before execution. Output uses `autosql.output/v1` and normal
AutoSQL redaction.

GitHub callers provide both an exact release tag and the independently
reviewed SHA-256 of the runner archive. The reusable workflow checks the caller
repository and the matching AutoSQL release into separate directories, so a
caller cannot replace the first-party action implementation:

```yaml
- uses: stigenai/autosql@v0.1.32
  with:
    mode: verify
    contract: .autosql/review-contract.json
    contract-digest: sha256:...
    version: v0.1.32
    binary-sha256: 0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef
```

The GitLab Catalog component requires the immutable container index digest,
not a tag:

```yaml
include:
  - component: gitlab.example/stigenai/autosql/autosql@0.1.32
    inputs:
      mode: verify
      contract: .autosql/review-contract.json
      contract_digest: sha256:...
      image_digest: sha256:...
```

For passwordless database access, authenticate with the cloud provider's OIDC
login step and put a non-secret `WorkloadIdentity` binding in
`AUTOSQL_APPLY_CONFIG`. AWS IRSA/RDS IAM, GCP Workload Identity/Cloud SQL IAM,
and Azure workload identity/PostgreSQL Entra use the same short-lived token
runtime as the Kubernetes operator.

Major integration versions follow AutoSQL majors. Deprecated inputs remain
for at least two minor releases and are announced in release notes.

## Native publication

Tag releases run a fail-closed preflight before publishing anything. GitHub
Release, GHCR images, and the Helm OCI chart are enabled by default and require
`GHCR_TOKEN`. Other catalogs are disabled until their repository variable is
set to exactly `true`. Configure the matching protected GitHub Actions secrets
before enabling each target:

- `PUBLISH_TERRAFORM_PROVIDER`: `TERRAFORM_PROVIDER_GITHUB_TOKEN` for the public
  `stigenai/terraform-provider-autosql` repository;
- `PUBLISH_TERRAFORM_PROVIDER`: `TERRAFORM_REGISTRY_GPG_PRIVATE_KEY` and
  `TERRAFORM_REGISTRY_GPG_PASSPHRASE` for provider and integration checksums;
- `PUBLISH_CIRCLECI`: `CIRCLECI_CLI_TOKEN` for the `stigenai/autosql` orb;
- `PUBLISH_GITLAB`: `GITLAB_CATALOG_TOKEN`, `GITLAB_CATALOG_REPOSITORY_URL`,
  `GITLAB_CATALOG_PROJECT_ID`, and `GITLAB_API_URL` for the catalog project;
- `PUBLISH_BITBUCKET`: `BITBUCKET_PIPE_TOKEN`, `BITBUCKET_PIPE_USERNAME`, and
  `BITBUCKET_PIPE_REPOSITORY_URL` for the tagged Pipe repository;
- `PUBLISH_AZURE_DEVOPS`: `AZURE_DEVOPS_EXT_PAT` and `AZURE_DEVOPS_PUBLISHER` for the public
  Marketplace extension;

The GitLab project must be marked as a CI/CD Catalog resource. The Terraform,
GitLab, and Bitbucket repositories must already exist, be public, and protect
their default branches. Tokens should be environment-scoped, minimum-role
publisher credentials with rotation and revocation owned by the organization.

The gate builds all native packages first, publishes each enabled catalog, and
only then creates the main GitHub release. Terraform signing is skipped with
Terraform publication. Missing configuration for an enabled target fails
during preflight rather than leaving a partially published release.
