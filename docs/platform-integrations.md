# Platform integration support and upgrades

One gated release publishes the CLI/container, Terraform/OpenTofu provider,
Helm chart, CI packages, and GitOps examples. Shared contracts, Terraform
protocol, Helm, operator, workload-identity, checksum, SBOM, and signature
checks must pass first.

## Supported versions

- PostgreSQL 14–18.
- Kubernetes 1.30–1.35, with conformance pinned to 1.35.
- Linux amd64/arm64 for the image and operator.
- Linux, macOS, and Windows amd64/arm64 for the provider.
- Helm 3.14+, Argo CD 2.11+, Flux `OCIRepository` v1 and `HelmRelease` v2,
  and Crossplane pipeline compositions.

The chart is published at
`oci://ghcr.io/stigenai/charts/autosql-operator`, signed keylessly, and pins the
controller image by digest. CRDs upgrade with `CreateReplace`; uninstall keeps
CRDs, database resources, and persistent claims. Argo CD, Flux, and Crossplane
all reconcile the same `AutoSQLSchema`, so sync cannot bypass approval.

The release gate tests fresh installation and the previous-minor upgrade path.
Platform rollback never rolls back PostgreSQL implicitly; database rollback
requires its own signed and approved artifact.

The same gate also runs the current controller image on Kubernetes 1.35 and
requires all three declarative adapters to reconcile through their real
controllers: Flux must report its `OCIRepository` and `HelmRelease` Ready,
Argo CD must report the Application Synced and Healthy, and Crossplane must
create an `AutoSQLSchema` and propagate its status.

Apply all four Crossplane resources in `deploy/gitops/crossplane`: the XRD,
Composition, pinned function, and aggregate RBAC role. The role grants
Crossplane access only to `AutoSQLSchema` objects and their subresources; it
does not grant access to Secrets or database credentials. Crossplane-owned
conditions remain in `status.conditions`, while the composed operator's
conditions are exposed separately as `status.autosqlConditions`. Applied
digest, applied fingerprint, and retry count are also propagated.

Set `spec.suspend: true` on either an `AutoSQLSchema` or the Crossplane
`XAutoSQLSchema` to pause reconciliation before source, database Secret, or
workload-identity resolution. The operator reports Ready with reason
`Suspended`; clearing the field resumes normal reconciliation.

## Workload identity

`spec.databaseIdentity` replaces `spec.databaseURL` for passwordless access:

- `aws_rds_iam`: AWS default credential chain, including IRSA;
- `gcp_cloud_sql_iam`: Application Default Credentials and Workload Identity;
- `azure_postgresql_entra`: `DefaultAzureCredential` and Azure workload identity.

Provider, host, port, database, user, TLS mode, audience, region, and subject
are fingerprinted. Tokens are runtime-only, refreshed before expiry, and
redacted. Expired, unusually long-lived, wrong-audience, or revoked responses
fail closed. Workload identity permits only `require`, `verify-ca`, and
`verify-full` TLS modes.

The same binding can replace `DatabaseURL` in `AUTOSQL_APPLY_CONFIG` for CI:

```json
{
  "WorkloadIdentity": {
    "provider": "aws_rds_iam",
    "host": "orders.cluster-example.us-east-1.rds.amazonaws.com",
    "port": 5432,
    "user": "autosql",
    "database": "orders",
    "tlsMode": "verify-full",
    "region": "us-east-1",
    "audience": "sts.amazonaws.com",
    "subject": "arn:aws:iam::123456789012:role/autosql-production"
  }
}
```

Configure the CI platform's official AWS, GCP, or Azure OIDC login before the
AutoSQL deploy step. AutoSQL then uses the provider's default workload
credential chain; it never asks CI to export a static database password.
