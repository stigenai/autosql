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
	Data                                       any
	Kind                                       string
	Message                                    string
	Code                                       ExitCode
	Cause                                      error
	Status                                     string
	PendingStep, ExecutionID, RecoveryGuidance string
	AppliedSteps                               int
}

func (e *Error) Error() string { return e.Message }
func (e *Error) Unwrap() error { return e.Cause }

func classify(err error, fallback *Error) *Error {
	if errors.Is(err, context.DeadlineExceeded) {
		return contextualError(err, "timeout", "operation timed out", ExitTimeout)
	}
	if errors.Is(err, context.Canceled) {
		return contextualError(err, "canceled", "operation canceled", ExitCanceled)
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

// Preserve structured outcomes and recovery paths when cancellation determines
// the exit status after partial execution and cleanup.
func contextualError(err error, kind, message string, code ExitCode) *Error {
	result := Error{}
	var existing *Error
	if errors.As(err, &existing) {
		result = *existing
	}
	result.Kind, result.Message, result.Code, result.Cause = kind, message, code, err
	return &result
}
