# Policy language v1

Policies are versioned JSON documents containing variables, reusable predicates,
and rules. A rule targets `schema`, `migration`, or `all` resources and may filter
resource kinds. Expressions support `all`, `any`, `not`, `predicate`, `eq`, `ne`,
`in`, `matches`, and `exists`. Values can refer to `resource.kind`,
`resource.name`, `resource.owner`, `resource.attributes.<key>`, and
`variables.<key>`. Rule packs wrap a policy document with an organization-owned
name and independent version. Evaluation is bounded by context cancellation,
resource count, expression steps, and an injectable-clock timeout.
