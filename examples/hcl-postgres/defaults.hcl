schema "defaults_demo" {}

resource "enum" "job_status" {
  schema    = "defaults_demo"
  parent    = schema_id("defaults_demo")
  spec_json = jsonencode({ values = ["pending", "done"] })
  deps_json = jsonencode([contains(schema_id("defaults_demo"))])
}

resource "domain" "positive_int" {
  schema = "defaults_demo"
  parent = schema_id("defaults_demo")
  spec_json = jsonencode({
    base_type   = "integer"
    not_null    = false
    constraints = ["CHECK (VALUE > 0)"]
  })
  deps_json = jsonencode([contains(schema_id("defaults_demo"))])
}

resource "sequence" "widget_id_seq" {
  schema = "defaults_demo"
  parent = schema_id("defaults_demo")
  spec_json = jsonencode({
    start     = 10
    increment = 2
    min       = 1
    max       = 9223372036854775807
    cache     = 1
    cycle     = false
  })
  deps_json = jsonencode([contains(schema_id("defaults_demo"))])
}

resource "table" "widgets" {
  schema = "defaults_demo"
  parent = schema_id("defaults_demo")
  spec_json = jsonencode({
    partitioned       = false
    persistence       = "p"
    row_security      = false
    force_row_security = false
  })
  deps_json = jsonencode([contains(schema_id("defaults_demo"))])
}

resource "column" "id" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({
    type     = "bigint"
    default  = "nextval('defaults_demo.widget_id_seq'::regclass)"
    not_null = true
    ordinal  = 1
  })
  deps_json = jsonencode([
    contains(table_id("defaults_demo", "widgets")),
    references(resource_id("sequence", "defaults_demo", schema_id("defaults_demo"), "widget_id_seq")),
  ])
}

resource "column" "price" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "numeric(10,2)", default = "0.00", not_null = true, ordinal = 2 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}
resource "column" "active" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "boolean", default = "true", not_null = true, ordinal = 3 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "text_state" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "text", default = "'pending'::text", not_null = true, ordinal = 4 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "metadata" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "jsonb", default = "'{}'::jsonb", not_null = true, ordinal = 5 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "items" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "jsonb", default = "'[]'::jsonb", not_null = true, ordinal = 6 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "external_id" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "uuid", default = "'550e8400-e29b-41d4-a716-446655440000'::uuid", not_null = true, ordinal = 7 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "generated_id" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "uuid", default = "gen_random_uuid()", not_null = true, ordinal = 8 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "state" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({
    type     = "defaults_demo.job_status"
    default  = "'pending'::defaults_demo.job_status"
    not_null = true
    ordinal  = 9
  })
  deps_json = jsonencode([
    contains(table_id("defaults_demo", "widgets")),
    uses(resource_id("enum", "defaults_demo", schema_id("defaults_demo"), "job_status")),
  ])
}

resource "column" "positive" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "defaults_demo.positive_int", default = "5", not_null = true, ordinal = 10 })
  deps_json = jsonencode([
    contains(table_id("defaults_demo", "widgets")),
    uses(resource_id("domain", "defaults_demo", schema_id("defaults_demo"), "positive_int")),
  ])
}

resource "column" "empty_tags" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "text[]", default = "'{}'::text[]", not_null = true, ordinal = 11 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "tags" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "text[]", default = "ARRAY['a'::text, 'b'::text]", not_null = true, ordinal = 12 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "business_date" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "date", default = "CURRENT_DATE", not_null = true, ordinal = 13 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "created_at" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "timestamptz", default = "now()", not_null = true, ordinal = 14 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "local_stamp" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "timestamp(2)", default = "LOCALTIMESTAMP(2)", not_null = true, ordinal = 15 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "utc_stamp" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "timestamp", default = "timezone('utc'::text, now())", not_null = true, ordinal = 16 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "delay" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "interval", default = "'00:05:00'::interval", not_null = true, ordinal = 17 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "code" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "character(4)", default = "'x'::character(1)", not_null = true, ordinal = 18 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "flags" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "bit(4)", default = "'1010'::bit(4)", not_null = true, ordinal = 19 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "dbos_updated_at" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({
    type     = "bigint"
    default  = "(extract(epoch from CURRENT_TIMESTAMP) OPERATOR(pg_catalog.*) 1000)::bigint"
    not_null = true
    ordinal  = 20
  })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "client_network" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "cidr", default = "'10.0.0.0/8'::cidr", not_null = true, ordinal = 21 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "client_address" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "inet", default = "'192.0.2.1/24'::inet", not_null = true, ordinal = 22 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "client_mac" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "macaddr", default = "'08:00:2b:01:02:03'::macaddr", not_null = true, ordinal = 23 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}

resource "column" "generated_text_id" {
  schema = "defaults_demo"
  parent = table_id("defaults_demo", "widgets")
  spec_json = jsonencode({ type = "text", default = "pg_catalog.gen_random_uuid()::text", not_null = true, ordinal = 24 })
  deps_json = jsonencode([contains(table_id("defaults_demo", "widgets"))])
}
