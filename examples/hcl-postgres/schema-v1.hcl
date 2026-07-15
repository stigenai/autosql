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
}
