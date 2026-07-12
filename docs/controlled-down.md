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

The shipped production service is enabled by `down_config_path` in the trusted
apply configuration. The referenced JSON supplies the owner-controlled
migration directory, revision schema, distinct development database reference
and runtime identity, plan-signing key reference and ID, plan TTL, owner-only
artifact directory, trusted release and generator key references, trusted
operator, optional reverse statements/checks, exact rule/object safety
suppressions, external approval authority data, and scoped override public
keys. Planning runs the complete static controls and
approval audit, then generator-attests, release-signs, verifies and atomically
publishes an artifact for exactly the down plan. The artifact uses source
revision `down:<locked-head>:<target>` and its digest/path are signed back into
the down plan. Apply will read no other artifact. Under the locked transaction
it proves the resulting semantic fingerprint equals the replayed target before
committing DDL, executor evidence, and the append-only reversal revision
together.
