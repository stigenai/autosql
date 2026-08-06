// Package simulate executes migration plans in isolated development databases.
package simulate

import (
	"autosql/pkg/plan"
	"autosql/pkg/schema"
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

var ErrConfig = errors.New("invalid simulation configuration")
var ErrLifecycle = errors.New("simulation lifecycle failed")
var ErrFingerprint = errors.New("simulation fingerprint mismatch")

type Error struct {
	Code string
	kind error
}

func (e *Error) Error() string           { return "simulation " + e.Code }
func (e *Error) Unwrap() error           { return e.kind }
func fail(code string, kind error) error { return &Error{Code: code, kind: kind} }

// PostgresError retains the actionable, non-secret portion of a PostgreSQL
// failure. Query text, connection strings, detail, hints, and context are
// deliberately omitted at the simulation boundary.
type PostgresError struct {
	Code    string
	Message string
}

func (e *PostgresError) Error() string {
	return fmt.Sprintf("%s (SQLSTATE %s)", e.Message, e.Code)
}

func (e *PostgresError) SQLState() string { return e.Code }

// RedactedCause returns only causes that are safe and useful to surface.
func RedactedCause(err error) error {
	if err == nil {
		return nil
	}
	var safe *PostgresError
	if errors.As(err, &safe) {
		return safe
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		message := strings.Map(func(r rune) rune {
			if r < ' ' || r == 0x7f {
				return -1
			}
			return r
		}, strings.TrimSpace(pgErr.Message))
		if len(message) > 256 {
			message = message[:256]
		}
		lower := strings.ToLower(message)
		if message == "" || strings.Contains(message, "://") || strings.Contains(message, "@") || strings.Contains(lower, "password") || strings.Contains(lower, "secret") {
			message = "PostgreSQL operation failed"
		}
		return &PostgresError{Code: pgErr.Code, Message: message}
	}
	if errors.Is(err, context.Canceled) {
		return context.Canceled
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return context.DeadlineExceeded
	}
	return nil
}

func lifecycleFailure(code string, cause error) error {
	primary := fail(code, ErrLifecycle)
	if safe := RedactedCause(cause); safe != nil {
		return errors.Join(primary, safe)
	}
	return primary
}

type Config struct {
	DevelopmentURL, DevelopmentIdentity, ProductionIdentity string
	CleanupTimeout                                          time.Duration
	AllowedHosts                                            []string
}
type Factory interface {
	Create(context.Context, Config) (Isolation, error)
}
type Isolation interface {
	Identity() string
	Materialize(context.Context, schema.Document) error
	Execute(context.Context, plan.Plan) error
	Inspect(context.Context) (schema.Document, error)
	Cleanup(context.Context) error
}
type Request struct {
	Config Config
	From   schema.Document
	Plan   plan.Plan
}
type Result struct {
	IsolationIdentity, FromFingerprint, ToFingerprint string
	Verified                                          bool
}

func Run(ctx context.Context, f Factory, req Request) (result Result, err error) {
	if f == nil || req.Config.DevelopmentURL == "" || req.Config.ProductionIdentity == "" {
		return result, fail("config", ErrConfig)
	}
	if e := req.Plan.Validate(); e != nil {
		return result, fail("plan", ErrConfig)
	}
	from, e := schema.SemanticFingerprint(req.From)
	if e != nil || from != req.Plan.FromFingerprint {
		return result, fail("source", ErrFingerprint)
	}
	iso, e := f.Create(ctx, req.Config)
	if e != nil {
		return result, lifecycleFailure("create", e)
	}
	result.IsolationIdentity = iso.Identity()
	timeout := req.Config.CleanupTimeout
	if timeout <= 0 {
		timeout = 15 * time.Second
	}
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), timeout)
		defer cancel()
		if ce := iso.Cleanup(cleanupCtx); ce != nil {
			err = errors.Join(err, lifecycleFailure("cleanup", ce))
		}
	}()
	if e = iso.Materialize(ctx, req.From); e != nil {
		return result, lifecycleFailure("materialize", e)
	}
	if e = iso.Execute(ctx, req.Plan); e != nil {
		return result, lifecycleFailure("execute", e)
	}
	actual, e := iso.Inspect(ctx)
	if e != nil {
		return result, lifecycleFailure("inspect", e)
	}
	got, e := schema.SemanticFingerprint(actual)
	if e != nil || got != req.Plan.ToFingerprint {
		return result, fail("target", ErrFingerprint)
	}
	result.FromFingerprint = from
	result.ToFingerprint = got
	result.Verified = true
	return result, nil
}
func Redacted(err error) error {
	if err == nil {
		return nil
	}
	var out []error
	var simulationError *Error
	if errors.As(err, &simulationError) {
		out = append(out, simulationError)
	}
	if cause := RedactedCause(err); cause != nil {
		out = append(out, cause)
	}
	if len(out) != 0 {
		return errors.Join(out...)
	}
	return fmt.Errorf("%w", ErrLifecycle)
}
