package dbtest

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

func TestParseTestFileSQLStatementBoundaries(t *testing.T) {
	for _, query := range []string{"SELECT 1; -- trailing comment", "/* leading /* nested */ comment */ SELECT 1; /* trailing */", `SELECT E'escaped\';semicolon';`, "DO $$ BEGIN PERFORM 1; END $$;"} {
		raw, _ := json.Marshal(query)
		_, err := ParseTestFile("statements.test.hcl", []byte("version = 1\nschema_test \"a\" {\nexec { sql = "+string(raw)+" }\n}"))
		if err != nil {
			t.Fatal(err)
		}
	}
	for _, query := range []string{"-- only comment", "/* nested /* comment */ */", "SELECT 1; SELECT 2"} {
		raw, _ := json.Marshal(query)
		if _, err := ParseTestFile("statements.test.hcl", []byte("version = 1\nschema_test \"a\" {\nexec { sql = "+string(raw)+" }\n}")); err == nil {
			t.Fatal("empty or multiple SQL operations accepted")
		}
	}
}

func TestParseTestFileOrderedImmutableCases(t *testing.T) {
	input := []byte(`version = 1
schema_test "scalar checks" {
  timeout = "30s"
  fixture { sql = "INSERT INTO items VALUES (1)" }
  assert "integer" {
    sql = "SELECT count(*) FROM items WHERE id = $1"
    params = [1, "text", true, null, -2]
    equals = 1
  }
  expect_error "duplicate" {
    sql = "INSERT INTO items VALUES (1)"
    sqlstate = "23505"
  }
  cleanup { sql = "DELETE FROM items" }
}
migration_test "upgrade" {
  version "1.0" {
    seed { sql = "INSERT INTO items VALUES (1)" }
    assert "seeded" {
      sql = "SELECT count(*) FROM items"
      equals = 1
    }
  }
  version "2" {
    assert "preserved" {
      sql = "SELECT count(*) FROM items"
      equals = 1
    }
  }
}
schema_test "temporarily skipped" {
  skip = "Waiting for the approved schema addition"
}
`)
	file, err := ParseTestFile("tests/example.test.hcl", input)
	if err != nil {
		t.Fatal(err)
	}
	cases := file.Cases()
	if len(cases) != 3 || cases[0].Kind() != "schema" || cases[0].Timeout() != 30*time.Second || cases[2].SkipReason() == "" {
		t.Fatalf("cases: %#v", cases)
	}
	ops := cases[0].Operations()
	if ops[2].SQLState != "23505" || ops[2].NonTransactional {
		t.Fatalf("expected error lost: %#v", ops[2])
	}
	if len(ops) != 3 || len(cases[0].Cleanup()) != 1 || ops[1].Range.StartLine != 5 || ops[1].Equals != IntegerScalar(1) || ops[1].Params[4] != IntegerScalar(-2) {
		t.Fatalf("operations: %#v", ops)
	}
	if got := cases[1].Operations(); len(got) != 5 || got[0].Kind != "version" || got[1].Kind != "seed" || got[3].Version != "2.0.0" {
		t.Fatalf("upgrade order: %#v", got)
	}
	before := file.Digest()
	input[0] = 'X'
	ops[1].Params[0] = StringScalar("changed")
	ops[1].SQL = "changed"
	cases[0] = TestCase{}
	if file.Cases()[0].Operations()[1].Params[0] != IntegerScalar(1) || file.Digest() != before || len(before) != 64 {
		t.Fatal("parsed input is aliased")
	}
}

