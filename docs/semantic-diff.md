# Semantic schema diff

AutoSQL represents inspected databases as canonical `schema.Document` graphs.
Before comparison, callers normalize each document with its database driver.
PostgreSQL normalization canonicalizes established type aliases, stable default
expressions, conventional generated names, and known SQL definition fields. It
does not recursively rewrite unknown extension data, so newer semantics survive
older clients.

`schema.Diff(current, desired, options)` returns a deterministic `ChangeSet`.
Creates follow dependency order and drops reverse it, so children are removed
before their parents. Every change has a stable identifier and explicit
`depends_on` edges. Resource input order does not affect the result.

## Scope selection

`DiffOptions.Include` and `DiffOptions.Exclude` accept `path.Match` patterns
against resource IDs, qualified names such as `public.users`, and kind-qualified
names such as `table:public.users`. Includes add the selected resources plus
their parent and dependency closure. Excludes remove matching resources and all
of their dependents, preventing an invalid partial graph. `schema.Select` makes
the same behavior available without computing a diff.

## Renames and generated names

General renames are never guessed. Supply `DiffOptions.RenameHints` with an old
and new resource ID or qualified name. A missing, multiply matched, wrong-kind,
cross-parent, or conflicting hint returns `schema.ErrAmbiguousRename` instead
of choosing silently. When a renamed object also changes semantically, the
change set emits an ordered rename followed by an alter.

An explicitly renamed container carries uniquely identifiable descendants to
the new parent. Columns and same-named constraints or indexes are therefore
renamed with the table rather than dropped and recreated. A child hint may
cross parent IDs only when those parents are themselves an explicit rename
pair; unrelated moves remain ambiguous.

A database inspector may attach process-only generated-name provenance after
proving an exact catalog derivation. This trust is neither serialized nor
accepted from public annotations, so an authored document cannot forge it. A
conventional-looking suffix alone is not evidence. Two proven generated
objects are treated as equivalent only when
their kind, parent, and all name-independent semantics match and the pairing is
unique. This narrow rule
avoids churn from database-generated constraint and index names without hiding
user-defined renames.

## Semantic fingerprints

`schema.SemanticFingerprint` and `schema.ResourceFingerprint` produce versioned
SHA-256 fingerprints after canonical ordering. Source locations are deliberately
excluded; all other known and unknown fields remain semantic. Fingerprints are
therefore suitable for drift checks, caches, and reproducibility assertions,
but not as a substitute for driver normalization.
