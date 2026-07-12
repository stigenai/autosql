# Controlled plan editing

`autosql plan edit` accepts explicit artifact, SQL, editor identity, reason, and
output files. It never launches a shell or editor process. The output preserves
the byte-exact generated artifact and an immutable digest-linked provenance
chain while carrying a rebuilt candidate plan. Any edit changes the edit digest
and has no signature or reusable approval.

An edited plan becomes approval-eligible only through `planedit.Pipeline`, whose
order is fixed: location-aware SQL parsing, exact statement/change/order
rebinding, isolated simulation and final fingerprint verification, safety and
analyzer attestation, then policy/precheck/guardrail binding. Failure at any
stage prevents every later stage.

`--no-edits` is an apply-time policy, not a creation hint. Artifact verification,
the guarded CLI service, and the PostgreSQL executor can each enforce it. The
production configuration also supports `NoEdits`, so noninteractive digest and
artifact apply cannot bypass the restriction.
