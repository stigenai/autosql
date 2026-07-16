# Kubernetes operator contract

The operator is built with controller-runtime 0.23 and Kubernetes client
libraries 0.35, and is certified by envtest against Kubernetes 1.35.x.

`autosql operator --leader-election` starts the controller-runtime adapter in
the published AutoSQL image. `deploy/operator/crd.yaml` declares
`AutoSQLSchema` resources. A resource may
be declarative or versioned and may select exactly one inline, Secret, ConfigMap,
URL, or immutable registry-digest source. Database URLs are always Secret key
references. The controller writes only non-secret execution metadata to
status: conditions, generation, retry count, artifact/plan digests, target
identity, operation and execution IDs, pending recovery step, and recovery
guidance. It never copies secret values.

Fresh database provisioning uses the same `bootstrapAuthority` contract as
the PostgreSQL library preflight. Each of database creation, role creation,
extension setup, schema-object creation, grant setup, and final ownership
handoff is assigned to one named identity with the exact required capability.
Authentication may use the current session, an external Secret reference, AWS
IRSA, or OIDC. IRSA and OIDC carry no credential reference; short-lived values
are resolved only by the runtime provider. The contract, plan, status, logs,
and fingerprints therefore contain identity subjects and reference metadata,
never passwords, tokens, or resolved connection URLs. `createDatabase: true`
is rejected at admission and by the reconciliation core without this contract.

The live PostgreSQL 14–18 gate drives a `DeclarativeSchema` reconciliation
through `databaseTarget`, `maintenanceDatabaseURL`, and `bootstrapAuthority`
into the same whole-database planner/executor used by the CLI. The operator
persists only plan/execution IDs, applied-step counts, pending step IDs, and
recovery guidance in status; resolved maintenance credentials and routine SQL
remain transient.

The reconciliation core in `pkg/operator` persists an idempotency key and
requires a leader lease before applying. Conditions move through Planning,
ApprovalRequired (when configured), Applying, Ready, or Failed. Persisting the
applied key means a restart or leader change cannot duplicate a successful
apply; retries are safe because the same key is reused.

## CRD validation and CEL

The `v1alpha1` CRD now uses a structural schema plus CEL validation to reject
invalid objects before they reach the controller:

- exactly one of `inline`, `secretRef`, `configMapRef`, `url`, or
  `registryDigest` must be selected;
- registry and artifact digests must be lowercase SHA-256 digests;
- registry sources must name the same digest as `artifactDigest`;
- generations must be positive;
- Secret/ConfigMap names and keys must be valid Kubernetes references;
- fresh database creation must declare a bootstrap authority contract, whose
  credential reference is present only for `secret_reference` authentication;
- status conditions have typed condition names, statuses, timestamps, and
  deduplicating list semantics; and
- plan, target, operation, and recovery fields have explicit status types.

Useful next CEL additions should remain deterministic and admission-local:

- require `artifactDigest` for `VersionedMigration` resources;
- require `requireApproval: true` for production-labeled namespaces;
- enforce an approved registry hostname or digest-only sources;
- reject mutable URL sources unless an explicit update policy is set; and
- enforce bounded retry or generation policies.

CEL cannot safely validate Secret contents, PostgreSQL capabilities, plan
semantics, signatures, drift, or live approval state. Those checks belong in
the controller's guarded planning and execution path rather than admission.

Declarative and versioned resources are applied through the production
signed-artifact service. `artifactDigest` is required for both kinds; source
fields describe the desired artifact provenance, while the immutable artifact
is the only mutation input. For inline, Secret, and ConfigMap declarative
sources, the controller parses and plans the desired schema against the
resolved PostgreSQL target and refuses to apply when that plan digest differs
from the approved artifact.
Mount the immutable artifact directory and set
`AUTOSQL_OPERATOR_ARTIFACT_DIR`; the operator then delegates to the same
`ProductionServicesForURL` verification and PostgreSQL executor used by the
CLI, with the database URL resolved transiently from the CR's Secret.
Reconciliation records are written atomically to
`AUTOSQL_OPERATOR_STATE_FILE` (default `/var/lib/autosql/operator/state.json`).
Successful generations are also checkpointed in the CR status. A replacement
pod rehydrates its local store from the observed generation, applied digest,
and recovery state, so the sample `emptyDir` does not cause a successful apply
to run twice. A persistent volume may still be used when retaining the local
audit cache across pod replacement is operationally useful.

The controller integration test starts the real controller-runtime manager and
watches a real envtest API server. It can be run locally with:

```sh
export KUBEBUILDER_ASSETS="$(setup-envtest use -p path 1.35.x)"
AUTOSQL_OPERATOR_ENVTEST=1 go test ./internal/operatorcontroller \\
  -run TestEnvtestReconcilesCRAndWritesStatus -count=1
```

The declarative source-to-plan boundary can also be checked against PostgreSQL:

```sh
AUTOSQL_OPERATOR_PG_URL='postgres://postgres:postgres@localhost/autosql?sslmode=disable' \\
  go test ./internal/operatorcontroller \\
  -run TestDeclarativePlanVerificationAgainstPostgres -count=1
```
