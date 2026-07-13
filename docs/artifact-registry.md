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