func TestParseTestFileRejectsLocatedInvalidInputs(t *testing.T) {
	validAssert := "assert \"value\" {\n sql = \"SELECT 1\"\n equals = 1\n}"
	for name, text := range map[string]string{
		"late schema setup":     "version = 1\nschema_test \"a\" {\nexec { sql = \"SELECT 1\" }\nsetup { sql = \"SELECT 1\" }\n}",
		"missing version":       `schema_test "a" {}`,
		"unknown version":       "version = 2\nschema_test \"a\" {}",
		"string version":        "version = \"1\"\nschema_test \"a\" {}",
		"no cases":              "version = 1",
		"duplicate case":        "version = 1\nschema_test \"a\" {}\nmigration_test \"a\" { version \"1\" {} }",
		"unknown attribute":     "version = 1\nschema_test \"a\" { target = \"secret-value\" }",
		"shell hook":            "version = 1\nschema_test \"a\" { shell { command = \"secret-value\" } }",
		"missing name":          "version = 1\nschema_test {}",
		"empty skip":            "version = 1\nschema_test \"a\" { skip = \" \" }",
		"bad timeout":           "version = 1\nschema_test \"a\" { timeout = \"0s\" }",
		"large timeout":         "version = 1\nschema_test \"a\" { timeout = \"25h\" }",
		"fraction":              "version = 1\nschema_test \"a\" {\n" + strings.Replace(validAssert, "equals = 1", "equals = 1.5", 1) + "\n}",
		"overflow":              "version = 1\nschema_test \"a\" {\n" + strings.Replace(validAssert, "equals = 1", "equals = 9223372036854775808", 1) + "\n}",
		"object scalar":         "version = 1\nschema_test \"a\" {\n" + strings.Replace(validAssert, "equals = 1", "equals = {}", 1) + "\n}",
		"expression":            "version = 1\nschema_test \"a\" {\n" + strings.Replace(validAssert, "equals = 1", "equals = 1 + 1", 1) + "\n}",
		"function":              "version = 1\nschema_test \"a\" { fixture { sql = file(\"secret-value\") } }",
		"interpolation":         "version = 1\nschema_test \"a\" { fixture { sql = \"${env.secret}\" } }",
		"missing SQL":           "version = 1\nschema_test \"a\" { exec {} }",
		"multiple statements":   "version = 1\nschema_test \"a\" { exec { sql = \"SELECT 1; SELECT 2\" } }",
		"no migration versions": "version = 1\nmigration_test \"a\" {}",
		"backwards":             "version = 1\nmigration_test \"a\" {\nversion \"2\" {}\nversion \"1\" {}\n}",
		"equivalent duplicate":  "version = 1\nmigration_test \"a\" {\nversion \"1\" {}\nversion \"1.0\" {}\n}",
		"schema version":        "version = 1\nschema_test \"a\" { version \"1\" {} }",
		"unversioned seed":      "version = 1\nmigration_test \"a\" { seed { sql = \"SELECT 1\" } }",
		"bad SQLSTATE":          "version = 1\nschema_test \"a\" {\nexpect_error \"x\" {\nsql = \"SELECT 1\"\nsqlstate = \"23*\"\n}\n}",
		"connection SQLSTATE":   "version = 1\nschema_test \"a\" {\nexpect_error \"x\" {\nsql = \"SELECT 1\"\nsqlstate = \"08006\"\n}\n}",
		"cancel SQLSTATE":       "version = 1\nschema_test \"a\" {\nexpect_error \"x\" {\nsql = \"SELECT 1\"\nsqlstate = \"57014\"\n}\n}",
	} {
		t.Run(name, func(t *testing.T) {
			_, err := ParseTestFile("invalid.test.hcl", []byte(text))
			var located *LanguageError
			if !errors.As(err, &located) || located.Range.Filename != "invalid.test.hcl" || located.Range.StartLine < 1 || located.Range.StartColumn < 1 {
				t.Fatalf("unlocated error: %v", err)
			}
			if strings.Contains(err.Error(), "secret-value") {
				t.Fatal("input leaked in diagnostic")
			}
		})
	}
}

func TestParseTestFileLiteralScalarsAndBounds(t *testing.T) {
	for _, literal := range []string{`"hello"`, `true`, `false`, `null`, `-9223372036854775808`, `9223372036854775807`} {
		_, err := ParseTestFile("scalars.test.hcl", []byte("version = 1\nschema_test \"a\" {\nassert \"x\" {\nsql = \"SELECT $1\"\nparams = ["+literal+"]\nequals = "+literal+"\n}\n}"))
		if err != nil {
			t.Fatalf("%s: %v", literal, err)
		}
	}
	for _, input := range []string{strings.Repeat(" ", maxTestFileBytes+1), "version = 1\nx = " + strings.Repeat("[", 65) + "1" + strings.Repeat("]", 65)} {
		if _, err := ParseTestFile("bounded.test.hcl", []byte(input)); err == nil {
			t.Fatal("unbounded input accepted")
		}
	}
}

func FuzzParseTestFile(f *testing.F) {
	for _, seed := range []string{"version = 1\nschema_test \"a\" {}", "version = 1\nmigration_test \"upgrade\" {\nversion \"1\" {}\nversion \"2\" {}\n}", "version = 1\nschema_test \"a\" {\nassert \"x\" {\nsql = \"SELECT $1\"\nparams = [null, -1]\nequals = true\n}\n}"} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, input []byte) {
		file, err := ParseTestFile("fuzz.test.hcl", input)
		if err != nil {
			var located *LanguageError
			if !errors.As(err, &located) || located.Range.StartLine < 1 || located.Range.StartColumn < 1 {
				t.Fatalf("unlocated language error: %v", err)
			}
			return
		}
		if len(file.Cases()) == 0 || len(file.Digest()) != 64 {
			t.Fatal("successful parse lacks cases or input identity")
		}
	})
}
