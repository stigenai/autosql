# Zero-downtime migration format

`autosql.zero-downtime/v1` is AutoSQL's immutable expand/synchronize/contract
artifact. `pkg/zerodowntime` validates it entirely offline, before target
discovery or connection. JSON and YAML are interchangeable authoring formats;
the canonical JSON content digest is the signing identity in both cases.

Every operation signs its stable ID, object names, exact four-phase effects,
reversal mode, transformation, stable backfill ordering, unique constraint or
unique-index evidence, and batch size. IDs must be unique and sorted. Backfills
require a positive batch. PostgreSQL expressions are parsed into an AST and a
conservative node, immutable-function, and cast-type allowlist is applied;
subqueries, privileged calls, side effects, volatility, parameters, and extra
statements are rejected. Unknown or duplicated fields, YAML aliases, anchors,
custom tags, multiple documents, and trailing content are errors. Errors
identify the invalid field or operation but never echo source content.

The minimum supported server is PostgreSQL 14. Artifacts may select 14–18 and
are rejected before execution when the target is older. Version schemas expose
compatibility surfaces during expansion and may be retained after contract.

## Capability matrix

The classification is identical on PostgreSQL 14, 15, 16, 17, and 18; callers
must still compare the artifact's minimum version with the discovered target.

| Operation | Availability | Expand | Synchronize/backfill | Contract | Rewrite / lock | Reverse |
|---|---|---|---|---|---|---|
| add table | online | create table | none | publish | none / brief access-exclusive | drop if unused |
| add column | online | nullable column | stable batched backfill and dual-write | final constraints | none / brief access-exclusive | drop destination |
| rename column | conditional | compatibility destination | backfill and dual-write | remove source | shadow copy / brief access-exclusive | restore source name |
| alter column type | conditional | typed shadow column | immutable transform and dual-write | swap and remove source | shadow copy / brief access-exclusive | only for declared lossless transform |
| set not null | conditional | NOT VALID check | backfill and validate | set constraint | scan, no table rewrite / brief access-exclusive | drop constraint |
| create index | conditional | concurrently create | wait until valid | publish | none / share-update-exclusive | concurrently drop |
| drop index | conditional | hide from version schema | verify query paths | concurrently drop | none / share-update-exclusive | concurrently recreate |
| drop column | maintenance-required | hide | stop writes/readers | destructive drop | destructive / access-exclusive | backup only |
| drop table | maintenance-required | hide | stop writes/readers | destructive drop | destructive / access-exclusive | backup only |

The library exposes this matrix as structured data via `CapabilityMatrix` and
the exact four-phase effect via `Effects` so CLI and policy code do not need to
reimplement these decisions. Each artifact must carry those effects verbatim,
making phase intent part of its digest and signature. Concurrent index actions
must explicitly opt in and are rejected for partitioned indexes and indexes
backing constraints on PostgreSQL 14–18; those cases need a separately reviewed
maintenance plan.

Reversal is also signed. Ordinary additive and compatibility operations use
`automatic`; type transforms require `lossless` plus a separately validated
reverse expression; destructive table/column removal requires `backup` plus an
explicit backup reference. These are claims enforced by structure and policy,
not inferred from a generic operation label.

## Compatibility and upgrades

Legacy `autosql.zero-downtime/v0` JSON is accepted only through
`UpgradeLegacyJSON`; it is never silently decoded as v1. Upgrade converts the
string PostgreSQL version, assigns explicit timeout defaults, enables the
legacy version schema during expansion, records `upgraded_from`, validates all
operations under current rules, and produces a new digest that must be reviewed
and signed. Ed25519 signatures use a format-specific domain and the same
`sha256:<hex>` digest convention as immutable AutoSQL plan artifacts.
