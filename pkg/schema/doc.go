/*
Package schema defines AutoSQL's versioned, database-independent wire model.

# Compatibility

Documents and change sets carry explicit version strings. A consumer rejects an
unknown version and any resource kind it cannot safely understand. In contrast,
unknown JSON object fields are retained at every structural level and emitted
again, including inside resources, names, dependency edges, locations, graphs,
and changes. Kind-specific spec and details objects remain lossless JSON.

MarshalCanonical sorts resources and dependency sets, JSON object keys, and
extension fields. This makes output stable across runs and independent of input
object-key order. Resource IDs are derived from kind plus case-sensitive logical
identity and are therefore stable across inspection and desired-state sources.
*/
package schema
