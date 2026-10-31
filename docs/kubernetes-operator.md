# Kubernetes operator contract

Install the supported bundle from the signed OCI Helm chart:

```sh
helm install autosql oci://ghcr.io/stigenai/charts/autosql-operator \
  --namespace autosql-system --create-namespace --version 0.1.0
```

The chart pins the controller by digest, includes the complete CRD, persistent
state and artifact volumes, least-privilege RBAC, admission policy, and
workload-identity service-account settings.

The default installation watches only its release namespace and receives a
namespaced Role; it cannot read Secrets from other namespaces. Set
`rbac.clusterWide=true` only when one controller must reconcile multiple
namespaces, and treat that as an explicit expansion of its secret-read trust
boundary.

The operator is built with controller-runtime 0.23 and Kubernetes client
libraries 0.35, and is certified by envtest against Kubernetes 1.35.x.

For the complete non-interactive HCL → signed artifact → OCI/Flux volume
workflow, including keys, CI approval freshness, generated
`AUTOSQL_APPLY_CONFIG`, and digest semantics, see
[GitOps operator artifacts](operator-gitops.md).

`autosql operator --leader-election` starts the controller-runtime adapter in
the published AutoSQL image. `deploy/operator/crd.yaml` declares
`AutoSQLSchema` resources. A resource may
be declarative or versioned and may select exactly one inline, Secret, ConfigMap,
URL, or immutable registry-digest source. Database access uses either a Secret
key reference or `databaseIdentity` for AWS RDS IAM, GCP Cloud SQL IAM, or
Azure PostgreSQL Entra. The controller writes only non-secret execution metadata to
status: conditions, generation, retry count, artifact/plan digests, target
identity, operation and execution IDs, pending recovery step, and recovery
guidance. It never copies secret values.

Populated databases can be adopted explicitly with
`spec.adoptionPolicy: IfEquivalent` on a controller-resolvable
`DeclarativeSchema` source. Adoption accepts only a signed desired-to-desired
no-op artifact (publish it with `autosql operator artifact publish --adopt`),
proves the locked live schema yields the same plan, and reports
`Ready/Adopted` without executing application SQL. Mismatches and bootstrap or
database-creation combinations are refused.

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

## State-store failure behavior

The operator's file store replaces its state file atomically before publishing
new records in memory. If serialization, directory creation, temporary-file
writing, or replacement fails, the last committed records stay intact; a later
successful save does not persist entries from an earlier failed save. Both file
and memory stores copy condition slices on save and load, so changing a returned
record cannot change the saved checkpoint.

The reconciler checks every required checkpoint write. A failed write before
apply prevents execution. A failed write after apply reports
`StatePersistenceFailed`, preserves the previous successful generation, digest,
fingerprint, and observation time, and never publishes Ready for that attempt.
Failure messages omit filesystem paths and backend error details.

The reconciliation core accepts a completion observer for ordinary declarative
resources. Before an uncached ordinary apply, a read-only start check examines
the managed state, all history states/attempts, and existing lifecycle audit.
An original-state match with no history and no execution event can continue
through normal apply authorization. A desired-state match, prior history, an
execution event, or ambiguity selects observation; refused or unavailable
evidence cannot fall through to apply. Suspension and required approval are
checked before either database path. Once a checkpoint records a possible
attempt, retry also requires an existing audit with valid pre-execution events;
a missing or emptied audit cannot reset that attempt to a fresh start. Known
uncertainty always enters completion verification and never the fresh-start path.
Proven completion must be saved before returning Ready/`CompletionRecovered`.
Its optional `completionObservedAt` records when completion was observed, not
when SQL originally finished. Checkpoints include the resource UID so a
replacement object cannot inherit an earlier object's execution key. Both CRD
copies accept the timestamp; apply CRD updates before upgrading the operator.

Recovered status publication refreshes the resource and its runtime references.
UID, generation, spec, approval annotation, or reference version/content changes
discard the old result. Conflicts retry publication and current authorization
checks only; they do not repeat observation or SQL. The controller requires an
authorization recheck before publishing recovered readiness.

The production controller now connects the observer and publication checks for
ordinary declarative resources. Expired releases enter observation only after
current authorization succeeds, before runtime references are resolved. A
missing local checkpoint with matching Kubernetes completion status also
selects observation. When both stores are empty, the read-only start check
rediscovers a completed target and requires full proof. Changing references for
an already-applied release re-observes the new target rather than executing the
release again. Recovery requires resolvable inline, Secret, ConfigMap, or
immutable registry input; mutable URL content is not fetched or assumed to match
the signed goal. Fresh URL-source apply behavior remains unchanged. The original
signed artifact snapshot is carried into apply;
current signature, expiry, policy, and approval checks still run before mutation.

New operator release bundles preserve the original automation approval proof
and its public verification key. Recovery checks the signed expiry, current
identity/roles, policy, safety settings, release key, and validation evidence.
It never substitutes the artifact expiry for the signed approval expiry.
Legacy bundles without the original proof are refused for recovery. Older
operator binaries reject the new apply-configuration fields, so upgrade the
controller before publishing bundles with this publisher. The current
publisher bounds approvals before artifact expiry and validation evidence at
artifact expiry, so its expired releases remain refused; publish a fresh
approved release when those windows have elapsed.

