schema "hcl_demo" {}

table "accounts" {
  schema  = "hcl_demo"
  options = ""

  column "id" {
    type     = "bigint"
    nullable = false
    ordinal  = 1
  }

  column "email" {
    type     = "text"
    nullable = false
    ordinal  = 2
  }

  column "display_name" {
    type     = "text"
    nullable = false
    default  = "'anonymous'"
    ordinal  = 3
  }

  column "last_seen_at" {
    type     = "timestamptz"
    nullable = true
    ordinal  = 4
  }
}
