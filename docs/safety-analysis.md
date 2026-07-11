# Migration safety analysis

`pkg/safety` analyzes a canonical `schema.ChangeSet` before database mutation.
The built-in analyzers cover destructive and application-incompatible changes
as well as PostgreSQL lock, rewrite, scan, index-build, and transaction risks.

```go
diagnostics, err := (safety.Runner{
    Analyzers: safety.Builtins(),
}).Run(ctx, safety.Input{
    Changes: changeSet,
    Target: safety.Target{Engine: "postgresql", Version: 16},
    Thresholds: safety.Thresholds{
        MaxLockLevel: safety.LockShareUpdateExclusive,
        MaxRowsScanned: 1_000_000,
        MaxRewriteBytes: 10 << 30,
    },
})
```

Rendered statements can be supplied with their change and source location.
They enable statement-specific checks such as `TRUNCATE`, non-concurrent index
builds, constraint validation, and commands that cannot safely run in a normal
migration transaction. Statement SQL is deliberately excluded from report
serialization. Human, JSON, and SARIF reporters additionally redact common
credential, token, and connection-URL forms.

Target statistics are optional and keyed by stable resource ID. When absent,
the PostgreSQL analyzer retains static findings, lowers confidence, and records
its conservative assumption. Thresholds promote excessive lock, row-scan, or
rewrite estimates to errors for a policy gate to enforce.

Suppressions are narrow audit records: each must identify exactly one rule and
one object and include a non-empty reason. An optional expiry stops the
suppression automatically. Suppressed findings stay in every report so an
approval trail is preserved.

Rule IDs are stable public API:

| Rule | Risk |
| --- | --- |
| `AUTOSQL001` | dropped object |
| `AUTOSQL002` | truncated table |
| `AUTOSQL003` | narrowing type conversion |
| `AUTOSQL004` | new `NOT NULL` requirement |
| `AUTOSQL005` | changed default semantics |
| `AUTOSQL006` | changed constraint semantics |
| `AUTOSQL007` | application-visible rename |
| `AUTOSQL101` | blocking PostgreSQL DDL |
| `AUTOSQL102` | PostgreSQL table rewrite |
| `AUTOSQL103` | blocking index build |
| `AUTOSQL104` | table validation scan |
| `AUTOSQL105` | transaction-restricted statement |

PostgreSQL's fast constant-default behavior is modeled from version 11 onward.
Other version-sensitive decisions include enum and concurrent-index transaction
restrictions. New rules should prefer conservative findings with explicit
assumptions when target facts are unavailable.
