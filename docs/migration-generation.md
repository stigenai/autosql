# Migration generation

`autosql migrate generate --generation-config <file>` is the production
generation boundary. It verifies the immutable snapshot and replays every
migration in manifest order into a short-lived database on the explicitly
configured development PostgreSQL server. Runtime server identities must prove
that the development and production databases are distinct.

The replayed schema is inspected rather than inferred from SQL. AutoSQL then
loads the desired source, builds a semantic PostgreSQL plan, and simulates that
exact plan from the replayed schema. Publication is refused unless the simulated
fingerprint exactly equals the desired fingerprint. Built-in safety analyzers,
the configured policy, signed count-query prechecks, canonical statement
bindings, and the complete guardrail bundle are evaluated before signing. The
prechecks and exact candidate statements execute through the guardrail's trusted
approval authority and durable audit against the disposable replay database.
Approval identity, timestamps, and proof digest in the artifact are derived only
from approvals verified by that authority.

Each SQL migration has a canonical, generator-attested and release-signed
artifact in the same immutable generation. Typed replay/simulation, safety, and
policy/precheck/guardrail attestations are signed with it. Its plan, check, and guardrail bundle digests are also bound into the
manifest directives. The snapshot digest, source and target fingerprints, and
rename-hint digest are signed metadata. Version, label, format, and rename hints
are explicit inputs. Rename hints are canonical JSON mapping an existing stable
resource ID to one unique desired qualified name; they participate in semantic
diffing and malformed, ambiguous, duplicate, or stale hints fail closed.

The manifest update is the sole publication point and uses the verified head as
a compare-and-swap token. The migration SQL and embedded artifact therefore
appear together or not at all; crash recovery is delegated to the migration
directory journal. Diverged/nonlinear heads are typed conflicts, and concurrent
processes racing from one head have exactly one winner. Any replay, simulation,
control, signing, or publication failure leaves the published snapshot intact.
A semantic no-op returns before creating generation state and does not change
any migration-directory byte or metadata timestamp.

There is no legacy publication mode. The shipped command requires a trusted
generation configuration; invocation without it refuses before reading or
writing migration state.
