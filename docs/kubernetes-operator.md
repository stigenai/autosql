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

Declarative inline, Secret, and ConfigMap content may set `source.format` to
`sql` or `hcl`. Setting it is recommended and makes parser selection part of
the generation fingerprint. For compatibility, resources that omit `format`
are accepted only when exactly one of the SQL and HCL parsers recognizes the
content. Schema-only HCL therefore works without a database block, while SQL
comments and string literals containing words such as `database` remain SQL.
Ambiguous content, an explicit format mismatch, and formats on URL or registry
sources fail before database or authorization credentials are resolved.

```yaml
source:
  format: hcl
  configMapRef:
    name: desired-schema
    key: schema.hcl
```

Whole-database resources should also set `spec.postgresVersion` (14 through
18) so versioned PostgreSQL semantics such as membership `INHERIT`/`SET`
options are bound into the reviewed plan. `spec.concurrentIndexes` defaults to
true and makes standalone index creation use the resumable online phase.

Routine and extension bootstrap authorization uses the same signed,
plan-bound workflow as CLI and HCL execution. A CR may declare only runtime
references and trusted public identity policy:

```yaml
spec:
  bootstrapAuthorization:
    manifestSecretRef: {name: bootstrap-authorization, key: manifest.json}
    publicKeySecretRef: {name: bootstrap-authorization, key: public-key}
    issuer: security
    signer: dba-reviewers
    purpose: bootstrap-authorization
```

The namespaced Secrets are read immediately before guarded planning. Resolved
manifest and public-key bytes are transient and excluded from records, status,
events, and errors. The structural CRD has no private-key, password, token, or
credential field; the required admission policy rejects unmodeled fields and
the controller's strict decoder repeats that check at its direct boundary. The
controller prepares the complete inventory, verifies the signed manifest, and
uses the same opaque authorized-plan bridge as the CLI before mutation.
`BootstrapAuthorization` conditions and matching events distinguish `Missing`,
`Invalid`, `Stale`, and `Accepted`. Expired or plan/source-mismatched artifacts
are stale; malformed, untrusted, or incorrectly signed artifacts are invalid;
and absent Secret objects or keys are missing.

Install both `deploy/operator/crd.yaml` and
`deploy/operator/admission.yaml`. The latter is a Kubernetes 1.35
`ValidatingAdmissionPolicy` plus binding. The CRD preserves unknown fields only
inside the authorization object long enough for that policy to deny them;
private keys, inline manifest/public-key bytes, namespace overrides, misspelled
fields, and any other unmodeled key are rejected on CREATE and UPDATE instead
of being silently pruned. The policy is a required part of the operator
installation, not an optional client-side validation convention.

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

For a whole-database resource, production reconciliation verifies the signed
migration artifact without applying it through the generic target connection,
then executes the exact manifest-authorized `bootstrap.Plan` through the
resolved maintenance URL. The authorized plan is never discarded or rebuilt.
This is the path that creates a fresh managed target, performs extension
readiness and runtime-capability checks before target creation, and records
bootstrap checkpoints in status.

The reconciliation core in `pkg/operator` persists an idempotency key and
requires a leader lease before applying. Conditions move through Planning,
ApprovalRequired (when configured), Applying, Ready, or Failed, with an
independent BootstrapAuthorization condition. Persisting the
applied key means a restart or leader change cannot duplicate a successful
apply; retries are safe because the same key is reused.

The applied fingerprint covers Kubernetes metadata generation, source and
database target declarations, authorization references and issuer policy,
referenced object resource versions, SHA-256 bindings for source/manifest/public
key content, and the exact source and whole-plan digests returned by guarded
planning. An empty local store never trusts status alone: after pod replacement
the operator resolves and verifies again. Secret and source ConfigMap watches
immediately enqueue dependent resources after rotation or deletion, and
successful authorization is requeued before its expiry. No resolved value is
stored in the fingerprint.

## CRD validation and CEL

The `v1alpha1` CRD now uses a structural schema plus CEL validation to reject
invalid objects before they reach the controller:

- exactly one of `inline`, `secretRef`, `configMapRef`, `url`, or
  `registryDigest` must be selected;
- registry and artifact digests must be lowercase SHA-256 digests;
- registry sources must name the same digest as `artifactDigest`;
- `source.format`, when present, is `sql` or `hcl` and applies only to inline,
  Secret, or ConfigMap declarative content;
- generations must be positive;
- Secret/ConfigMap names and keys must be valid Kubernetes references;
- bootstrap authorization must contain complete manifest/public-key Secret
  references and issuer/signer/purpose policy; the admission policy denies
  every unknown, inline, cross-namespace, or private-key field;
- fresh database creation must declare a bootstrap authority contract, whose
  credential reference is present only for `secret_reference` authentication;
- status conditions have typed condition names, statuses, timestamps, and
  deduplicating list semantics; and
- status records a non-secret applied fingerprint and authorization expiry so
  restart, Secret rotation, and expiry cannot reuse stale acceptance; and
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
Whole-database bootstrap always enforces the production no-edits policy,
regardless of a more permissive `NoEdits` value in the apply configuration.
The release must carry a signed `generated` origin bound to the exact approved
plan and trusted generator key; unattested `artifact.New` releases, edited
origins, missing provenance or generator signatures, and configuration-based
bypass attempts are rejected before the maintenance executor can run. The
operator-provided runtime database URL is authoritative, so verification does
not resolve a second configured database credential when that override is
present. Reconciliation performs this release check before reading database,
maintenance-database, desired-source, or bootstrap-authorization Secrets. It
then carries an opaque verified-artifact token through reference resolution
and execution; the bootstrap path does not reread the artifact file, closing
the artifact-swap window between verification and mutation.
Reconciliation records are written atomically to
`AUTOSQL_OPERATOR_STATE_FILE` (default `/var/lib/autosql/operator/state.json`).
Successful generations are also checkpointed in the CR status for observation
and audit, but status is never treated as authorization or used to reconstruct
an empty local store. A replacement pod resolves every runtime reference and
reverifies the source, artifact, authorization manifest, and exact target before
it can apply. The sample `emptyDir` therefore favors safe re-verification over
status-based suppression; use a persistent volume when retaining the local
idempotency and audit state across pod replacement is required.

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
