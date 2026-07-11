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
or duplicate hint returns `schema.ErrAmbiguousRename` instead of choosing
silently.

A driver may annotate an object with `autosql.io/generated-name=true`. Two such
objects are treated as equivalent only when their kind, parent, and all
name-independent semantics match and the pairing is unique. This narrow rule
avoids churn from database-generated constraint and index names without hiding
user-defined renames.

## Semantic fingerprints

`schema.SemanticFingerprint` and `schema.ResourceFingerprint` produce versioned
SHA-256 fingerprints after canonical ordering. Source locations are deliberately
excluded; all other known and unknown fields remain semantic. Fingerprints are
therefore suitable for drift checks, caches, and reproducibility assertions,
but not as a substitute for driver normalization.
