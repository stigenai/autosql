# Policy language v1

Policies are versioned JSON documents containing variables, reusable predicates,
and rules. A rule targets `schema`, `migration`, or `all` resources and may filter
resource kinds. Expressions support `all`, `any`, `not`, `predicate`, `eq`, `ne`,
`in`, `matches`, and `exists`. Values can refer to `resource.kind`,
`resource.name`, `resource.owner`, `resource.attributes.<key>`, and
`variables.<key>`. Rule packs wrap a policy document with an organization-owned
name and independent version. Evaluation is bounded by context cancellation,
resource count, metered validation/traversal/expression/regex steps, bounded
pattern and input sizes, and an injectable-clock timeout. Missing references
fail comparisons. JSON numbers retain `json.Number` precision and equality uses
exact rational comparison across JSON and Go numeric representations. Recursive
values are restricted to JSON-compatible scalars, arrays, and string-keyed maps;
depth, item count, aggregate bytes, numeric digits, and exponent magnitude are
metered before comparison or rational allocation. Map entries consume those
budgets before keys are retained for deterministic sorting, and numeric literals
pass a hard raw-byte gate before lexical scanning.
