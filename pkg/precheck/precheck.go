// Package precheck executes data-dependent assertions before a migration mutates a database.
package precheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"strings"
	"time"
)

var (
	ErrInvalidPlan = errors.New("invalid guarded plan")
	ErrAssertion   = errors.New("pre-migration assertion failed")
)

// Plan is the immutable unit protected by assertions.
type Plan struct {
	ID           string
	Digest       string
	ChangeDigest string
	Statements   []string
	Assertions   []Assertion
}

// Assertion is a scalar count query. It deliberately cannot return row data.
type Assertion struct {
	Name         string
	Query        string
	Args         []any
	MaxAllowed   int64
	PlanDigest   string
	ChangeDigest string
	Timeout      time.Duration
}

type Result struct {
	Name       string
	Observed   int64
	MaxAllowed int64
	Passed     bool
}

type DB interface {
	Begin(context.Context) (Tx, error)
}

// Tx combines locking and database work so every mutation can be rolled back.
type Tx interface {
	AcquireLock(context.Context) error
	QueryCount(context.Context, string, ...any) (int64, error)
	Exec(context.Context, string) error
	Commit(context.Context) error
	Rollback(context.Context) error
}

type Failure struct{ Result Result }

func (e *Failure) Error() string {
	return fmt.Sprintf("%v: %s observed %d, maximum %d", ErrAssertion, e.Result.Name, e.Result.Observed, e.Result.MaxAllowed)
}
func (e *Failure) Unwrap() error { return ErrAssertion }

// Digest calculates the digest assertions bind to. Assertion results are excluded,
// avoiding a circular digest while binding the exact changes and SQL statements.
func Digest(p Plan) string {
	h := sha256.New()
	for _, value := range append([]string{p.ID, p.ChangeDigest}, p.Statements...) {
		h.Write([]byte(fmt.Sprintf("%d:", len(value))))
		h.Write([]byte(value))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// GuardedApply acquires the migration lock, runs every assertion, and only then
// executes statements. Any error rolls back the transaction.
func GuardedApply(ctx context.Context, db DB, plan Plan) (results []Result, err error) {
	if err := validate(plan); err != nil {
		return nil, err
	}
	tx, err := db.Begin(ctx)
	if err != nil {
		return nil, fmt.Errorf("begin guarded apply: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			rollbackCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 10*time.Second)
			defer cancel()
			if rollbackErr := tx.Rollback(rollbackCtx); err == nil && rollbackErr != nil {
				err = fmt.Errorf("rollback guarded apply: %w", rollbackErr)
			}
		}
	}()
	if err = tx.AcquireLock(ctx); err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	for _, check := range plan.Assertions {
		checkCtx := ctx
		cancel := func() {}
		if check.Timeout > 0 {
			checkCtx, cancel = context.WithTimeout(ctx, check.Timeout)
		}
		observed, queryErr := tx.QueryCount(checkCtx, check.Query, check.Args...)
		cancel()
		if queryErr != nil {
			return results, fmt.Errorf("precheck %q: %w", check.Name, queryErr)
		}
		result := Result{Name: check.Name, Observed: observed, MaxAllowed: check.MaxAllowed, Passed: observed <= check.MaxAllowed}
		results = append(results, result)
		if !result.Passed {
			return results, &Failure{Result: result}
		}
	}
	for i, statement := range plan.Statements {
		if err = tx.Exec(ctx, statement); err != nil {
			return results, fmt.Errorf("migration statement %d: %w", i+1, err)
		}
	}
	if err = tx.Commit(ctx); err != nil {
		return results, fmt.Errorf("commit guarded apply: %w", err)
	}
	committed = true
	return results, nil
}

func validate(p Plan) error {
	if p.ID == "" || p.ChangeDigest == "" || p.Digest == "" || p.Digest != Digest(p) {
		return fmt.Errorf("%w: plan digest mismatch", ErrInvalidPlan)
	}
	for _, a := range p.Assertions {
		if a.Name == "" || strings.TrimSpace(a.Query) == "" {
			return fmt.Errorf("%w: assertion name and query are required", ErrInvalidPlan)
		}
		if a.PlanDigest != p.Digest || a.ChangeDigest != p.ChangeDigest {
			return fmt.Errorf("%w: assertion %q digest mismatch", ErrInvalidPlan, a.Name)
		}
		if a.MaxAllowed < 0 {
			return fmt.Errorf("%w: assertion %q has negative maximum", ErrInvalidPlan, a.Name)
		}
		if !safeQuery(a.Query) {
			return fmt.Errorf("%w: assertion %q is not a read-only scalar query", ErrInvalidPlan, a.Name)
		}
	}
	return nil
}

func safeQuery(query string) bool {
	q := strings.ToLower(strings.TrimSpace(query))
	fields := strings.Fields(q)
	if len(fields) == 0 || fields[0] != "select" || strings.Contains(q, ";") {
		return false
	}
	for _, keyword := range []string{" insert ", " update ", " delete ", " merge ", " copy ", " alter ", " drop ", " create ", " truncate ", " grant ", " revoke ", " call "} {
		if strings.Contains(" "+q+" ", keyword) {
			return false
		}
	}
	return true
}
