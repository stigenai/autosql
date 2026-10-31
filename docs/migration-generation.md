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

## Project inputs and automation approvals

Add `--config project.hcl --env NAME` to select version-2 project inputs; `--dir`
and `--to` become optional explicit replacements. See
[project release generation](project-configuration.md#project-release-generation-and-verification)
for the input manifest, trusted Apply entry, and supported combinations.

The existing trusted generation configuration can optionally include:

```json
{
  "AutomationApproval": {
    "KeyID": "ci-approval",
    "Identity": "release-automation",
    "Roles": ["release-automation"],
    "PrivateKeyReference": "env://AUTOSQL_CI_APPROVAL_KEY",
    "TTL": "10m"
  }
}
```

This is a fragment of `generation.json`, not a complete configuration. The
provider signs the exact computed bundle and environment after planning,
simulation, and policy checks. It uses the same approval implementation as the
operator publisher. Its identity must differ from `Author` and `Requester`; its
key ID must differ from generator and release key IDs. Configure an
`ApprovalPolicy` that authorizes its roles. The approval TTL must be positive and
shorter than `Lifetime`. Do not combine it with `Approvals` or
`VerifiedApprovals`; omitting it preserves the existing precomputed-approval
path. Private keys remain secret references and are never returned.

The trusted Apply configuration carries `AutomationApprovalKeyID`,
`AutomationApprovalPublicKey`, and the original `ApprovalEvidence` alongside
`ApprovalIdentities` in the per-artifact release entry. Project releases verify
that original signature and expiry before Apply and again through the approval
gate. Editing the transported expiry or dropping required proof does not extend
authorization. This is independent of the release artifact's later expiry.

## Disposable integration proof

With `AUTOSQL_PROJECT_TEST_DSN` set to a disposable PostgreSQL 17 administrator
URL (including a username and password field), run:

```sh
go test -race ./internal/cli -run '^TestProjectReleaseLive' -count=1 -v
```

The test creates and removes uniquely named development and target databases,
invokes project generation with real signatures, builds the trusted Apply
configuration, refuses changed inputs before execution, refuses a rotated
reference pointing to another database, and applies the valid release after a
report-path move. It checks target schema state and audit-file absence for
input refusals. Without that environment variable it reports a skip; a skipped
run is not live qualification. It does not use production endpoints or prove a
released binary or production deployment.
