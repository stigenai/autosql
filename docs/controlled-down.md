# Controlled down migrations

`autosql migrate down --to <revision> --dry-run` produces a signed down plan;
it does not mutate the database. The plan binds the verified manifest and
generation, the clean applied revision head, every original signed artifact
being reversed, the locked live fingerprint, the replayed prior fingerprint,
the semantic reverse plan, prechecks, optional author reverse SQL, destructive
impacts, and expiry. Dry-run output includes destructive effects and required
preconditions.

Planning refuses dirty, failed, partial, uncertain, stale, or divergent heads.
The prior schema must be reconstructed by replaying the verified manifest to the
selected revision. Author reverse SQL is digest-bound to its original version,
artifact and scope. Reference-data/business changes without reverse logic are
irreversible unless a trusted, scoped, expiring override signs the actor and
reason. Nontransactional reversals require a separate signed scope because an
ambiguous outcome cannot be inferred safely.

Apply reloads state after acquiring the canonical target lock, verifies the down
plan against the locked manifest and revision head, obtains a fresh guardrail-
approved signed artifact for exactly the reverse plan and checks, and delegates
to the canonical executor/revision transaction. That transaction appends a new
reversal revision and events linking all original artifacts. Existing history is
never rewritten or erased. Any uncertain executor outcome remains dirty and must
be reconciled before another down operation.
