schema "author_demo" {}

table "accounts" {
  schema       = schema.author_demo
  renamed_from = table["author_demo"]["customer_accounts"]

  column "id" { type = "bigint" }
  primary_key { columns = [column.id] }
}

moved {
  from = table["author_demo"]["customer_accounts"]["column"]["customer_id"]
  to   = table["author_demo"]["accounts"]["column"]["id"]
}
