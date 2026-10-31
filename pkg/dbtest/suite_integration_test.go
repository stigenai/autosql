package dbtest

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"autosql/pkg/migrate"
	"autosql/pkg/source"
)

type gatedSuiteFactory struct {
	OwnedFactory
	created chan struct{}
	release chan struct{}
}

func (f gatedSuiteFactory) Create(ctx context.Context, name string) (*DisposableTarget, error) {
	target, err := f.OwnedFactory.Create(ctx, name)
	f.created <- struct{}{}
	<-f.release
	return target, err
}

func TestSuiteLiveCancellationCleansEveryStartedCase(t *testing.T) {
	for _, mode := range []string{"container", "existing-admin"} {
		t.Run(mode, func(t *testing.T) {
			factory, major := liveSuiteFactory(t, mode)
			gate := gatedSuiteFactory{OwnedFactory: factory, created: make(chan struct{}, 2), release: make(chan struct{})}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			runner := SuiteRunner{Factory: gate, Parallel: 2, Schema: liveSchemaProgram(t, major)}
			cases := parsedSuite(t, "version = 1\nschema_test \"first\" {}\nschema_test \"second\" {}\nschema_test \"unstarted\" {}")
			finished := make(chan struct{})
			var result SuiteResult
			var runErr error
			go func() { result, runErr = runner.Run(ctx, cases); close(finished) }()
			for range 2 {
				select {
				case <-gate.created:
				case <-time.After(45 * time.Second):
					cancel()
					close(gate.release)
					<-finished
					t.Fatal("parallel fixture readiness timed out")
				}
			}
			cancel()
			close(gate.release)
			<-finished
			if !errors.Is(runErr, context.Canceled) || result.NotRun != 1 || result.Failed != 2 {
				t.Fatalf("cancel outcomes: %+v %v", result, runErr)
			}
			assertSuiteCleaned(t, result)
		})
	}
}

func TestSuiteLiveInlineConstraintSemantics(t *testing.T) {
	for _, mode := range []string{"container", "existing-admin"} {
		t.Run(mode, func(t *testing.T) {
			factory, major := liveSuiteFactory(t, mode)
			doc, err := source.ParseSQL("constraints.sql", `CREATE SCHEMA app;
CREATE TABLE app.parent(id bigint CONSTRAINT parent_pk PRIMARY KEY);
CREATE TABLE app.child(id bigint PRIMARY KEY, email text UNIQUE DEFERRABLE INITIALLY DEFERRED, parent_id bigint REFERENCES app.parent(id) ON DELETE SET NULL, balance integer CHECK(balance>=0));`)
			if err != nil {
				t.Fatal(err)
			}
			program, err := PrepareSchema(context.Background(), doc, major)
			if err != nil {
				t.Fatal(err)
			}
			cases := parsedSuite(t, `version = 1
schema_test "constraints" {
  fixture { sql = "INSERT INTO app.parent VALUES (1)" }
  fixture { sql = "INSERT INTO app.child VALUES (1, 'one', 1, 0)" }
  expect_error "primary key" {
    sql = "INSERT INTO app.parent VALUES (1)"
    sqlstate = "23505"
  }
  expect_error "check" {
    sql = "UPDATE app.child SET balance=-1"
    sqlstate = "23514"
  }
  expect_error "foreign key" {
    sql = "UPDATE app.child SET parent_id=2"
    sqlstate = "23503"
  }
  exec { sql = "DELETE FROM app.parent" }
  assert "referential action" {
    sql = "SELECT parent_id FROM app.child"
    equals = null
  }
  assert "unique timing" {
    sql = "SELECT condeferrable AND condeferred FROM pg_constraint WHERE conname='child_email_key' AND conrelid='app.child'::regclass"
    equals = true
  }
}
`)
			result, err := (SuiteRunner{Factory: factory, Schema: program, Parallel: 1}).Run(context.Background(), cases)
			if err != nil || result.Passed != 1 {
				t.Fatalf("constraints: %+v %v", result, err)
			}
			assertSuiteCleaned(t, result)
		})
	}
}

func liveSuiteFactory(t *testing.T, mode string) (OwnedFactory, int) {
	t.Helper()
	if mode == "container" {
		f := liveContainerFactory(t)
		return f, f.Definition.PostgresVersion
	}
	f, _ := liveAdminFactory(t)
	return f, f.Definition.PostgresVersion
}

func liveSchemaProgram(t *testing.T, major int) SchemaProgram {
	t.Helper()
	doc, err := source.ParseSQL("schema.sql", "CREATE SCHEMA runner_test; CREATE TABLE runner_test.items(id bigint PRIMARY KEY);")
	if err != nil {
		t.Fatal(err)
	}
	program, err := PrepareSchema(context.Background(), doc, major)
	if err != nil {
		t.Fatalf("%v: %v", err, errors.Unwrap(err))
	}
	return program
}

func parsedSuite(t *testing.T, raw string) []TestCase {
	t.Helper()
	f, err := ParseTestFile("tests/run.test.hcl", []byte(raw))
	if err != nil {
		t.Fatal(err)
	}
	return f.Cases()
}

func assertSuiteCleaned(t *testing.T, result SuiteResult) {
	t.Helper()
	for _, c := range result.Cases {
		if !c.Started {
			continue
		}
		if c.Lease == nil || c.Lease.State != "cleaned" {
			t.Fatalf("uncleaned case: %#v", c)
		}
		info, err := InspectLease(c.Lease.Path)
		if err != nil || info.State != "cleaned" {
			t.Fatalf("cleanup evidence: %v", err)
		}
	}
}

