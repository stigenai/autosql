schema "project_demo" {}

table "accounts" {
  schema = "project_demo"
  column "id" {
    type     = "bigint"
    nullable = false
    ordinal  = 1
  }
  column "label" {
    type     = "text"
    nullable = false
    default  = "'preserved'"
    ordinal  = 2
  }
}
