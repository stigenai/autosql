variable "environment" {
  type    = string
  default = "development"

  validation {
    condition     = var.environment == "development" || var.environment == "production"
    error_message = "environment must be development or production"
  }
}

locals {
  schema_name = "author_demo"
}

module "accounts" {
  source = "accounts"
  inputs = {
    schema_name = local.schema_name
    environment = var.environment
  }
}

table "audit_log" {
  schema = module.accounts.schema_name

  column "id" {
    type = "bigint"
    identity { mode = "always" }
  }

  column "account_id" { type = "bigint" }
  column "event" {
    type    = "text"
    null    = false
    default = literal("created")
  }
  column "created_at" {
    type    = "timestamptz"
    null    = false
    default = sql("CURRENT_TIMESTAMP")
  }

  primary_key { columns = [column.id] }
  foreign_key "audit_log_account_fkey" {
    columns     = [column.account_id]
    ref_columns = [table.accounts.column.id]
    on_delete   = "cascade"
  }
  index "audit_log_account_idx" {
    columns = [column.account_id, column.created_at]
  }
}

table "regional_queue" {
  for_each = {
    east = { region = "us-east-1" }
    west = { region = "us-west-2" }
  }
  name   = "queue_${each.key}"
  schema = module.accounts.schema_name

  column "id" { type = "bigint" }
  column "region" {
    type    = "text"
    default = literal(each.value.region)
  }
  primary_key { columns = [column.id] }
}
