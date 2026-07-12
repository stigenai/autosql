# Migration directories

AutoSQL stores an ordered SQL history behind a canonical
`.autosql-manifest.json`. A manifest identifies one immutable directory under
`.autosql-generations/`; publishing a new manifest is the only operation that
changes the visible history. Readers therefore observe the complete old
generation or the complete new generation, including across a process crash.
Old generations are retained rather than destructively rewritten.

Migration names use `V<major>[.<minor>[.<patch>]][-prerelease]__<name>.sql`.
Versions are normalized to three numeric components and sorted with SemVer
prerelease precedence. Versions and case-folded filenames are unique. A normal
entry points to its immediate predecessor. A fork must set `nonlinear` and list
existing, earlier parent versions explicitly; missing, duplicate, cyclic,
forward, or spuriously nonlinear edges are rejected with remediation guidance.

Typed header directives are comments before the first SQL token:

```sql
-- autosql:transaction=required
-- autosql:plan-digest=sha256:<64 lowercase hex characters>
-- autosql:check-digest=sha256:<64 lowercase hex characters>
-- autosql:bundle-digest=sha256:<64 lowercase hex characters>
```

Transaction mode is `auto`, `required`, or `forbidden`. Unknown and duplicate
directives are errors. The manifest binds raw SQL, typed directives, the ordered
statement text and source boundaries, parent edges, and the transitive parent
chain. Canonical, duplicate-key-free JSON plus deterministic generation and
manifest digests make additions, removals, edits, and reordering detectable.

All readers take a shared process lock and all writers take its exclusive form.
After initial creation, every update must provide the manifest digest it read as
a compare-and-swap precondition. Storage is accessed relative to trusted
directory descriptors with no-follow opens. The root, locks, generations, and
files must be owner-controlled; linked, writable-by-others, non-regular, and
oversized inputs are rejected.

An update writes and synchronizes every migration, the generation directory,
the generation parent, and the candidate manifest before writing its canonical
authorization journal. The manifest rename occurs last and its directory is
synchronized before the journal is retired. The journal contains the exact
candidate, compare-and-swap precondition, and any legacy cleanup. `Recover` can
therefore finish that exact unpublished transition, or finish cleanup after a
published transition, without guessing or deleting a generation. A retry must
present the same candidate and precondition; unrelated work cannot adopt a
pending journal. `MigrateManifest` uses the same crash-recoverable transaction
for its checked `v0` to `v1` conversion while retaining the canonical legacy
manifest as `.autosql-manifest.json.v0`.
