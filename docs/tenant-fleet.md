# Tenant-scale fleet rollout

`pkg/tenantfleet` discovers active tenant databases, validates stable tenant
and target identities, sorts and hashes immutable snapshots, and executes
tenant-isolated rollouts with canaries, per-tenant policy overrides, bounded
concurrency, partial-failure reporting, and resumable checkpoints.

Each executor call receives exactly one tenant and its override. A failed
canary prevents later tenants from being mutated; already-passed tenants can
be resumed without reapplying them. Connection references remain opaque and
are never shared across tenants.
