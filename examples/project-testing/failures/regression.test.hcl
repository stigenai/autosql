version = 1

migration_test "intentional regression" {
  version "1" {
    seed {
      sql = "INSERT INTO project_demo.accounts(id) VALUES (42)"
    }
  }
  version "2" {
    assert "deliberately wrong count" {
      sql    = "SELECT count(*) FROM project_demo.accounts"
      equals = 2
    }
  }
  cleanup {
    sql = "SELECT 1/0"
  }
}
