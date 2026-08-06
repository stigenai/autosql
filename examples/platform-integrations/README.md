# Platform integrations

The Terraform example uses only opaque database and approval references. The
Kubernetes examples select each supported cloud workload identity and contain
no static database password. Replace sample identities, hosts, and digests with
reviewed production values.

CI examples are in `deploy/ci`; Argo CD, Flux, and Crossplane resources are in
`deploy/gitops`; Helm values and CRDs are in `deploy/helm/autosql-operator`.
