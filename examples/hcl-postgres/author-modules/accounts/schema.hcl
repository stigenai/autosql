variable "schema_name" { type = string }
variable "environment" { type = string }

schema "accounts_namespace" {
  for_each = { selected = var.schema_name }
  name     = each.value
  comment  = "Account resources managed by AutoSQL"
}

enum "account_state" {
  schema = var.schema_name
  values = ["pending", "active", "disabled"]
}

table "accounts" {
  schema  = var.schema_name
  comment = "Tenant accounts"

  column "id" {
    type = "bigint"
    identity { mode = "always" }
  }
  column "email" {
    type = "text"
    null = false
  }
  column "state" {
    type    = enum.account_state
    null    = false
    default = enum_value(enum.account_state, "pending")
  }
  column "metadata" {
    type    = "jsonb"
    null    = false
    default = cast(literal("{}"), "jsonb")
  }

  primary_key { columns = [column.id] }
  unique "accounts_email_key" { columns = [column.email] }
  check "accounts_email_check" { expr = sql("position('@' in email) > 1") }
  index "accounts_active_idx" {
    columns = [column.email]
    where   = sql("state = 'active'::author_demo.account_state")
  }
}

output "schema_name" {
  type  = string
  value = var.schema_name
}
