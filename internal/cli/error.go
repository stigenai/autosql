package cli

import (
	"context"
	"errors"
)

type ExitCode int

const (
	ExitOK         ExitCode = 0
	ExitInternal   ExitCode = 1
	ExitUsage      ExitCode = 2
	ExitConfig     ExitCode = 3
	ExitSecret     ExitCode = 4
	ExitValidation ExitCode = 5
	ExitConnection ExitCode = 6
	ExitMigration  ExitCode = 7
	ExitConflict   ExitCode = 8
	ExitTimeout    ExitCode = 124
	ExitCanceled   ExitCode = 130
)

type Error struct {
	Kind                                       string
	Message                                    string
	Code                                       ExitCode
	Cause                                      error
	Status                                     string
	PendingStep, ExecutionID, RecoveryGuidance string
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Cause }

func classify(err error, fallback *Error) *Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return &Error{Kind: "timeout", Message: "operation timed out", Code: ExitTimeout, Cause: err}
	}
	if errors.Is(err, context.Canceled) {
		return &Error{Kind: "canceled", Message: "operation canceled", Code: ExitCanceled, Cause: err}
	}
	var ce *Error
	if errors.As(err, &ce) {
		return ce
	}
	if fallback != nil {
		fallback.Cause = err
		if fallback.Message == "" {
			fallback.Message = err.Error()
		}
		return fallback
	}
	return &Error{Kind: "internal", Message: "internal error", Code: ExitInternal, Cause: err}
}
