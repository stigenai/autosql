# Dynamic and composite sources

`pkg/source` supports bounded external inputs without weakening the canonical
schema or reference-data contracts. A source is allowlisted, limited by bytes,
rows, and timeout, and emits a digest plus provenance record. HTTP sources
reject secret-bearing authorization headers; program sources execute an
allowlisted executable with argument arrays (never a shell string).

Supported adapters include local files, HTTP, external programs, CSV, JSON,
YAML, deterministic template directories, and JSON overlays. Offline CI uses a
content-addressed cache and lock map (`URI -> sha256:digest`); a missing cache
entry or digest mismatch fails closed. Unlocked network/file inputs remain
visible in provenance so release workflows can require locks.

Dynamic row decoding still passes through the reference-data limits before
planning. Template inputs are sorted, path-confined, and rendered with missing
variables rejected. Composite overlays merge nested objects and replace arrays,
which makes environment-specific overrides explicit and deterministic.
