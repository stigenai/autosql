# AutoSQL Constitution

Version: 1.0.0

Ratified: 2026-07-15

This constitution defines the non-negotiable engineering rules for AutoSQL.
Feature specifications, Beads issues, pull requests, releases, and exceptions
must comply with these principles.

## I. Examples and demos are part of the feature

A feature is not complete when only its implementation exists. Every shipped
feature must have all of the following:

1. discoverable user documentation describing its purpose, supported path,
   safety boundaries, and expected outcome;
2. an executable example, demo, contract scenario, integration scenario, or
   live workflow that shows the feature in use; and
3. automated evidence that detects when the documented behavior stops working.

The evidence level must be explicit:

- `example`: a runnable, user-facing example or script;
- `contract`: a deterministic executable scenario with no external service;
- `integration`: an executable scenario across multiple AutoSQL components;
- `live`: a scenario verified against a real external system such as
  PostgreSQL or Kubernetes.

Unit tests alone may support a feature but do not replace its user-facing
documentation. A contract or integration test may serve as the executable demo
when a standalone program would be artificial, provided the feature catalog
gives the exact command and the documentation explains the workflow.

## II. The feature catalog is the completeness ledger

[`examples/catalog.json`](examples/catalog.json) is the machine-readable source
of truth for shipped feature coverage. [`examples/README.md`](examples/README.md)
is its human entry point.

The catalog must:

- map every production Go package to exactly one coherent feature area;
- map every advertised PostgreSQL resource kind and feature flag;
- link at least one documentation page and one executable evidence artifact
  for every feature area;
- use repository-relative, existing paths and reproducible commands; and
- distinguish contract, integration, example, and live evidence honestly.

Catalog checks are required quality gates. Adding a package or advertising a
capability without updating its documentation and demo evidence must fail the
test suite.

## III. Demonstrations must prove the supported path

Demos must exercise public contracts and realistic workflows. They must not
bypass validation, approvals, dependency ordering, redaction, or safety gates
merely to produce a successful result. Where a feature is fail-closed, its
documentation must include both the supported path and the important rejection
boundary.

Examples must be deterministic, use disposable resources, accept credentials
only through documented secret references, and never require production data.
Live demos must state prerequisites, cleanup behavior, and the external systems
they mutate.

## IV. Feature changes update evidence in the same change

Any change that adds, expands, renames, or removes a feature must update the
catalog, documentation, examples, and executable evidence in the same review.
The Definition of Done for a feature includes:

- implementation and backward-compatibility behavior;
- documentation and a catalog entry;
- executable happy-path evidence;
- meaningful failure or safety-boundary evidence;
- integration coverage proportional to operational risk; and
- release notes when the user-visible contract changes.

A follow-up issue is not a substitute for this rule unless an explicit,
time-bounded exception is approved before merge.

## V. Exceptions are visible and temporary

An exception requires a Beads issue containing the missing evidence, reason,
owner, risk, compensating validation, and expiry condition. The catalog must
label the gap; it may not claim a stronger evidence level than exists. Expired
exceptions block release until resolved or renewed through review.

## Governance

Changes to this constitution require an explicit pull request, rationale,
migration plan for existing features, and a semantic version change:

- major: removes or weakens a principle;
- minor: adds a principle or materially expands enforcement; or
- patch: clarifies wording without changing obligations.

Reviewers must evaluate feature work against this constitution and the checked
catalog. The automated catalog test enforces structural coverage; reviewers
remain responsible for judging whether the linked demonstration is truthful,
useful, and proportionate to risk.
