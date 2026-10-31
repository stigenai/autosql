package dbtest

import (
	"autosql/pkg/migrate"
	"autosql/pkg/source"
	pg_query "github.com/pganalyze/pg_query_go/v6"
)

type testMigration struct {
	version, file string
	transaction   migrate.TransactionMode
	statements    []source.Statement
}

// BindMigrations validates the complete manifest and every SQL/artifact byte,
// then captures the ordered SQL needed for each transition. Input maps/slices
// remain caller-owned; neither later mutation nor a report accessor can change
// the program. This grants no production execution capability.
func BindMigrations(c TestCase, snapshot migrate.Snapshot) (TestCase, error) {
	if c.kind != "migration" {
		return TestCase{}, &LanguageError{Range: c.at, Message: "migration binding requires a parsed migration test"}
	}
	verified, err := migrate.PrefixSnapshot(snapshot, len(snapshot.Manifest.Entries))
	if err != nil {
		return TestCase{}, &LanguageError{Range: c.at, Message: "migration snapshot or manifest has changed or is invalid"}
	}
	positions, err := testVersionPositions(verified)
	if err != nil {
		return TestCase{}, &LanguageError{Range: c.at, Message: "test migrations require a linear, ordered manifest"}
	}
	c.migrationDigest = verified.Manifest.Digest
	c.migrations = map[int][]testMigration{}
	previous := -1
	for i, op := range c.operations {
		if op.Kind != "version" {
			continue
		}
		position, ok := positions[op.Version]
		if !ok || position <= previous {
			return TestCase{}, &LanguageError{Range: op.Range, Message: "migration version is missing or does not advance the snapshot"}
		}
		steps, err := testMigrationSteps(verified, previous+1, position+1)
		if err != nil {
			return TestCase{}, &LanguageError{Range: op.Range, Message: "migration statements are invalid"}
		}
		c.migrations[i] = steps
		previous = position
	}
	return c, nil
}

func testVersionPositions(snapshot migrate.Snapshot) (map[string]int, error) {
	positions := map[string]int{}
	var previous *migrate.Version
	for i, entry := range snapshot.Manifest.Entries {
		version, err := migrate.ParseVersion(entry.Version)
		if err != nil || entry.NonLinear {
			return nil, migrate.ErrInvalid
		}
		if previous != nil && previous.Compare(version) >= 0 {
			return nil, migrate.ErrInvalid
		}
		positions[version.String()] = i
		previous = &version
	}
	return positions, nil
}

func testMigrationSteps(snapshot migrate.Snapshot, start, end int) ([]testMigration, error) {
	result := make([]testMigration, 0, end-start)
	for _, entry := range snapshot.Manifest.Entries[start:end] {
		statements, err := source.SplitSQL(entry.File, string(snapshot.Files[entry.File]))
		if err != nil {
			return nil, err
		}
		if entry.Directives.Transaction != migrate.TransactionForbidden && hasTransactionControl(statements) {
			return nil, migrate.ErrInvalid
		}
		result = append(result, testMigration{version: entry.Version, file: entry.File, transaction: entry.Directives.Transaction, statements: statements})
	}
	return result, nil
}

// Explicit COMMIT/ROLLBACK/BEGIN/SAVEPOINT would escape or change the runner's
// per-file transaction. Require the author's explicit nontransactional mode.
// Other SQL syntax errors remain located runtime migration failures.
func hasTransactionControl(statements []source.Statement) bool {
	for _, statement := range statements {
		parsed, err := pg_query.Parse(statement.SQL)
		if err != nil {
			continue
		}
		for _, parsedStatement := range parsed.Stmts {
			if parsedStatement.Stmt.GetTransactionStmt() != nil {
				return true
			}
		}
	}
	return false
}