The observation path rechecks the source fingerprint and current authorization,
reads the exact database and audit evidence, then durably appends one
`completion_observed` event per resource UID, metadata generation, artifact, and
execution attempt. It does not fabricate the original execution completion.
An audit or checkpoint write failure prevents recovered readiness. A later
publication authorization failure clears previously recovered Ready status
while preserving historical successful fields.

The live scenario uses the real CLI publisher and generated signatures, applies
to owned disposable databases, injects a completion checkpoint failure, and
recovers without calling Apply again. It verifies unchanged database history,
one observation event across retries, and refusals for missing proof, revoked
keys, removed roles, changed source, missing audit, and expired published
validation evidence. It also clears both state stores, replaces the controller,
changes a Secret version, and retries conflicting status publications without
repeating database work. With `AUTOSQL_OPERATOR_ENVTEST=1`, those publications
use a disposable Kubernetes 1.35 API server and actual resource-version
conflicts; without it, the fixture uses a fake API client and injected conflicts.
Prerequisites are a local Go toolchain and
`AUTOSQL_TEST_POSTGRES_URL` pointing at a disposable PostgreSQL administration
target with database-creation permission. The fixture creates and removes only
its own databases and temporary files:

```sh
go test -race ./internal/operatorcontroller \
  -run '^TestGitOpsPublishBundleIsAcceptedAndAppliedByOperator$' -count=1
```

For the combined PostgreSQL and Kubernetes test, also set
`KUBEBUILDER_ASSETS` to the local Kubernetes 1.35 envtest binaries and run:

```sh
AUTOSQL_OPERATOR_ENVTEST=1 go test -race ./internal/operatorcontroller \
  -run '^TestGitOpsPublishBundleIsAcceptedAndAppliedByOperator$' -count=1
```

The API server is local and disposable. The diagnostics test below separately
qualifies two-manager leader election. Release and deployed-controller
qualification remain separate acceptance gates.

Run the deterministic contract scenarios without PostgreSQL or Kubernetes:

```sh
go test ./pkg/operator \
  -run 'Test(StoreRecordsAreIsolated|FileStoreFailedSavePreservesRecords|FileStorePersistsRecords)$'
go test ./pkg/operator -run 'TestOperator(Reliability|Completion|Checkpoint|Probe)'
go test ./internal/operatorcontroller -run TestCompletionStatusRoundTripAndLegacyCompatibility
go test ./internal/operatorcontroller -run 'Test(ControllerCompletion|CompletionPublication)'
```

The scenarios use temporary directories, prove preservation across failed writes
and file replacement, verify cleanup and reopening, and test record ownership
for both store implementations. Additional scenarios cover pre/post-apply write
failures, observation without repeat apply, bounded rejection diagnostics,
recovery write failure, current authorization expiry, suspension, resource UID,
and optional-status compatibility. They modify no application database.

With local Kubernetes 1.35 envtest binaries available, run the API validation:

```sh
AUTOSQL_OPERATOR_ENVTEST=1 go test ./internal/operatorcontroller \
  -run TestEnvtestCompletionTimestampSchema -count=1
```

Set `KUBEBUILDER_ASSETS` to the directory containing those binaries. This starts
and stops a disposable API server, verifies both CRD copies are identical, and
checks that the optional timestamp survives status writes and rejects malformed
values. It does not connect to a cluster or database.

## Diagnostics, metrics, and health

The operator initializes JSON logging before manager creation. Each attempted
reconciliation emits start/result records with namespace, name, generation,
validated release digest, outcome, reason and duration. Completion observations
include their stable audit `event_id` and recovery result. Connection strings,
SQL, artifact payloads and raw errors are excluded from diagnostics. Status API
failures remain visible in logs and metrics even when Kubernetes rejects status.

Transient errors retry from one second with exponential backoff and up to 10%
positive jitter, capped at 60 seconds. Deterministic evidence, drift, expiry and
authorization refusals recheck after five minutes. Generation, annotation and
watched Secret/ConfigMap changes permit immediate evaluation; suspension takes
priority. An unchanged warning is suppressed for five minutes, while a changed
reason or evidence category emits immediately. Retries are failures of
reconciliation, not counts of SQL executions. `retryCount` retains the largest
known cumulative count from local state, Kubernetes status and this process;
if both durable writes fail, that process-only increment cannot survive restart.

```sh
autosql operator --leader-election \
  --metrics-bind-address=:8080 --health-probe-bind-address=:8081
```

Both addresses accept `0` to disable their endpoints. `/metrics` exposes the
controller-runtime metrics plus the following initialized series:

| Metric | Labels and values |
| --- | --- |
| `autosql_operator_reconciliations_total` | `outcome`: success, failure, suspended; `reason`: bounded code below |
| `autosql_operator_reconcile_duration_seconds` | `outcome`: success, failure, suspended |
| `autosql_operator_completion_observations_total` | `outcome`: recovered, ineligible, refused, retryable |
| `autosql_operator_state_persistence_failures_total` | `stage`: pre_apply, post_apply, recovery |

