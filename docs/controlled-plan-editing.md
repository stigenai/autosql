# Controlled plan editing

`autosql plan edit` accepts explicit artifact, SQL, editor identity, reason, and
output files. It never launches a shell or editor process. The output preserves
the byte-exact generated artifact and an immutable digest-linked provenance
chain while carrying a rebuilt candidate plan. The original artifact is verified
against the configured release manifest before editing. Any edit changes the
edit digest and has no signature or reusable approval.

SQL is parsed by `pg_query_go`'s PostgreSQL parser. Transaction/session control,
role changes, COPY, explicit locks, advisory locks, search-path manipulation,
and AutoSQL history/audit access are rejected from the parse tree.

An edited plan becomes approval-eligible only through `planedit.Pipeline`, whose
order is fixed: location-aware SQL parsing, exact statement/change/order
rebinding, isolated simulation and final fingerprint verification, safety and
analyzer attestation, then policy/precheck/guardrail binding. Failure at any
stage prevents every later stage. `plan revalidate` serializes the typed stage
attestations; `plan publish` requires fresh post-edit approval, embeds the exact
original artifact and cryptographic provenance in the final artifact, signs it,
and writes it using no-follow/no-replace atomic creation.

`--no-edits` is an apply-time policy, not a creation hint. Artifact verification,
the guarded CLI service, and the PostgreSQL executor can each enforce it. The
production configuration also supports `NoEdits`, so noninteractive digest and
artifact apply cannot bypass the restriction.
