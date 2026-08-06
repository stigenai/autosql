# Releasing AutoSQL

Pushing a semantic version tag (`vX.Y.Z`) starts the release workflow.

The workflow builds and attaches these Linux CLI archives to the GitHub release:

- Linux AMD64 and ARM64

Each archive contains a version-stamped `autosql` binary and has a matching
SHA-256 file. The workflow also publishes `ghcr.io/stigenai/autosql` for
Linux AMD64 and ARM64 with the release tag, commit tag, and `latest` tag. The
images reuse the native release binaries rather than recompiling under
emulation, and include BuildKit provenance and SBOM attestations.

To cut a release after merging to `main`:

```sh
git checkout main
git pull --ff-only origin main
git tag -a vX.Y.Z -m "AutoSQL vX.Y.Z"
git push origin vX.Y.Z
```

Release notes should describe user-visible features, compatibility or upgrade
requirements, and the validation completed for the tagged commit.

## Optional native publishers

GitHub Release, the multi-architecture AutoSQL container, and the signed Helm
OCI chart are the default release path. They require `GHCR_TOKEN`; GitHub
Release uses the workflow's `GITHUB_TOKEN`.

Other public catalogs are opt-in. Set a repository variable to exactly `true`
only after its destination and protected secrets are ready:

| Repository variable | Optional publication |
|---|---|
| `PUBLISH_TERRAFORM_PROVIDER` | Terraform/OpenTofu provider repository and signed checksums |
| `PUBLISH_CIRCLECI` | CircleCI orb |
| `PUBLISH_GITLAB` | GitLab CI/CD Catalog component |
| `PUBLISH_AZURE_DEVOPS` | Azure DevOps Marketplace extension |
| `PUBLISH_BITBUCKET` | Bitbucket Pipe repository |

Unset variables and variables set to `false` skip that publisher. An enabled
publisher remains fail-closed: preflight stops before building or publishing
when any credential for that target is missing. The required secrets for each
target are listed in [Native CI/CD integrations](ci-cd-integrations.md#native-publication).
