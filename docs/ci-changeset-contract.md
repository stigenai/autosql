# CI changeset and review contract

`pkg/ci` implements `autosql.ci/v1`. A job declares a base and head reference
(`branch`, `tag`, `registry`, or `migration_directory`). `Detect` walks only
the head's parent chain, filters the declared migration directory, rejects
cycles, missing parents, duplicate revisions, and non-ancestor bases, and
sorts the selected revisions deterministically. Thus a non-linear or stale
history fails with rebase guidance instead of analyzing an accidental superset.

The review pipeline runs named, independent stages for replay, diff, lint,
tests, and policy. Every stage returns stable diagnostics and a result digest.
Results are exposed as terminal text, JSON, SARIF 2.1.0, and a neutral PR
annotation model (`level`, `message`, `path`, line/column, rule code), allowing
GitHub, GitLab, and other CI systems to render the same review.

When signing is configured, the attestation binds source revision, exact
changeset digest, artifact digest, policy version, and a digest of test stage
results. CI must verify the Ed25519 signature and compare those bindings to
trusted job context before accepting a merge. This prevents stale reports or
results from another artifact/policy from being replayed.
