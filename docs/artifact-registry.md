# Managed artifact registry

`pkg/artifact.ManagedRegistry` composes the immutable `artifact.Registry` stores
with the control-plane contracts needed for publication. A caller first
verifies an artifact, creates an `IntegrityManifest`, and submits a
`PushRequest`. The manifest binds both the artifact digest and the SHA-256 of
the canonical bytes; the registry also checks every configured required
attestation stage before writing. Existing `MemoryRegistry` and
`LocalRegistry` therefore remain content-addressed, immutable stores.

Tags are mutable names, never artifact identity. `Promote` verifies that the
target digest already exists and appends a `TagRecord` containing sequence,
actor, timestamp, old pointer, and new pointer. `TagHistory` is a read-only
copy of that append-only history, while `ResolveTag` reads the current pointer.

Authorization is split into `artifact.push`, `artifact.read`, and
`artifact.promote` actions through the `Authorizer` interface. A deployment
service can grant read access without granting publication or promotion, and a
failed authorization happens before the backing store is touched.

The in-memory tag index is deliberately a first implementation boundary. A
durable control-plane adapter should persist the same `TagRecord` schema with a
unique `(tag, sequence)` constraint and transactional pointer updates before
using it for multi-process operation.

## Authenticating a completion observation

`Artifact.VerifyForObservation(policy)` authenticates a signed artifact for
inspection after its execution lifetime has elapsed. It returns a
`VerifiedObservation`, a separate type that cannot be converted to the
`VerifiedArtifact` accepted by the executor and registry. It retains signature,
current signing-key validity/revocation, trusted release bindings, approval
identity/proof binding, attestation, and mandatory no-edits checks. Observation
also requires validation attestations to remain within their own validity
intervals; allowing artifact expiry never extends them. Future-dated
artifacts are still refused. `Payload()` returns an independent snapshot.

An observation token does not prove that SQL completed or that an external
approval policy still permits recovery. A completion observer must separately
check current authorization, exact execution history, current managed state,
and durable lifecycle audit evidence before publishing recovered readiness.
In particular, an independently expired approval proof cannot be extended by
allowing an expired artifact. This API alone does not enable operator recovery.

The production verification bridge uses the same trusted release policy as
apply, without resolving database credentials or writing audit records. These
are additive library APIs; the signed format and execution expiry rules are
unchanged. Trying to verify an expired observation payload for execution still
fails. The controller carries its opaque verified artifact snapshot into apply
instead of reopening the mounted artifact file after resolving references.
Apply revalidates that snapshot against current key policy, expiry, and no-edits
requirements before constructing a mutation.

`PrepareProductionCompletion` adds the external authorization checks required
by the controller. It verifies the original automation approval signature,
bundle/environment bindings, current identity roles, policy and safety inputs,
and actual signed approval expiry. Verification resolves no database, editor,
or rollback credentials and writes no audit records. It returns an empty
connection URL and an authorization deadline bounded by the signing key,
approval, attestations, and safety suppressions. Older manifests that stored only
an identity/proof digest remain unsupported for recovery.

The current operator publisher includes this signed proof and its public key in
new apply configurations. Its approval and validation windows are not extended:
expired published releases remain refused for recovery. Existing artifact
signatures and the execution verification API are unchanged.

Run the supported and rejected contract scenarios without an external service:

```sh
go test ./pkg/artifact ./internal/cli -run 'Test(Observation|ProductionObservation|CompletionApproval|ApplyRevalidatesVerifiedSnapshot)'
```

These scenarios cover elapsed lifetime, future creation, revocation, key expiry,
tampering, target and approval binding failures, token separation, snapshot
isolation, and the production no-edits boundary. They use temporary files and
generated signing keys, and open no database.