Reason codes are `Applied`, `Adopted`, `CompletionRecovered`, `Suspended`,
`AwaitingApproval`, `ArtifactExpired`, `RecoveryEvidenceInvalid`,
`RecoveryStateDrifted`, `RecoveryAuditUnavailable`, `StatePersistenceFailed`,
`ReleaseAuthorizationInvalid`, `DependencyUnavailable`, `StatusPublicationFailed`,
`Invalid`, `ApplyFailed`, `Missing`, `Stale`, `Accepted`, `InputsChanged`,
`NotFound`, and `other`. Unknown codes map to `other`. Resource identities,
digests, targets and error strings never become metric labels. Deferred queue
entries log their deferral and do not count as fresh reconciliation attempts.
Observation counters count observer invocations, including a retry when
publication fails; they do not count database executions. Counters are per
process and reset on restart. Use increases/rates together with scrape health.

`/healthz` checks the running manager; `/readyz` checks cache synchronization.
Neither depends on a schema being Ready or the replica holding leadership.
A blocked schema therefore does not cause a healthy standby or leader to restart.
The Helm chart defaults to internal ports 8080/8081 and a ClusterIP metrics
Service with scrape annotations. Set `metrics.enabled=false` or
`healthProbe.enabled=false` to disable endpoints; `metrics.port` and
`healthProbe.port` customize distinct ports. `metrics.service.enabled=false`
disables only the Service. The standalone deployment and metrics Service expose
the same ports and probes. No public Ingress or additional operator RBAC is added.

Use an image built from this change before enabling these new flags/probes.
The checked-in release image pin is updated only during release qualification;
older binaries do not implement the new flags. Do not apply the new deployment
examples with the older pinned image. Keep the CRD upgrade ordering described above.

### Collection and a five-minute runbook

[The Prometheus example](../deploy/operator/prometheus.yaml) discovers each Pod's
named metrics port, covering Helm and standalone labels. Adjust its namespace
and label selector if the installation name differs. Merge the job into an
existing in-cluster collector with Pod discovery access and network access to
that port. Use either this job or Service-annotation discovery to avoid duplicate
scrapes. Do not scrape a load-balanced Service address as if it were one process.
The example follows [Prometheus Kubernetes discovery and relabeling](https://prometheus.io/docs/prometheus/latest/configuration/configuration/#kubernetes_sd_config).
Production owners choose collector credentials and network policy.

Within five minutes of a failure, follow these checks in order:

1. Read the resource conditions, `recoveryGuidance`, generation and artifact
   digest. Match the namespace/name and digest to a structured result log.
2. For `ArtifactExpired` or `ReleaseAuthorizationInvalid`, inspect the current
   approval/trust lifetimes and publish a fresh approved release when required.
   For `RecoveryEvidenceInvalid`, preserve history and investigate its exact
   mismatch. For `RecoveryStateDrifted`, inspect managed-schema drift.
3. For `RecoveryAuditUnavailable`, repair access to existing durable audit
   evidence. For `StatePersistenceFailed`, restore writable checkpoint storage.
   For `DependencyUnavailable`, restore the named dependency. For
   `StatusPublicationFailed`, restore Kubernetes status access. None of these
   repairs calls for replaying SQL to repair bookkeeping.
4. Check `/healthz` and `/readyz` on each replica and confirm that only the
   elected leader is processing resources. Match an observation's `event_id`
   to the existing audit chain. A failed resource can coexist with healthy Pods.
5. Inspect the collector's actual target selector and the following queries.
   Confirm expected targets are present; an empty query is missing evidence.

```promql
up{job="autosql-operator"}
sum by (outcome, reason) (increase(autosql_operator_reconciliations_total[30m]))
sum by (outcome) (increase(autosql_operator_completion_observations_total[30m]))
sum by (stage) (increase(autosql_operator_state_persistence_failures_total[30m]))
```

A successful endpoint scrape proves local emission. Production qualification
must separately capture backend ingestion and a 30-minute window without gaps
longer than two collection intervals. Record the image digest, target selection,
query window and result; do not infer uptime or SQL counts from a quiet counter.

Reproduce the supported and rejected diagnostic scenarios locally:

```sh
go test -race ./internal/operatorcontroller \
  -run 'Test(Managed|OperatorMetric|OperatorHealth|ReferenceChange)'
sh deploy/helm/autosql-operator/tests/render.sh
AUTOSQL_OPERATOR_ENVTEST=1 go test -race ./internal/operatorcontroller \
  -run '^TestOperatorDiagnosticsTwoReplicas$' -count=1
```

The last command requires local Kubernetes 1.35 envtest binaries through
`KUBEBUILDER_ASSETS`. It starts two separate operator processes, scrapes both,
verifies refusal/health behavior and leadership handover, then stops all owned
processes. Its injected observer never calls Apply. Real PostgreSQL recovery
and byte-identical history are covered by the separate signed-publisher fixture
above. These local checks do not claim production ingestion or a released image.

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
