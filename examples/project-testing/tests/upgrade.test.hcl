version = 1

migration_test "seed survives upgrade" {
  version "1" {
    seed {
      sql    = "INSERT INTO project_demo.accounts(id) VALUES ($1)"
      params = [42]
    }
  }
  version "2" {
    assert "new default preserves seeded rows" {
      sql    = "SELECT label FROM project_demo.accounts WHERE id = $1"
      params = [42]
      equals = "preserved"
    }
    assert "seed remains" {
      sql    = "SELECT count(*) FROM project_demo.accounts WHERE id = $1"
      params = [42]
      equals = 1
    }
  }
}
