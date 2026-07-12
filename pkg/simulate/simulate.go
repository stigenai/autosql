// Package simulate executes migration plans in isolated development databases.
package simulate

import (
	"autosql/pkg/plan"
	"autosql/pkg/schema"
	"context"
	"errors"
	"fmt"
	"time"
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
		return result, fail("create", ErrLifecycle)
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
			err = errors.Join(err, fail("cleanup", ErrLifecycle), ce)
		}
	}()
	if e = iso.Materialize(ctx, req.From); e != nil {
		return result, fail("materialize", ErrLifecycle)
	}
	if e = iso.Execute(ctx, req.Plan); e != nil {
		return result, fail("execute", ErrLifecycle)
	}
	actual, e := iso.Inspect(ctx)
	if e != nil {
		return result, fail("inspect", ErrLifecycle)
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
	var e *Error
	if errors.As(err, &e) {
		return e
	}
	return fmt.Errorf("%w", ErrLifecycle)
}
