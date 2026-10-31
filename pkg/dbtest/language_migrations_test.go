package dbtest

import (
	"errors"
	"testing"

	"autosql/pkg/migrate"
)

func testMigrationCase(t *testing.T, last string) TestCase {
	t.Helper()
	file, err := ParseTestFile("upgrade.test.hcl", []byte("version = 1\nmigration_test \"upgrade\" {\n version \"1\" {}\n version \""+last+"\" {}\n}"))
	if err != nil {
		t.Fatal(err)
	}
	return file.Cases()[0]
}

func TestBindMigrationsValidatesImmutableVersions(t *testing.T) {
	dir := t.TempDir()
	_, err := migrate.Update(dir, migrate.UpdateRequest{Files: []migrate.File{{Name: "V1__initial.sql", SQL: []byte("CREATE TABLE items(id integer);")}, {Name: "V2__upgrade.sql", SQL: []byte("ALTER TABLE items ADD COLUMN label text;")}}})
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err := migrate.LoadSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	c, err := BindMigrations(testMigrationCase(t, "2"), snapshot)
	if err != nil {
		t.Fatal(err)
	}
	if c.MigrationDigest() != snapshot.Manifest.Digest || len(c.migrations[0]) != 1 || len(c.migrations[1]) != 1 || c.migrations[1][0].version != "2.0.0" {
		t.Fatalf("binding: %#v", c)
	}
	snapshot.Files["V2__upgrade.sql"][0] = 'X'
	if c.migrations[1][0].statements[0].SQL != "ALTER TABLE items ADD COLUMN label text" {
		t.Fatal("snapshot bytes aliased")
	}
	if _, err = BindMigrations(testMigrationCase(t, "2"), snapshot); err == nil {
		t.Fatal("modified SQL accepted")
	}
	valid, err := migrate.LoadSnapshot(dir)
	if err != nil {
		t.Fatal(err)
	}
	_, err = BindMigrations(testMigrationCase(t, "3"), valid)
	var located *LanguageError
	if !errors.As(err, &located) || located.Range.StartLine != 4 {
		t.Fatalf("missing version unlocated: %v", err)
	}
	valid.Manifest.Entries[0].SQLDigest = "tampered"
	if _, err = BindMigrations(testMigrationCase(t, "2"), valid); err == nil {
		t.Fatal("modified manifest accepted")
	}
}

func TestBindMigrationsRejectsTransactionControlInAtomicFiles(t *testing.T) {
	for _, mode := range []string{"auto", "required", "forbidden"} {
		dir := t.TempDir()
		_, err := migrate.Update(dir, migrate.UpdateRequest{Files: []migrate.File{
			{Name: "V1__initial.sql", SQL: []byte("SELECT 1;")},
			{Name: "V2__escape.sql", SQL: []byte("-- autosql:transaction=" + mode + "\nCOMMIT; SELECT 1;")},
		}})
		if err != nil {
			t.Fatal(err)
		}
		snapshot, err := migrate.LoadSnapshot(dir)
		if err != nil {
			t.Fatal(err)
		}
		_, err = BindMigrations(testMigrationCase(t, "2"), snapshot)
		if (err == nil) != (mode == "forbidden") {
			t.Fatalf("transaction mode %s: %v", mode, err)
		}
	}
}
