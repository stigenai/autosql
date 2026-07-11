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
its conservative assumption. If a row-scan or rewrite threshold is configured
but statistics are missing, that threshold is marked unproven and the finding
is promoted to an error. Rewrite-size caps apply only to actual rewrite risks.
An unknown PostgreSQL version is never replaced with an arbitrary default;
version-sensitive rules use the oldest supported behavior and low confidence.

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

PostgreSQL's fast non-volatile-default behavior is modeled from version 11
onward, distinguishing literals and stable expressions from volatile or unknown
expressions. PostgreSQL 12's transactional enum support is modeled separately
from the rule that a newly added enum value remains unusable until commit.
Statement rules ignore SQL comments, literals, quoted identifiers, and
dollar-quoted bodies. New rules should prefer conservative findings with
explicit assumptions when target facts are unavailable.
