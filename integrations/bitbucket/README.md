# AutoSQL Bitbucket Pipe

The AutoSQL Pipe verifies or deploys immutable database artifacts through the
same contract used by every first-party integration. `pipe.yml` pins the Pipe
container by multi-architecture image digest.

```yaml
- pipe: stigenai/autosql-pipe:v0.1.32
  variables:
    MODE: verify
    CONTRACT: .autosql/review-contract.json
    CONTRACT_DIGEST: sha256:...
```

Use `MODE: run` only in a protected deployment step with OIDC enabled. Database
credentials remain short-lived runtime references and are never Pipe inputs.
