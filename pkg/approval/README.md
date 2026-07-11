# Apply approval gate

`Gate` enforces environment allow-lists and risk-tiered approval requirements.
Plans may have an apply deadline. Approvals must match both the immutable plan digest and environment, be current,
come from distinct eligible identities, and carry required roles. Plan authors
and the apply requester cannot approve their own work. Emergency authorization
requires an identity and reason. Every allow or deny decision is appended to an
`AuditLog`; an allow is durable before the mutation callback can run, and audit
failure is fail-closed.
