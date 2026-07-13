# Database security as code

`pkg/security` provides the canonical, redacted model for database roles and
users, memberships, grants, default-privilege declarations, and row-level
security policies. A principal is explicitly `managed` or `external`; external
principals are observed for context but are never dropped or altered by a plan
unless an operator explicitly opts in.

Security plans are ordered so principals exist before memberships and grants.
Revokes and drops include affected access paths. A plan refuses to remove the
executing principal, an emergency principal, or their effective membership and
grant access. Apply can require a final inspection and digest comparison.

Authentication is always an opaque `secret.Reference` or a short-lived
`TokenSource`. `State.Sanitized`, digests, and audit-facing structures contain
no resolved credentials. Token sessions refresh before expiry and can be
cleared between targets during a fleet rollout.

`Policy` rule packs evaluate desired state and observed drift. Built-in helpers
cover PUBLIC grants and tables that have access without a row policy; custom
rules return an effective object path and remediation text. Rule-pack versions
should be pinned in CI and included in the approved plan bundle.
