# CI/CD and GitOps ecosystem

`pkg/integrations/gitops` provides one versioned contract for CircleCI,
Bitbucket Pipelines, Azure DevOps, Argo CD, GitHub, and GitLab. Every rendered
integration binds an artifact digest, policy digest, target snapshot digest,
and approval reference. Deploy contracts require OIDC; review contracts remain
credential-free.

Images must carry immutable digests, signatures, SBOM and scan evidence through
the existing integration contract. Artifact and secret references remain
opaque `env://`/`file://` references. A platform status check carries the
contract digest, so stale or forged callbacks cannot mark a different plan as
passed. Retry policy is explicit and machine-readable.

Argo CD output carries the same bindings as application metadata and leaves
automated sync disabled; approval remains an AutoSQL gate. Platform adapters
can map the contract to native actions, components, orbs, pipes, tasks, and
sync hooks without embedding long-lived credentials.
