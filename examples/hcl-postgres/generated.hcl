schema "generated_demo" {}

# Managed only when the normalized body digest is independently supplied in
# reviewed_routine_digests. Without that authority this definition is inert.
resource "function" "lifecycle_state_to_v2(value text)" {
  schema = "generated_demo"
  parent = schema_id("generated_demo")
  spec_json = jsonencode({
    name               = "lifecycle_state_to_v2"
    identity_arguments = "value text"
    result             = "text"
    language           = "sql"
    volatility         = "i"
    security_definer   = false
    leakproof          = false
    parallel           = "s"
    definition         = "CREATE FUNCTION generated_demo.lifecycle_state_to_v2(value text) RETURNS text LANGUAGE sql IMMUTABLE RETURN upper(value)"
  })
  deps_json = jsonencode([contains(schema_id("generated_demo"))])
}

resource "table" "jobs" {
  schema = "generated_demo"
  parent = schema_id("generated_demo")
  spec_json = jsonencode({
    partitioned        = false
    persistence        = "p"
    row_security       = false
    force_row_security = false
  })
  deps_json = jsonencode([contains(schema_id("generated_demo"))])
}

resource "column" "state" {
  schema = "generated_demo"
  parent = table_id("generated_demo", "jobs")
  spec_json = jsonencode({ type = "text", not_null = false, ordinal = 1 })
  deps_json = jsonencode([contains(table_id("generated_demo", "jobs"))])
}

resource "column" "state_v2" {
  schema = "generated_demo"
  parent = table_id("generated_demo", "jobs")
  spec_json = jsonencode({
    type      = "text"
    not_null  = false
    ordinal   = 2
    default   = "generated_demo.lifecycle_state_to_v2(state)"
    generated = "s"
  })
  deps_json = jsonencode([
    contains(table_id("generated_demo", "jobs")),
    references(column_id("generated_demo", "jobs", "state")),
    references(resource_id("function", "generated_demo", schema_id("generated_demo"), "lifecycle_state_to_v2(value text)")),
  ])
}
