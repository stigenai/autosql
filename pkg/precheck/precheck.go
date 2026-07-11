// Package precheck executes data-dependent assertions before a migration mutates a database.
package precheck

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"
)

var (
	ErrInvalidPlan = errors.New("invalid guarded plan")
	ErrAssertion   = errors.New("pre-migration assertion failed")
)

type Source struct {
	File         string
	Line, Column int
}

// ValidationError retains the declaration location without exposing query results.
type ValidationError struct {
	Source             Source
	Assertion, Message string
}

func (e *ValidationError) Error() string {
	location := e.Source.File
	if e.Source.Line > 0 {
		location = fmt.Sprintf("%s:%d", location, e.Source.Line)
		if e.Source.Column > 0 {
			location = fmt.Sprintf("%s:%d", location, e.Source.Column)
		}
	}
	if location != "" {
		location = " at " + location
	}
	return fmt.Sprintf("%v: assertion %q%s: %s", ErrInvalidPlan, e.Assertion, location, e.Message)
}
func (e *ValidationError) Unwrap() error { return ErrInvalidPlan }

type Plan struct {
	ID, Digest, ChangeDigest string
	Statements               []string
	Assertions               []Assertion
}

// Assertion is restricted to a parsed SELECT count(*) query. The canonical
// query, arguments, threshold, timeout, name, and change identity are all signed.
type Assertion struct {
	Name, Query              string
	Args                     []any
	MaxAllowed               int64
	PlanDigest, ChangeDigest string
	Timeout                  time.Duration
	Source                   Source
}

type Result struct {
	Name                 string
	Observed, MaxAllowed int64
	Passed               bool
	Source               Source
}
type DB interface {
	Begin(context.Context) (Tx, error)
}
type Tx interface {
	AcquireLock(context.Context) error
	QueryCount(context.Context, string, ...any) (int64, error)
	Exec(context.Context, string) error
	Commit(context.Context) error
	Rollback(context.Context) error
}

type Failure struct{ Result Result }

func (e *Failure) Error() string {
	location := e.Result.Source.File
	if e.Result.Source.Line > 0 {
		location = fmt.Sprintf("%s:%d", location, e.Result.Source.Line)
		if e.Result.Source.Column > 0 {
			location = fmt.Sprintf("%s:%d", location, e.Result.Source.Column)
		}
	}
	if location != "" {
		location = " at " + location
	}
	return fmt.Sprintf("%v: %s%s observed %d, maximum %d", ErrAssertion, e.Result.Name, location, e.Result.Observed, e.Result.MaxAllowed)
}
func (e *Failure) Unwrap() error { return ErrAssertion }

// CheckError identifies a declaration whose live count query failed.
type CheckError struct {
	Source    Source
	Assertion string
	Err       error
}

func (e *CheckError) Error() string {
	return fmt.Sprintf("precheck %q at %s:%d:%d: %v", e.Assertion, e.Source.File, e.Source.Line, e.Source.Column, e.Err)
}
func (e *CheckError) Unwrap() error { return e.Err }

type digestAssertion struct {
	Name, Query  string
	Args         []digestArg
	MaxAllowed   int64
	Timeout      int64
	ChangeDigest string
	Source       Source
}
type digestArg struct {
	Type  string
	Value json.RawMessage
}
type digestPlan struct {
	ID, ChangeDigest string
	Statements       []string
	Assertions       []digestAssertion
}

// Digest signs every field that can alter assertion or mutation semantics.
func Digest(p Plan) (string, error) {
	dp := digestPlan{ID: p.ID, ChangeDigest: p.ChangeDigest, Statements: append([]string(nil), p.Statements...)}
	for _, a := range p.Assertions {
		query, _, err := canonicalCountQuery(a.Query, len(a.Args))
		if err != nil {
			return "", validation(a, err.Error())
		}
		args, err := canonicalArgs(a.Args)
		if err != nil {
			return "", validation(a, "arguments are not canonically encodable: "+err.Error())
		}
		dp.Assertions = append(dp.Assertions, digestAssertion{Name: a.Name, Query: query, Args: args, MaxAllowed: a.MaxAllowed, Timeout: int64(a.Timeout), ChangeDigest: a.ChangeDigest, Source: a.Source})
	}
	b, err := json.Marshal(dp)
	if err != nil {
		return "", fmt.Errorf("%w: canonical digest: %v", ErrInvalidPlan, err)
	}
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:]), nil
}

func canonicalArgs(args []any) ([]digestArg, error) {
	out := make([]digestArg, 0, len(args))
	for _, arg := range args {
		typeName := fmt.Sprintf("%T", arg)
		switch arg.(type) {
		case nil, string, bool, int, int8, int16, int32, int64, uint, uint8, uint16, uint32, uint64, float32, float64, []byte, time.Time, json.Number:
		default:
			return nil, fmt.Errorf("unsupported argument type %T", arg)
		}
		value, err := json.Marshal(arg)
		if err != nil {
			return nil, fmt.Errorf("%T: %w", arg, err)
		}
		out = append(out, digestArg{Type: typeName, Value: value})
	}
	return out, nil
}