func TestSuiteLiveSchemaResultsIsolationAndCleanup(t *testing.T) {
	for _, mode := range []string{"container", "existing-admin"} {
		t.Run(mode, func(t *testing.T) {
			factory, major := liveSuiteFactory(t, mode)
			cases := parsedSuite(t, `version = 1
schema_test "passing" {
	setup { sql = "DO $$ BEGIN IF to_regclass('runner_test.items') IS NOT NULL THEN RAISE EXCEPTION 'schema prepared before setup'; END IF; END $$" }
  fixture { sql = "INSERT INTO runner_test.items VALUES (1)" }
  expect_error "duplicate" {
    sql = "INSERT INTO runner_test.items VALUES (1)"
    sqlstate = "23505"
  }
  assert "count" {
    sql = "SELECT count(*) FROM runner_test.items"
    equals = 1
  }
  cleanup { sql = "DELETE FROM runner_test.items" }
}
schema_test "first failure" {
  assert "fresh empty database" {
    sql = "SELECT count(*) FROM runner_test.items"
    equals = 2
  }
  cleanup { sql = "SELECT 1/0" }
}
schema_test "cleanup failure" {
  assert "fresh empty database" {
    sql = "SELECT count(*) FROM runner_test.items"
    equals = 0
  }
  cleanup { sql = "SELECT 1/0" }
}
schema_test "skip" { skip = "deliberately omitted" }
`)
			result, err := (SuiteRunner{Factory: factory, Schema: liveSchemaProgram(t, major), Parallel: 3}).Run(context.Background(), cases)
			var failure *SuiteError
			if !errors.As(err, &failure) || failure.Failure.Code != 5 || result.Passed != 1 || result.Failed != 2 || result.Skipped != 1 || result.Executed != 3 {
				t.Fatalf("suite=%#v err=%v", result, err)
			}
			if result.Cases[1].Primary == nil || len(result.Cases[1].Cleanup) != 1 || result.Cases[2].Primary != nil || len(result.Cases[2].Cleanup) != 1 {
				t.Fatal("primary/cleanup outcomes collapsed")
			}
			assertSuiteCleaned(t, result)
		})
	}
}

func TestSuiteLiveSeedUpgradeAndChangedInputRefusal(t *testing.T) {
	for _, mode := range []string{"container", "existing-admin"} {
		t.Run(mode, func(t *testing.T) {
			factory, _ := liveSuiteFactory(t, mode)
			dir := t.TempDir()
			_, err := migrate.Update(dir, migrate.UpdateRequest{Files: []migrate.File{{Name: "V1__initial.sql", SQL: []byte("CREATE TABLE items(id bigint PRIMARY KEY);")}, {Name: "V2__upgrade.sql", SQL: []byte("ALTER TABLE items ADD COLUMN label text NOT NULL DEFAULT 'preserved';")}}})
			if err != nil {
				t.Fatal(err)
			}
			snapshot, err := migrate.LoadSnapshot(dir)
			if err != nil {
				t.Fatal(err)
			}
			cases := parsedSuite(t, `version = 1
migration_test "upgrade" {
  version "1" {
    seed { sql = "INSERT INTO items VALUES (1)" }
  }
  version "2" {
    assert "preserved" {
      sql = "SELECT label FROM items WHERE id=1"
      equals = "preserved"
    }
  }
}
`)
			cases[0], err = BindMigrations(cases[0], snapshot)
			if err != nil {
				t.Fatal(err)
			}
			runner := SuiteRunner{Factory: factory, Parallel: 1, MigrationDirectory: "migrations"}
			result, err := runner.Run(context.Background(), cases)
			if err != nil || result.Passed != 1 || len(result.Cases[0].Versions) != 2 {
				t.Fatalf("upgrade=%#v err=%v", result, err)
			}
			assertSuiteCleaned(t, result)
			checks := 0
			runner.VerifyInputs = func(context.Context) error {
				checks++
				if checks == 3 {
					return errors.New("changed")
				}
				return nil
			}
			result, err = runner.Run(context.Background(), cases)
			var failure *SuiteError
			if !errors.As(err, &failure) || failure.Failure.Code != 5 || len(result.Cases[0].Versions) != 1 {
				t.Fatalf("changed snapshot continued: %#v %v", result, err)
			}
			assertSuiteCleaned(t, result)
		})
	}
}

func TestSuiteLiveTimeoutStillRunsCleanupAndCloses(t *testing.T) {
	for _, mode := range []string{"container", "existing-admin"} {
		t.Run(mode, func(t *testing.T) {
			factory, major := liveSuiteFactory(t, mode)
			cases := parsedSuite(t, `version = 1
schema_test "timeout" {
  exec { sql = "SELECT pg_sleep(30)" }
  cleanup { sql = "SELECT 1/0" }
  cleanup { sql = "SELECT 1" }
}
`)
			result, err := (SuiteRunner{Factory: factory, Schema: liveSchemaProgram(t, major), Parallel: 1, Timeout: 250 * time.Millisecond}).Run(context.Background(), cases)
			var failure *SuiteError
			if !errors.As(err, &failure) || failure.Failure.Code != 124 || len(result.Cases[0].Cleanup) != 1 || !strings.Contains(result.Cases[0].Cleanup[0].Message, "22012") {
				t.Fatalf("timeout/cleanup=%#v err=%v", result, err)
			}
			assertSuiteCleaned(t, result)
		})
	}
}
