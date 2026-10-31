version = 1

schema_test "bound scalar assertions" {
  timeout = "30s"

  fixture {
    sql    = "INSERT INTO project_demo.accounts(id) VALUES ($1)"
    params = [42]
  }
  assert "fixture is present" {
    sql    = "SELECT count(*) FROM project_demo.accounts WHERE id = $1"
    params = [42]
    equals = 1
  }
  assert "boolean retains its type" {
    sql    = "SELECT EXISTS(SELECT 1 FROM project_demo.accounts WHERE id = 42)"
    equals = true
  }
  expect_error "required id" {
    sql      = "INSERT INTO project_demo.accounts(id) VALUES (NULL)"
    sqlstate = "23502"
  }
  cleanup {
    sql = "DELETE FROM project_demo.accounts"
  }
}
