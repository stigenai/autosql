# Releasing AutoSQL

Pushing a semantic version tag (`vX.Y.Z`) starts the release workflow.

The workflow builds and attaches these Linux CLI archives to the GitHub release:

- Linux AMD64 and ARM64

Each archive contains a version-stamped `autosql` binary and has a matching
SHA-256 file. The workflow also publishes `ghcr.io/stigenai/autosql` for
Linux AMD64 and ARM64 with the release tag, commit tag, and `latest` tag. The
images include BuildKit provenance and SBOM attestations.

To cut a release after merging to `main`:

```sh
git checkout main
git pull --ff-only origin main
git tag -a vX.Y.Z -m "AutoSQL vX.Y.Z"
git push origin vX.Y.Z
```

Release notes should describe user-visible features, compatibility or upgrade
requirements, and the validation completed for the tagged commit.
