# Advanced migration lint and AI-safe review

`pkg/safety.AdvancedAnalyzer` adds deterministic, provider-neutral checks to
the existing compatibility and PostgreSQL analyzers. Use `safety.AllBuiltins`
when the complete rule pack is required; `safety.Builtins` remains available
for compatibility-only integrations.

The advanced pack reports:

- lower-snake-case naming and optional ownership prefixes;
- dynamic SQL assembled through `EXECUTE`, concatenation, or formatting;
- table copies and full-table data dependencies;
- conflicting changes targeting the same resource;
- reproducible simulation-test tokens for data-writing or copy operations; and
- explicit untrusted agent provenance for AI-authored changes.

Every diagnostic contains a stable rule, affected object, evidence, impact,
remediation, confidence, and machine-readable properties. Generated test
tokens are hashes only: tests must run in a disposable environment and cannot
receive production credentials. Agent provenance never bypasses the normal
analyzer, policy, approval, or audit gates.

The analyzer is attested with an implementation, semantic version, and config
digest. Bind that attestation and the emitted diagnostics to the plan and
approval artifact; SARIF and JSON writers can then produce CI annotations
without changing the decision path.
