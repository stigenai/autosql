schema "friendly" {}

extension "hstore" {
  schema  = "friendly"
  version = "1.8"
}

composite_type "contact_profile" {
  schema = "friendly"
  dependencies = [uses(extension_id("friendly", "hstore"))]
  attributes = [
    composite_attribute("email", "text", 1),
    collated_composite_attribute("display_name", "text", 2, "pg_catalog.\"C\""),
    composite_attribute("metadata", "friendly.hstore", 3),
  ]
}

resource "enum" "account_status" {
  schema    = "friendly"
  parent    = schema_id("friendly")
  spec_json = jsonencode({ values = ["pending", "active", "disabled"] })
  deps_json = jsonencode([contains(schema_id("friendly"))])
}

table "organizations" {
  schema = "friendly"

  column "id" {
    type     = "bigint"
    nullable = false
    ordinal  = 1
  }
}

table "accounts" {
  schema = "friendly"

  column "id" {
    type     = "bigint"
    nullable = false
    ordinal  = 1
  }

  column "organization_id" {
    type     = "bigint"
    nullable = false
    ordinal  = 2
  }

  column "email" {
    type     = "text"
    nullable = false
    ordinal  = 3
  }

  column "status" {
    type     = "friendly.account_status"
    nullable = false
    default  = "'pending'"
    ordinal  = 4
  }
}

resource "primary_key" "organizations_pkey" {
  schema    = "friendly"
  parent    = table_id("friendly", "organizations")
  spec_json = jsonencode({ definition = "PRIMARY KEY (id)" })
  deps_json = jsonencode([
    contains(table_id("friendly", "organizations")),
    references(column_id("friendly", "organizations", "id")),
  ])
}

resource "primary_key" "accounts_pkey" {
  schema    = "friendly"
  parent    = table_id("friendly", "accounts")
  spec_json = jsonencode({ definition = "PRIMARY KEY (id)" })
  deps_json = jsonencode([
    contains(table_id("friendly", "accounts")),
    references(column_id("friendly", "accounts", "id")),
  ])
}

resource "unique_constraint" "accounts_email_key" {
  schema    = "friendly"
  parent    = table_id("friendly", "accounts")
  spec_json = jsonencode({ definition = "UNIQUE (email)" })
  deps_json = jsonencode([
    contains(table_id("friendly", "accounts")),
    references(column_id("friendly", "accounts", "email")),
  ])
}

resource "check_constraint" "accounts_email_check" {
  schema    = "friendly"
  parent    = table_id("friendly", "accounts")
  spec_json = jsonencode({ definition = "CHECK (position('@' in email) > 1)" })
  deps_json = jsonencode([
    contains(table_id("friendly", "accounts")),
    references(column_id("friendly", "accounts", "email")),
  ])
}

resource "foreign_key" "accounts_organization_fkey" {
  schema    = "friendly"
  parent    = table_id("friendly", "accounts")
  spec_json = jsonencode({ definition = "FOREIGN KEY (organization_id) REFERENCES friendly.organizations(id) ON DELETE CASCADE" })
  deps_json = jsonencode([
    contains(table_id("friendly", "accounts")),
    references(column_id("friendly", "accounts", "organization_id")),
    references(column_id("friendly", "organizations", "id")),
  ])
}

resource "index" "accounts_email_idx" {
  schema    = "friendly"
  parent    = table_id("friendly", "accounts")
  spec_json = jsonencode({
    method     = "btree"
    unique     = false
    valid      = true
    ready      = true
    definition = "CREATE INDEX accounts_email_idx ON friendly.accounts USING btree (email)"
  })
  deps_json = jsonencode([
    contains(table_id("friendly", "accounts")),
    references(column_id("friendly", "accounts", "email")),
  ])
}

role "friendly_reader" {
  login       = false
  superuser   = false
  create_role = false
  create_database = false
  inherit     = true
}

role "friendly_app" {
  login       = false
  superuser   = false
  create_role = false
  create_database = false
  inherit     = true
}

resource "policy" "accounts_reader_policy" {
  schema    = "friendly"
  parent    = table_id("friendly", "accounts")
  spec_json = jsonencode({
    command    = "r"
    roles      = ["friendly_reader"]
    permissive = true
    using      = "status = 'active'"
  })
  deps_json = jsonencode([
    contains(table_id("friendly", "accounts")),
    references(column_id("friendly", "accounts", "status")),
    references(resource_id("role", "", "", "friendly_reader")),
  ])
}

resource "grant" "friendly_reader_select" {
  schema    = "friendly"
  parent    = table_id("friendly", "accounts")
  spec_json = jsonencode({
    grantor   = "friendly_app"
    grantee   = "friendly_reader"
    privilege = "SELECT"
    grantable = false
  })
  deps_json = jsonencode([
    references(table_id("friendly", "accounts")),
    references(resource_id("role", "", "", "friendly_app")),
    references(resource_id("role", "", "", "friendly_reader")),
  ])
}

resource "membership" "friendly_app_to_reader" {
  spec_json = jsonencode({
    parent = "friendly_reader"
    member = "friendly_app"
    admin  = false
  })
  deps_json = jsonencode([
    references(resource_id("role", "", "", "friendly_reader")),
    references(resource_id("role", "", "", "friendly_app")),
  ])
}

resource "default_privilege" "friendly_future_tables" {
  spec_json = jsonencode({
    owner       = "friendly_app"
    object_type = "r"
    schema      = "friendly"
    grantee     = "friendly_reader"
    privilege   = "SELECT"
    grantable   = false
  })
  deps_json = jsonencode([
    references(resource_id("role", "", "", "friendly_app")),
    references(resource_id("role", "", "", "friendly_reader")),
    references(schema_id("friendly")),
  ])
}
