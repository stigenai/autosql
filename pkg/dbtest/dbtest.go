// Package dbtest provides a database-independent migration test harness.
package dbtest

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

// Database is intentionally small so adapters can wrap sql.DB, pgx, or fakes.
type Database interface {
	Exec(context.Context, string) error
	QueryCount(context.Context, string, ...any) (int64, error)
	Close(context.Context) error
}

// Factory must return a fresh isolated database or schema for each case.
type Factory interface {
	OpenIsolated(context.Context, string) (Database, error)
}

type Command struct {
	SQL, File string
	Line      int
}
type Assertion struct {
	Name, SQL, File string
	Line, Column    int
	Args            []any
	Want            int64
}
type Version struct {
	Name       string
	Migrations []Command
	Plan       []Command
	Assertions []Assertion
}

type Case struct {
	Name                  string
	Variables             map[string]string
	Setup, Fixtures, Seed []Command
	Versions              []Version
	Assertions            []Assertion
	Teardown              []Command
	Timeout               time.Duration
}

type Result struct {
	Case       string
	Versions   []string
	Assertions int
	Duration   time.Duration
}

type Failure struct {
	Case, Stage, Name, File string
	Line, Column            int
	Err                     error
}

func (e *Failure) Error() string {
	loc := e.File
	if e.Line > 0 {
		loc = fmt.Sprintf("%s:%d", loc, e.Line)
		if e.Column > 0 {
			loc = fmt.Sprintf("%s:%d", loc, e.Column)
		}
	}
	if loc != "" {
		loc = " at " + loc
	}
	return fmt.Sprintf("dbtest %q %s %q%s: %v", e.Case, e.Stage, e.Name, loc, e.Err)
}
func (e *Failure) Unwrap() error { return e.Err }

type Runner struct {
	Factory        Factory
	CleanupTimeout time.Duration
}

type cleanupAction struct {
	name string
	file string
	line int
	run  func(context.Context) error
}

// Run executes one isolated case. Teardown and Close always run in reverse
// registration order, using a fresh bounded cleanup context after cancellation.
func (r Runner) Run(ctx context.Context, c Case) (result Result, err error) {
	started := time.Now()
	result.Case = c.Name
	if r.Factory == nil || strings.TrimSpace(c.Name) == "" {
		return result, errors.New("dbtest: factory and case name are required")
	}
	if validationErr := validateAssertions(c); validationErr != nil {
		return result, validationErr
	}
	if c.Timeout > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, c.Timeout)
		defer cancel()
	}
	db, openErr := r.Factory.OpenIsolated(ctx, c.Name)
	if openErr != nil {
		return result, &Failure{Case: c.Name, Stage: "isolate", Name: c.Name, Err: openErr}
	}
	cleanup := make([]cleanupAction, 0, len(c.Teardown)+1)
	cleanup = append(cleanup, cleanupAction{name: "close isolated database", run: db.Close})
	for _, cmd := range c.Teardown {
		cmd := cmd
		cleanup = append(cleanup, cleanupAction{name: "teardown", file: cmd.File, line: cmd.Line, run: func(cleanupCtx context.Context) error { return db.Exec(cleanupCtx, expand(cmd.SQL, c.Variables)) }})
	}
	defer func() {
		d := r.CleanupTimeout
		if d <= 0 {
			d = 10 * time.Second
		}
		for i := len(cleanup) - 1; i >= 0; i-- {
			// Every action receives its own full budget. A hung teardown cannot
			// consume the deadline needed by later teardown or Close actions.
			cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), d)
			cleanupErr := cleanup[i].run(cleanupCtx)
			cancel()
			if cleanupErr != nil {
				err = errors.Join(err, &Failure{Case: c.Name, Stage: "cleanup", Name: cleanup[i].name, File: cleanup[i].file, Line: cleanup[i].line, Err: cleanupErr})
			}
		}
		result.Duration = time.Since(started)
	}()
	for _, group := range []struct {
		name     string
		commands []Command
	}{{"setup", c.Setup}, {"fixture", c.Fixtures}, {"seed", c.Seed}} {
		if err = runCommands(ctx, db, c, group.name, group.commands); err != nil {
			return result, err
		}
	}
	for _, version := range c.Versions {
		if strings.TrimSpace(version.Name) == "" {
			return result, &Failure{Case: c.Name, Stage: "version", Err: errors.New("version name is required")}
		}
		if err = runCommands(ctx, db, c, "migration "+version.Name, version.Migrations); err != nil {
			return result, err
		}
		if err = runCommands(ctx, db, c, "plan "+version.Name, version.Plan); err != nil {
			return result, err
		}
		n, assertErr := runAssertions(ctx, db, c, "assertion "+version.Name, version.Assertions)
		result.Assertions += n
		if assertErr != nil {
			return result, assertErr
		}
		result.Versions = append(result.Versions, version.Name)
	}
	n, assertErr := runAssertions(ctx, db, c, "assertion", c.Assertions)
	result.Assertions += n
	if assertErr != nil {
		return result, assertErr
	}
	return result, nil
}

func runCommands(ctx context.Context, db Database, c Case, stage string, commands []Command) error {
	for i, cmd := range commands {
		if err := ctx.Err(); err != nil {
			return &Failure{Case: c.Name, Stage: stage, Name: fmt.Sprintf("command %d", i+1), File: cmd.File, Line: cmd.Line, Err: err}
		}
		if err := db.Exec(ctx, expand(cmd.SQL, c.Variables)); err != nil {
			return &Failure{Case: c.Name, Stage: stage, Name: fmt.Sprintf("command %d", i+1), File: cmd.File, Line: cmd.Line, Err: err}
		}
	}
	return nil
}
func runAssertions(ctx context.Context, db Database, c Case, stage string, assertions []Assertion) (int, error) {
	for i, a := range assertions {
		observed, err := db.QueryCount(ctx, expand(a.SQL, c.Variables), a.Args...)
		if err != nil {
			return i, &Failure{Case: c.Name, Stage: stage, Name: a.Name, File: a.File, Line: a.Line, Column: a.Column, Err: err}
		}
		if observed != a.Want {
			return i + 1, &Failure{Case: c.Name, Stage: stage, Name: a.Name, File: a.File, Line: a.Line, Column: a.Column, Err: fmt.Errorf("observed %d, want %d", observed, a.Want)}
		}
	}
	return len(assertions), nil
}

func validateAssertions(c Case) error {
	groups := []struct {
		stage      string
		assertions []Assertion
	}{{"assertion", c.Assertions}}
	for _, v := range c.Versions {
		groups = append(groups, struct {
			stage      string
			assertions []Assertion
		}{"assertion " + v.Name, v.Assertions})
	}
	for _, group := range groups {
		for _, a := range group.assertions {
			if strings.TrimSpace(a.Name) == "" || strings.TrimSpace(a.SQL) == "" || strings.TrimSpace(a.File) == "" || a.Line <= 0 || a.Column <= 0 {
				return &Failure{Case: c.Name, Stage: "validation", Name: a.Name, File: a.File, Line: a.Line, Column: a.Column, Err: errors.New("assertion name, SQL, source file, line, and column must be present and positive")}
			}
		}
	}
	return nil
}

// expand is deterministic and only substitutes explicitly supplied variables.
func expand(sql string, variables map[string]string) string {
	keys := make([]string, 0, len(variables))
	for k := range variables {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		sql = strings.ReplaceAll(sql, "${"+k+"}", variables[k])
	}
	return sql
}
