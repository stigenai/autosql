# External schema provider protocol

`autosql.provider/v1` is the boundary for ORM and generated-code integrations.
An implementation receives a `Request` containing a source URI, environment,
string parameters, cache key, and host-enforced timeout. It returns a canonical
`autosql.schema/v1` document and optional source-located diagnostics. It never
receives a database URL, connection, transaction, or write capability; metadata
must set `read_only: true`, and the host rejects mutating providers.

Metadata negotiates provider name/version, protocol version, supported resource
kinds, features, and implementation languages. Hosts reject unknown protocol
versions, duplicate/unknown kinds, invalid documents, missing diagnostic codes,
and deadline violations. The SDK copies parameters, applies a deadline,
canonicalizes and validates the document, sorts diagnostics, and emits
`sha256:` provider and state digests. Equivalent requests therefore produce
byte-identical state and stable cache identities.

The wire shape is JSON and is intentionally language-neutral. The Go SDK is in
`pkg/provider`; `fixtures/python_provider.py` demonstrates a process provider
in a second implementation language. Process adapters should use one JSON
request/response per line, preserve `source` coordinates, and write logs to
stderr. A provider error is surfaced as a CLI/CI diagnostic without discarding
its source location.