// GuardedApply proves the sequence transaction -> lock -> all checks -> mutations.
// Any failure before commit rolls back, so a failed assertion cannot mutate.
func GuardedApply(ctx context.Context, db DB, plan Plan) (results []Result, err error) {
	canonical, err := validate(plan)
	if err != nil {
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
			rollbackErr := tx.Rollback(rollbackCtx)
			if rollbackErr != nil {
				err = errors.Join(err, fmt.Errorf("rollback guarded apply: %w", rollbackErr))
			}
		}
	}()
	if err = tx.AcquireLock(ctx); err != nil {
		return nil, fmt.Errorf("acquire migration lock: %w", err)
	}
	for i, check := range plan.Assertions {
		checkCtx := ctx
		cancel := func() {}
		if check.Timeout > 0 {
			checkCtx, cancel = context.WithTimeout(ctx, check.Timeout)
		}
		observed, queryErr := tx.QueryCount(checkCtx, canonical[i], check.Args...)
		cancel()
		if queryErr != nil {
			return results, &CheckError{Source: check.Source, Assertion: check.Name, Err: queryErr}
		}
		result := Result{Name: check.Name, Observed: observed, MaxAllowed: check.MaxAllowed, Passed: observed <= check.MaxAllowed, Source: check.Source}
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

func validate(p Plan) ([]string, error) {
	if strings.TrimSpace(p.ID) == "" || strings.TrimSpace(p.ChangeDigest) == "" || strings.TrimSpace(p.Digest) == "" {
		return nil, fmt.Errorf("%w: plan identity is incomplete", ErrInvalidPlan)
	}
	canonical := make([]string, len(p.Assertions))
	for i, a := range p.Assertions {
		if strings.TrimSpace(a.Source.File) == "" || a.Source.Line <= 0 || a.Source.Column <= 0 {
			return nil, validation(a, "source file, line, and column must be present and positive")
		}
		if strings.TrimSpace(a.Name) == "" {
			return nil, validation(a, "name is required")
		}
		if strings.TrimSpace(a.Query) == "" {
			return nil, validation(a, "SQL is required")
		}
		if a.ChangeDigest != p.ChangeDigest {
			return nil, validation(a, "change digest mismatch")
		}
		if a.MaxAllowed < 0 {
			return nil, validation(a, "maximum must be non-negative")
		}
		if a.Timeout < 0 {
			return nil, validation(a, "timeout must be non-negative")
		}
		q, _, err := canonicalCountQuery(a.Query, len(a.Args))
		if err != nil {
			return nil, validation(a, err.Error())
		}
		canonical[i] = q
	}
	want, err := Digest(p)
	if err != nil {
		return nil, err
	}
	if p.Digest != want {
		return nil, fmt.Errorf("%w: plan digest mismatch", ErrInvalidPlan)
	}
	for _, a := range p.Assertions {
		if a.PlanDigest != p.Digest {
			return nil, validation(a, "plan digest mismatch")
		}
	}
	return canonical, nil
}

func validation(a Assertion, message string) error {
	return &ValidationError{Source: a.Source, Assertion: a.Name, Message: message}
}

var countQuery = regexp.MustCompile(`(?i)^select\s+count\s*\(\s*\*\s*\)\s+from\s+([a-z_][a-z0-9_]*)(?:\.([a-z_][a-z0-9_]*))?(?:\s+where\s+(.+))?$`)
var conjunction = regexp.MustCompile(`(?i)\s+and\s+`)
var comparison = regexp.MustCompile(`(?i)^([a-z_][a-z0-9_]*(?:\.[a-z_][a-z0-9_]*)?)\s*(=|!=|<>|<=|>=|<|>)\s*\$([1-9][0-9]*)$`)
var nullCheck = regexp.MustCompile(`(?i)^([a-z_][a-z0-9_]*(?:\.[a-z_][a-z0-9_]*)?)\s+is\s+(not\s+)?null$`)

// canonicalCountQuery is a strict parser, not a keyword blacklist. It accepts
// only count(*), identifiers, placeholder comparisons, NULL checks, and AND.
func canonicalCountQuery(sql string, argCount int) (string, []int, error) {
	trimmed := strings.TrimSpace(sql)
	if strings.ContainsAny(trimmed, ";\x00") || strings.Contains(trimmed, "--") || strings.Contains(trimmed, "/*") {
		return "", nil, errors.New("SQL must be one comment-free count query")
	}
	m := countQuery.FindStringSubmatch(trimmed)
	if m == nil {
		return "", nil, errors.New("SQL must be SELECT count(*) FROM <table> with optional safe WHERE predicates")
	}
	table := strings.ToLower(m[1])
	if m[2] != "" {
		table += "." + strings.ToLower(m[2])
	}
	canonical := "SELECT count(*) FROM " + table
	seen := map[int]bool{}
	if m[3] != "" {
		parts := conjunction.Split(strings.TrimSpace(m[3]), -1)
		normalized := make([]string, 0, len(parts))
		for _, part := range parts {
			if cm := comparison.FindStringSubmatch(strings.TrimSpace(part)); cm != nil {
				n, _ := strconv.Atoi(cm[3])
				seen[n] = true
				normalized = append(normalized, strings.ToLower(cm[1])+" "+cm[2]+" $"+cm[3])
				continue
			}
			if nm := nullCheck.FindStringSubmatch(strings.TrimSpace(part)); nm != nil {
				not := ""
				if nm[2] != "" {
					not = "NOT "
				}
				normalized = append(normalized, strings.ToLower(nm[1])+" IS "+not+"NULL")
				continue
			}
			return "", nil, fmt.Errorf("unsafe WHERE predicate %q", part)
		}
		canonical += " WHERE " + strings.Join(normalized, " AND ")
	}
	placeholders := make([]int, 0, len(seen))
	for n := range seen {
		placeholders = append(placeholders, n)
	}
	sort.Ints(placeholders)
	if len(placeholders) != argCount {
		return "", nil, fmt.Errorf("placeholder count %d does not match argument count %d", len(placeholders), argCount)
	}
	for i, n := range placeholders {
		if n != i+1 {
			return "", nil, errors.New("placeholders must be contiguous starting at $1")
		}
	}
	return canonical, placeholders, nil
}
