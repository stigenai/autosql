# Roadmap candidate qualification

This record covers the local candidate for `specs/001-operator-reliability` and
Beads `autosql-bkb`, `autosql-f2x.1`, `autosql-f2x.4`, and `autosql-f2x.3`.
It does not close the broader developer-experience epic or its export,
visualization and external-source work.

## Delivery state on 2026-10-30

| Stage                             | Evidence and limit                                                                                                                                                                                                                                                                                     |
| --------------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------------ |
| Local implementation              | Complete in the isolated `codex/autosql-roadmap` checkout, based on `588dafe19127ebdf9e28afbd2707ad3cd8184cd1`. Changes are uncommitted. The original dirty checkout is preserved.                                                                                                                     |
| Local repository gates            | `go test ./...`, `go vet ./...`, `go build ./...`, affected race tests, chart/render validation, workflow lint and formatting passed. Live tests have separate required-case evidence; an unset-environment skip is not a live pass.                                                                   |
| Reliability tests                 | Recovery/refusal, audit/checkpoint failures, publication conflicts, two-process leader handover, bounded diagnostics and no-replay evidence passed. Concurrent-DDL qualification passed 20 coordinated trials plus stable controls on each PostgreSQL major 14–18.                                     |
| Factory and suite matrix          | Both factories, lifecycle commands, language, runner, CLI and public examples passed with macOS/arm64 and Linux/arm64 clients on PostgreSQL 14–18. The suite evidence summary verifies 100 required top-level passes and ten empty administration-resource inventories.                                |
| Packaged command matrix           | Ten platform/major tuples passed, each using both factories: 160 case executions including intentional failures. Required semantics, shared input bytes and reference-only configuration identities agree. Both PostgreSQL 17 real-signature integrations passed. These binaries are local candidates. |
| Disposable Kubernetes conformance | Helm install/upgrade, Flux Ready, Argo CD Synced/Healthy and Crossplane status propagation passed. The test cluster and its registries were removed; existing clusters were retained. This was not a production deployment.                                                                            |
| Remote CI and integration         | No remote run contains this uncommitted candidate. The base revision's PostgreSQL 18 log confirms the stale-plan failure. Its independent conformance log could not be refreshed because GitHub log storage returned `ServerBusy`. Local conformance success does not replace required remote checks.  |
| Published workflow                | Unfulfilled. The new manual `released project workflow` CI definition downloads a specified published version and requires a clean matching checkout. Native macOS/arm64 packaging is added to the release workflow. v0.1.53 predates the new commands and has Linux assets only.                      |
| Production rollout and monitoring | Unfulfilled and not authorized. No candidate image has been published or deployed, and no 30-minute backend-ingestion evidence exists for it.                                                                                                                                                          |

## Immutable candidate identities

The candidate version is `v0.1.53-roadmap.5cee2ded7f20`. Its source manifest is
`5cee2ded7f20b4c511b8757d8c02c3906846689f8706f81f361d81dc1d3274fe`.
All 230 production Go source/module files in that snapshot were compared with
the current working tree after qualification and still match. Later changes
add qualification drivers, source fixtures and documentation; those example
inputs have their own hashes in each run's evidence.

| Artifact                              | SHA-256                                                            |
| ------------------------------------- | ------------------------------------------------------------------ |
| Native macOS/arm64 binary, Go 1.26.8  | `fe2b3cd9c0680aba77c4003603ed12252f31f2a38ce215c72f7ee8ee11372a17` |
| macOS archive                         | `55a199be4099d57e1fcd3d3016714ff61a7e17c5c66c2f440000f75b239adea9` |
| Native Linux/arm64 binary, Go 1.25.14 | `9e8c52f29ed29b11bf8795d931cab3a093865490831d0c3873a84530df57346a` |
| Linux archive                         | `8af087d3b3da8a03aa0f67be17f54de31e7b181558871099c667fa39c38d553c` |
| Local operator image ID               | `b5e148bf81f402b5589096bf04b79ad51f3e0c9333651f7aab410fdf85a0d3a1` |

The image ID is a local Docker identity, **not a published registry manifest
digest**. The operator image used the repository Dockerfile's Go 1.25.12 build
stage. Kubernetes used the CI-pinned v1.35.5 node image. Local Helm was 3.20.2;
CI pins 4.2.3 and must still run. The installed kind 0.31.0 created the cluster
but refused image loading because of containerd configuration version 4; the
checksum-verified CI-pinned kind 0.32.0 loaded the image successfully. No
application assertion was relaxed. The first Linux driver attempt lacked jq;
the completed matrix used a derived disposable builder containing it.

The packaged driver records exact binary/version/platform/major identities,
runtime configuration, captured source bytes, ordered cases, assertions,
version transitions, JSON/JUnit, redaction results and cleanup checks. It
requires all selected required cases to execute. It proves empty/all-skipped,
production and changed-migration refusal; intentional assertion plus cleanup
failure; and nonzero report-write failure. Source SQL, native JSON and HCL all
produce target fingerprint
`sha256:141afe8ebc545d85f9c94c337adeb6d0b5a62bccdca1f81c714722ff5c84750c`.

Factory endpoint and PostgreSQL image changes correctly alter execution input
identities. Cross-platform comparison removes only timing, run/resource IDs
and the separately recorded database major from semantic results. It compares
shared input bytes and unchanged reference-only configuration identities; it
does not claim different runtime configurations have the same input digest.

Local evidence is under `/private/tmp/autosql-roadmap-candidate-20261030`,
`/private/tmp/autosql-packaged-final-macos`, and
`/private/tmp/autosql-packaged-final-linux`. The summaries are
`/private/tmp/autosql-suite-qualification-summary.json` and
`/private/tmp/autosql-packaged-qualification-summary.json`. Conformance output is
`/private/tmp/autosql-roadmap-conformance.log`. These are local evidence paths,
not durable remote CI artifacts. Reproduction commands are in the
[project example](../examples/project-testing/README.md#packaged-binary-qualification)
and [database testing guide](database-testing.md#verification).

## Compatibility and promotion

Version-1 configuration and existing direct/library callers retain their
supported behavior. Version-2 project inputs bind effective configuration and
current local bytes into release verification. Deploy a compatible controller
before publishing bundles with the new strict configuration fields. New SQL
inline-constraint metadata changes schema/plan fingerprints; regenerate and
approve affected releases instead of reinterpreting prior signed plans.

Recovery retains **current** approval, attestation and trust validity. The
existing publisher expires approvals before artifact expiry and validation
evidence at artifact expiry, so its expired releases remain refused. Successful
recovery requires independently complete execution/audit evidence and current
managed state, with no application-SQL replay. The current production cell must
remain refused if any required proof or authorization is no longer valid.

`autosql-bkb.7` and `.8` remain open until their delivery evidence exists. The
next promotion requires explicit authority to commit/push the isolated changes,
required remote CI and conformance success for that exact revision, and a
published candidate with immutable image/binary identities. Run the released
workflow from a clean matching checkout on the supported platform/version
matrix, including the separate signed integration. Local candidate evidence
cannot substitute for these gates.

Production integration belongs to the `stigen-flux` owner and requires separate
authorization. Verify the exact deployed digest and desired/new/ready replica
counts, then record reconciliation and recovery metrics in the monitoring
backend for 30 minutes with no gap longer than two configured scrape intervals.
Read the existing cell's evidence without retrying SQL or changing its resource.
If proof is invalid, document the refusal and the fresh-plan or authorized
adoption path. Missing metrics, unresolved required CI, unexpected SQL or a
rollout regression stops promotion. See the
[operator recovery and observability runbook](kubernetes-operator.md).
