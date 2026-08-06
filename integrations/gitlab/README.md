# AutoSQL GitLab CI/CD component

This GitLab CI/CD Catalog component verifies or deploys an immutable AutoSQL
contract. Callers must provide an exact `sha256:` container index digest; the
component never follows an image tag.

```yaml
include:
  - component: gitlab.com/stigenai/autosql/autosql@0.1.32
    inputs:
      stage: review
      mode: verify
      contract: .autosql/review-contract.json
      contract_digest: sha256:...
      image_digest: sha256:...
```

Use `mode: run` only in a protected deployment environment with the platform
OIDC token and AutoSQL apply configuration available. Artifact, approval,
policy, target, contract, and image bindings remain enforced by AutoSQL.
