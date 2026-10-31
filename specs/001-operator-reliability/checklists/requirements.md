# Specification Quality Checklist: AutoSQL Recommended Roadmap

**Purpose**: Validate the full requested requirements/design before
implementation.

**Created**: 2026-10-29

**Feature**: [spec.md](../spec.md) | **Plan**: [plan.md](../plan.md)

This is a quality review record, not an implementation task tracker. Beads owns
execution state. A pass means the design is ready to implement, not that runtime
acceptance has already passed. The user selected the full recommended roadmap;
R1 reliability, R2 configuration/factories, and R3 testing/release proof are all
in scope.

## Content Quality

| Criterion                                                       | Result | Evidence                                                                            |
| --------------------------------------------------------------- | ------ | ----------------------------------------------------------------------------------- |
| Specification describes outcomes, not implementation mechanisms | Pass   | Technical contracts and package choices live in design artifacts                    |
| User value and intended audience are explicit                   | Pass   | Operators, developers, CI owners, and release owners have seven prioritized stories |
| Mandatory sections complete                                     | Pass   | Stories, edge cases, requirements, entities, measurable criteria, assumptions       |
| Scope reflects the user's selection                             | Pass   | All six recommended areas covered across R1/R2/R3; broader epic boundaries explicit |

## Requirement Completeness

| Criterion                                       | Result | Evidence                                                                                                           |
| ----------------------------------------------- | ------ | ------------------------------------------------------------------------------------------------------------------ |
| No unresolved product-policy clarification      | Pass   | Scope, supported platforms, identity boundary, compatibility, and reporting defaults are explicit                  |
| Requirements testable and unambiguous           | Pass   | FR-001 through FR-028 define supported/rejected behavior                                                           |
| Success criteria measurable and outcome-focused | Pass   | SC-001 through SC-010 cover timings, repetitions, concurrency, compatibility, report integrity, and secret leakage |
| Independent acceptance scenarios present        | Pass   | Seven stories each define a runnable end-user validation boundary                                                  |
| Edge cases covered                              | Pass   | Partial recording, expiry, authorization, drift, target aliases, failure cleanup, empty selection, reports         |
| Dependencies and assumptions identified         | Pass   | R2 prerequisites for R3; R1 promotion; existing Beads blockers and external owner gates                            |
| Existing behavior preserved deliberately        | Pass   | Version-1 configuration, CLI exit/output contracts, approval authority, and existing operator behavior             |

## Feature Readiness and Traceability

| Requirements  | Stories | Design/contract                                                  | Acceptance evidence                                                                    | Outcomes               |
| ------------- | ------- | ---------------------------------------------------------------- | -------------------------------------------------------------------------------------- | ---------------------- |
| FR-001–FR-008 | 1       | Completion observer, persistence, operator contract              | Recovery/refusal matrix and restart/status-conflict tests                              | SC-001, SC-002         |
| FR-009–FR-012 | 2       | Reasons, logs, metrics, retries                                  | Runbook, redaction, endpoint/backend ingestion                                         | SC-003, SC-005         |
| FR-013–FR-014 | 3       | Planning boundary and minimum-role constraints                   | Coordinated interference and stable controls, 20 trials/major, cleanup                 | SC-004                 |
| FR-015–FR-016 | All     | Constitution and delivery gates                                  | Catalog, required CI, immutable release and rollout evidence                           | SC-006                 |
| FR-017–FR-019 | 4       | Project versioning, selection, source ranges, effective identity | Legacy fixtures, JSON/HCL equivalence, override/tamper and no-side-effect tests        | SC-007, SC-010         |
| FR-020–FR-022 | 5       | Factory modes, protected identities, owned leases                | Both factories, platform/version matrix, ten concurrent cases, faults and cleanup      | SC-008, SC-010         |
| FR-023–FR-025 | 6       | Located DSL, runner, scalar/errors, JSON/JUnit/exit contracts    | Seeded upgrade, intentional failures, empty/skipped rejection, cleanup/report failures | SC-009, SC-010         |
| FR-026–FR-028 | 7       | Help/examples, bounded release proof, Beads scope accounting     | Released-binary local/CI run and evidence comparison; broader epic left open           | SC-006, SC-009, SC-010 |

Each functional requirement has acceptance coverage. All seven primary flows and
their material rejection boundaries are represented. Implementation details are
in [research](../research.md), [data model](../data-model.md), the
[operator contract](../contracts/operator.md), and the
[project/testing contract](../contracts/project-testing.md).

## Backlog Accounting

| Bead/scope                                                                                     | Coverage in this roadmap                         | Completion rule                                                                       |
| ---------------------------------------------------------------------------------------------- | ------------------------------------------------ | ------------------------------------------------------------------------------------- |
| Recovery predecessors `autosql-ptg.3`, `autosql-q40`, `autosql-v00`; concurrency `autosql-0si` | R1 follow-up regression work                     | Do not reopen old work automatically; use evidence-linked follow-ups                  |
| `autosql-f2x.1`                                                                                | R2 versioned project/environment workflow        | Close only after full issue acceptance and public workflow evidence                   |
| `autosql-f2x.4`                                                                                | R2 container/admin factories and owned cleanup   | Close only after declared supported-platform/version and fault/parallel qualification |
| `autosql-f2x.3`                                                                                | R3 DSL, schema/migrate test commands, reports    | Preserve `.4` blocker; require config integration and end-user evidence               |
| `autosql-f2x.7`                                                                                | Operator diagnostics and help for added commands | Partial only; whole command surface/completions remain outside                        |
| `autosql-f2x.8`                                                                                | Bounded project/testing released workflow        | Partial only; all existing source/export/visualization dependencies still required    |
| `autosql-f2x.2`, `.5`, `.6`, native publication `autosql-45s`                                  | Not implemented by these milestones              | Preserve their acceptance and deferral state                                          |

## Notes

Completion observation still refuses when audit evidence or current
authorization cannot be established. The production cell resource is not
declared recoverable. Test-target protection requires an explicit trusted
development allowlist; a name or absent denylist is insufficient. Abrupt process
death may leave owned resources, which must be reported and recoverable through
verified cleanup.

No constitution exception is proposed. Incident reproduction and
runtime/platform qualification remain implementation gates. Formatting, link,
requirement identity, and Spec Kit structural checks validate this planning
deliverable only.
