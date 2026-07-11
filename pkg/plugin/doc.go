/*
Package plugin defines extension contracts for database drivers and desired
state providers.

Compatibility is negotiated before use. API major versions must match and a
plugin may require no newer minor version than the host implements. Capabilities
are explicit per canonical resource kind: omitted/unsupported kinds cannot be
inspected or changed, read-only kinds may be inspected but not rendered for
management, and managed kinds support the complete driver lifecycle.

GuardDriver and GuardSource are host-side boundaries. They recover plugin
panics, retain a stack for logging, avoid exposing panic details to users, wrap
ordinary errors with plugin and operation context, and preserve cancellation and
sentinel error identity. Out-of-process transports should additionally enforce
process and resource isolation while implementing these same interfaces.
*/
package plugin
