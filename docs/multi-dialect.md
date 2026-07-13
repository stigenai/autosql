# Multi-dialect capability contracts

`pkg/dialect` provides capability descriptors for the initial MySQL and SQL
Server slices. Descriptors declare managed kinds, supported operations, and
dialect-specific features. Normalization stamps the dialect and rejects a
document from another dialect; rendering refuses unsupported kinds instead of
emitting misleading portable SQL.

The contracts are compatible with the existing `pkg/plugin` driver SDK, so live
inspectors and dialect-specific renderers can be added without changing the
canonical schema or safety gates.
