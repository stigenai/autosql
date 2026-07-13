# Portable CI/CD integrations

The checked-in `Dockerfile.autosql`, GitHub Actions workflow, and GitLab
pipeline use digest-pinned images. The review job has read-only permissions and
is safe for untrusted pull requests. Only the protected `main` deployment job
receives an artifact reference; credentials are supplied at runtime through
`env://` and never baked into an image or cache.

Every image must carry a signature, SBOM, vulnerability-scan result, and a
reproducible build record. `pkg/integration` rejects mutable tags and verifies
that a cache record points at the requested digest. The generic container
entrypoint is the regular `autosql` CLI, so shell runners and native actions
have identical behavior.
