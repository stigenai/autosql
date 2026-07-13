# Shadow-column synchronization

`autosql migrate shadow-sync apply|status|remove` manages a deterministic
`BEFORE INSERT OR UPDATE` trigger per physical table. The trigger assigns to
`NEW` directly; it never issues another `UPDATE`, so it does not recurse or
multiply row writes. Multiple column pairs execute in stable ID order.

Supported transformation expressions are deliberately conservative:

- `value`
- `lower(value)`, `upper(value)`, `btrim(value)`
- `value::<safe_type>`

Functions are qualified through `pg_catalog`, and trigger functions run with
`search_path=pg_catalog`. Volatile or arbitrary SQL expressions are rejected.
Transformation exceptions abort the application statement atomically.

NULL is propagated in either direction. A write that changes only the old
representation computes the new one; a write that changes only the new
representation computes the old one. Conflicting simultaneous values fail
instead of choosing an implicit winner. A missing reverse transform makes
new-side writes fail and marks rollback ineligible. Lossy and non-reversible
pairs require explicit CLI policy switches.

Installation and removal are transactional, target/environment/artifact
scoped, advisory-locked, idempotent, and protected by trigger/function ownership
markers. Status reports pair count and the maximum transformation assignments
per row, making write amplification explicit.
